// Package tds implementa um parser mínimo para o protocolo de rede TDS (Tabular Data Stream)
// usado pelo Microsoft SQL Server.
//
// Referência: https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-tds/
//
// O proxy só precisa parsear headers de pacote e um pequeno subconjunto do
// conteúdo dos pacotes (Pre-Login, Login7) para roteamento e detecção de pinning.
// Todos os outros dados são encaminhados como bytes opacos.
package tds

import (
	"encoding/binary"
	"fmt"
	"io"
)

// ── Tipos de Pacote TDS (MS-TDS 2.2.3.1) ─────────────────────────────────

// PacketType é o primeiro byte de um header de pacote TDS.
type PacketType byte

const (
	PacketSQLBatch   PacketType = 0x01 // SQL Batch
	PacketRPCRequest PacketType = 0x03 // Requisição RPC
	PacketReply      PacketType = 0x04 // Resultado tabular (servidor → cliente)
	PacketAttention  PacketType = 0x06 // Attention (cancelamento)
	PacketBulkLoad   PacketType = 0x07 // Carga em massa
	PacketTransMgr   PacketType = 0x0E // Requisição do Transaction Manager
	PacketPreLogin   PacketType = 0x12 // Pre-Login
	PacketLogin7     PacketType = 0x10 // Login7
	PacketSSPI       PacketType = 0x11 // Autenticação SSPI
	PacketPreLoginR  PacketType = 0x04 // Resposta Pre-Login (mesmo que Reply)
)

// String retorna um nome legível para o tipo de pacote.
func (t PacketType) String() string {
	switch t {
	case PacketSQLBatch:
		return "SQL_BATCH"
	case PacketRPCRequest:
		return "RPC"
	case PacketReply:
		return "REPLY"
	case PacketAttention:
		return "ATTENTION"
	case PacketBulkLoad:
		return "BULK_LOAD"
	case PacketTransMgr:
		return "TRANS_MGR"
	case PacketPreLogin:
		return "PRELOGIN"
	case PacketLogin7:
		return "LOGIN7"
	case PacketSSPI:
		return "SSPI"
	default:
		return fmt.Sprintf("UNKNOWN(0x%02X)", byte(t))
	}
}

// ── Status de Pacote TDS (MS-TDS 2.2.3.1.2) ─────────────────────────────

// Flags de PacketStatus.
const (
	StatusNormal         byte = 0x00
	StatusEOM            byte = 0x01 // Fim da mensagem
	StatusIgnore         byte = 0x02
	StatusResetConn      byte = 0x08 // Reset de conexão (sp_reset_connection)
	StatusResetConnSkip  byte = 0x10 // Reset com skip tran
)

// ── Header TDS (8 bytes) ──────────────────────────────────────────────

// HeaderSize é o tamanho fixo de um header de pacote TDS.
const HeaderSize = 8

// MaxPacketSize é o tamanho máximo de um pacote TDS (32 KB por padrão da spec,
// mas pode ser negociado até 32767 bytes).
const MaxPacketSize = 32768

// Header representa o header de 8 bytes de um pacote TDS.
//
//	Byte 0:   Type
//	Byte 1:   Status
//	Byte 2-3: Length (incluindo header, big-endian)
//	Byte 4-5: SPID (ID do processo no servidor, big-endian)
//	Byte 6:   PacketID (contador sequencial)
//	Byte 7:   Window (não utilizado, sempre 0)
type Header struct {
	Type     PacketType
	Status   byte
	Length   uint16 // Tamanho total do pacote incluindo header
	SPID     uint16
	PacketID byte
	Window   byte
}

// IsEOM retorna true se este for o último pacote de uma mensagem.
func (h *Header) IsEOM() bool {
	return h.Status&StatusEOM != 0
}

// PayloadLength retorna o número de bytes de payload (Length - HeaderSize).
func (h *Header) PayloadLength() int {
	if int(h.Length) <= HeaderSize {
		return 0
	}
	return int(h.Length) - HeaderSize
}

// Marshal serializa o header em um slice de 8 bytes.
func (h *Header) Marshal() []byte {
	buf := make([]byte, HeaderSize)
	buf[0] = byte(h.Type)
	buf[1] = h.Status
	binary.BigEndian.PutUint16(buf[2:4], h.Length)
	binary.BigEndian.PutUint16(buf[4:6], h.SPID)
	buf[6] = h.PacketID
	buf[7] = h.Window
	return buf
}

// ReadHeader lê um header TDS de 8 bytes do reader.
func ReadHeader(r io.Reader) (*Header, error) {
	buf := make([]byte, HeaderSize)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return ParseHeader(buf)
}

// ParseHeader parseia um buffer de 8 bytes em um Header.
func ParseHeader(buf []byte) (*Header, error) {
	if len(buf) < HeaderSize {
		return nil, fmt.Errorf("tds header too short: %d bytes", len(buf))
	}
	h := &Header{
		Type:     PacketType(buf[0]),
		Status:   buf[1],
		Length:   binary.BigEndian.Uint16(buf[2:4]),
		SPID:     binary.BigEndian.Uint16(buf[4:6]),
		PacketID: buf[6],
		Window:   buf[7],
	}
	if h.Length < HeaderSize {
		return nil, fmt.Errorf("tds packet length %d is less than header size", h.Length)
	}
	if h.Length > MaxPacketSize {
		return nil, fmt.Errorf("tds packet length %d exceeds max %d", h.Length, MaxPacketSize)
	}
	return h, nil
}

// ReadPacket lê um pacote TDS completo (header + payload) do reader.
// Retorna o header e os bytes completos do pacote (incluindo header).
func ReadPacket(r io.Reader) (*Header, []byte, error) {
	hdr, err := ReadHeader(r)
	if err != nil {
		return nil, nil, err
	}

	packet := make([]byte, hdr.Length)
	copy(packet[:HeaderSize], hdr.Marshal())

	payloadLen := hdr.PayloadLength()
	if payloadLen > 0 {
		if _, err := io.ReadFull(r, packet[HeaderSize:]); err != nil {
			return nil, nil, fmt.Errorf("reading tds payload (%d bytes): %w", payloadLen, err)
		}
	}

	return hdr, packet, nil
}

// ReadMessage lê uma mensagem TDS completa (um ou mais pacotes até EOM).
// Retorna o tipo de pacote, payload montado (sem headers), e todos os pacotes brutos.
func ReadMessage(r io.Reader) (PacketType, []byte, [][]byte, error) {
	var (
		pktType  PacketType
		payload  []byte
		packets  [][]byte
	)

	for {
		hdr, pkt, err := ReadPacket(r)
		if err != nil {
			return 0, nil, nil, err
		}

		if pktType == 0 {
			pktType = hdr.Type
		}

		packets = append(packets, pkt)
		if hdr.PayloadLength() > 0 {
			payload = append(payload, pkt[HeaderSize:]...)
		}

		if hdr.IsEOM() {
			break
		}
	}

	return pktType, payload, packets, nil
}

// WritePackets escreve bytes de pacotes brutos em um writer.
func WritePackets(w io.Writer, packets [][]byte) error {
	for _, pkt := range packets {
		if _, err := w.Write(pkt); err != nil {
			return err
		}
	}
	return nil
}

// BuildPackets divide um payload em um ou mais pacotes TDS com headers adequados.
// packetSize é o tamanho máximo de cada pacote (incluindo header).
func BuildPackets(pktType PacketType, payload []byte, packetSize int) [][]byte {
	if packetSize <= HeaderSize {
		packetSize = 4096
	}

	maxPayload := packetSize - HeaderSize
	var packets [][]byte
	var packetID byte

	for len(payload) > 0 {
		chunkSize := maxPayload
		if chunkSize > len(payload) {
			chunkSize = len(payload)
		}

		status := StatusNormal
		if chunkSize >= len(payload) {
			status = StatusEOM
		}

		hdr := Header{
			Type:     pktType,
			Status:   byte(status),
			Length:   uint16(HeaderSize + chunkSize),
			PacketID: packetID,
		}

		pkt := make([]byte, HeaderSize+chunkSize)
		copy(pkt[:HeaderSize], hdr.Marshal())
		copy(pkt[HeaderSize:], payload[:chunkSize])

		packets = append(packets, pkt)
		payload = payload[chunkSize:]
		packetID++
	}

	// If payload was empty, produce one empty EOM packet.
	if len(packets) == 0 {
		hdr := Header{
			Type:   pktType,
			Status: StatusEOM,
			Length: HeaderSize,
		}
		packets = append(packets, hdr.Marshal())
	}

	return packets
}
