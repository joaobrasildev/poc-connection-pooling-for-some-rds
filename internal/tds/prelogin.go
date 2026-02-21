package tds

import (
	"encoding/binary"
	"fmt"
	"io"
)

// ── Tipos de Token Pre-Login (MS-TDS 2.2.6.5) ──────────────────────────

// PreLoginOptionToken identifica campos em um pacote Pre-Login.
type PreLoginOptionToken byte

const (
	PreLoginVersion    PreLoginOptionToken = 0x00
	PreLoginEncryption PreLoginOptionToken = 0x01
	PreLoginInstOpt    PreLoginOptionToken = 0x02
	PreLoginThreadID   PreLoginOptionToken = 0x03
	PreLoginMARS       PreLoginOptionToken = 0x04
	PreLoginTraceID    PreLoginOptionToken = 0x05
	PreLoginFedAuth    PreLoginOptionToken = 0x06
	PreLoginNonce      PreLoginOptionToken = 0x07
	PreLoginTerminator PreLoginOptionToken = 0xFF
)

// Opções de criptografia.
const (
	EncryptOff    byte = 0x00
	EncryptOn     byte = 0x01
	EncryptNotSup byte = 0x02
	EncryptReq    byte = 0x03
)

// PreLoginOption representa uma única opção no pacote Pre-Login.
type PreLoginOption struct {
	Token  PreLoginOptionToken
	Data   []byte
}

// PreLoginMsg contém as opções Pre-Login parseadas.
type PreLoginMsg struct {
	Options []PreLoginOption
}

// ParsePreLogin faz o parse do payload de um pacote Pre-Login (sem o header TDS).
func ParsePreLogin(payload []byte) (*PreLoginMsg, error) {
	if len(payload) < 1 {
		return nil, fmt.Errorf("prelogin payload is empty")
	}

	msg := &PreLoginMsg{}

	// Fase 1: parsear headers de opções (token, offset, length).
	type optHeader struct {
		token  PreLoginOptionToken
		offset uint16
		length uint16
	}
	var headers []optHeader

	pos := 0
	for pos < len(payload) {
		token := PreLoginOptionToken(payload[pos])
		if token == PreLoginTerminator {
			pos++
			break
		}
		if pos+5 > len(payload) {
			return nil, fmt.Errorf("prelogin: truncated option header at pos %d", pos)
		}
		offset := binary.BigEndian.Uint16(payload[pos+1 : pos+3])
		length := binary.BigEndian.Uint16(payload[pos+3 : pos+5])
		headers = append(headers, optHeader{token, offset, length})
		pos += 5
	}

	// Fase 2: extrair dados das opções.
	for _, h := range headers {
		end := int(h.offset) + int(h.length)
		if end > len(payload) {
			return nil, fmt.Errorf("prelogin: option 0x%02X data out of bounds (offset=%d, len=%d, payload=%d)",
				h.token, h.offset, h.length, len(payload))
		}
		data := make([]byte, h.length)
		copy(data, payload[h.offset:end])
		msg.Options = append(msg.Options, PreLoginOption{Token: h.token, Data: data})
	}

	return msg, nil
}

// Encryption retorna a flag de criptografia das opções Pre-Login.
func (m *PreLoginMsg) Encryption() byte {
	for _, opt := range m.Options {
		if opt.Token == PreLoginEncryption && len(opt.Data) > 0 {
			return opt.Data[0]
		}
	}
	return EncryptNotSup
}

// SetEncryption atualiza a opção de criptografia na mensagem Pre-Login.
func (m *PreLoginMsg) SetEncryption(enc byte) {
	for i, opt := range m.Options {
		if opt.Token == PreLoginEncryption {
			if len(m.Options[i].Data) > 0 {
				m.Options[i].Data[0] = enc
			}
			return
		}
	}
}

// Marshal serializa a mensagem Pre-Login de volta para bytes.
func (m *PreLoginMsg) Marshal() []byte {
	// Calcular tamanho do header: 5 bytes por opção + 1 byte terminador.
	headerSize := len(m.Options)*5 + 1

	// Calcular tamanho total.
	totalSize := headerSize
	for _, opt := range m.Options {
		totalSize += len(opt.Data)
	}

	buf := make([]byte, totalSize)

	// Escrever headers das opções.
	dataOffset := headerSize
	pos := 0
	for _, opt := range m.Options {
		buf[pos] = byte(opt.Token)
		binary.BigEndian.PutUint16(buf[pos+1:pos+3], uint16(dataOffset))
		binary.BigEndian.PutUint16(buf[pos+3:pos+5], uint16(len(opt.Data)))
		copy(buf[dataOffset:], opt.Data)
		dataOffset += len(opt.Data)
		pos += 5
	}
	buf[pos] = byte(PreLoginTerminator)

	return buf
}

// BuildPreLoginResponse cria um payload mínimo de resposta Pre-Login.
// O proxy responde com a mesma versão e ENCRYPT_NOT_SUP para a POC.
func BuildPreLoginResponse(clientPreLogin *PreLoginMsg) []byte {
	resp := &PreLoginMsg{}

	// Copiar versão do cliente ou usar um valor padrão.
	var versionData []byte
	for _, opt := range clientPreLogin.Options {
		if opt.Token == PreLoginVersion {
			versionData = make([]byte, len(opt.Data))
			copy(versionData, opt.Data)
			break
		}
	}
	if versionData == nil {
		// Versão do SQL Server 2022: 16.0.4236
		versionData = []byte{0x10, 0x00, 0x10, 0x8C, 0x00, 0x00}
	}
	resp.Options = append(resp.Options, PreLoginOption{Token: PreLoginVersion, Data: versionData})

	// Responder com criptografia desativada para a POC.
	resp.Options = append(resp.Options, PreLoginOption{Token: PreLoginEncryption, Data: []byte{EncryptNotSup}})

	// MARS desativado.
	resp.Options = append(resp.Options, PreLoginOption{Token: PreLoginMARS, Data: []byte{0x00}})

	return resp.Marshal()
}

// ForwardPreLogin lê uma mensagem Pre-Login do cliente, encaminha ao
// backend, lê a resposta do backend e a envia de volta ao cliente.
// Retorna o PreLogin do cliente parseado para inspeção.
func ForwardPreLogin(client io.ReadWriter, backend io.ReadWriter) (*PreLoginMsg, error) {
	// Ler Pre-Login do cliente.
	pktType, payload, _, err := ReadMessage(client)
	if err != nil {
		return nil, fmt.Errorf("reading client prelogin: %w", err)
	}
	if pktType != PacketPreLogin {
		return nil, fmt.Errorf("expected PRELOGIN (0x12), got 0x%02X", pktType)
	}

	clientPL, err := ParsePreLogin(payload)
	if err != nil {
		return nil, fmt.Errorf("parsing client prelogin: %w", err)
	}

	// Encaminhar Pre-Login ao backend.
	fwdPackets := BuildPackets(PacketPreLogin, payload, 4096)
	if err := WritePackets(backend, fwdPackets); err != nil {
		return nil, fmt.Errorf("forwarding prelogin to backend: %w", err)
	}

	// Ler resposta Pre-Login do backend.
	respType, _, respPackets, err := ReadMessage(backend)
	if err != nil {
		return nil, fmt.Errorf("reading backend prelogin response: %w", err)
	}
	_ = respType // Resposta Pre-Login é tipo 0x04 (Reply)

	// Encaminhar resposta do backend ao cliente.
	if err := WritePackets(client, respPackets); err != nil {
		return nil, fmt.Errorf("forwarding prelogin response to client: %w", err)
	}

	return clientPL, nil
}
