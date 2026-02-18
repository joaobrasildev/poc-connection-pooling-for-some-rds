// Package tds implements a minimal parser for the TDS (Tabular Data Stream)
// wire protocol used by Microsoft SQL Server.
//
// Reference: https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-tds/
//
// The proxy only needs to parse packet headers and a small subset of packet
// contents (Pre-Login, Login7) for routing and pinning detection. All other
// data is forwarded as opaque bytes.
package tds

import (
	"encoding/binary"
	"fmt"
	"io"
)

// ── TDS Packet Types (MS-TDS 2.2.3.1) ─────────────────────────────────

// PacketType is the first byte of a TDS packet header.
type PacketType byte

const (
	PacketSQLBatch   PacketType = 0x01 // SQL Batch
	PacketRPCRequest PacketType = 0x03 // RPC Request
	PacketReply      PacketType = 0x04 // Tabular Result (server → client)
	PacketAttention  PacketType = 0x06 // Attention (cancel)
	PacketBulkLoad   PacketType = 0x07 // Bulk Load
	PacketTransMgr   PacketType = 0x0E // Transaction Manager Request
	PacketPreLogin   PacketType = 0x12 // Pre-Login
	PacketLogin7     PacketType = 0x10 // Login7
	PacketSSPI       PacketType = 0x11 // SSPI auth
	PacketPreLoginR  PacketType = 0x04 // Pre-Login Response (same as Reply)
)

// String returns a human-readable name for the packet type.
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

// ── TDS Packet Status (MS-TDS 2.2.3.1.2) ─────────────────────────────

// PacketStatus flags.
const (
	StatusNormal         byte = 0x00
	StatusEOM            byte = 0x01 // End of message
	StatusIgnore         byte = 0x02
	StatusResetConn      byte = 0x08 // Reset connection (sp_reset_connection)
	StatusResetConnSkip  byte = 0x10 // Reset with skip tran
)

// ── TDS Header (8 bytes) ──────────────────────────────────────────────

// HeaderSize is the fixed size of a TDS packet header.
const HeaderSize = 8

// MaxPacketSize is the maximum TDS packet size (32 KB per spec default,
// but can be negotiated up to 32767 bytes).
const MaxPacketSize = 32768

// Header represents the 8-byte TDS packet header.
//
//	Byte 0:   Type
//	Byte 1:   Status
//	Byte 2-3: Length (including header, big-endian)
//	Byte 4-5: SPID (server process ID, big-endian)
//	Byte 6:   PacketID (sequential counter)
//	Byte 7:   Window (unused, always 0)
type Header struct {
	Type     PacketType
	Status   byte
	Length   uint16 // Total packet length including header
	SPID     uint16
	PacketID byte
	Window   byte
}

// IsEOM returns true if this is the last packet in a message.
func (h *Header) IsEOM() bool {
	return h.Status&StatusEOM != 0
}

// PayloadLength returns the number of payload bytes (Length - HeaderSize).
func (h *Header) PayloadLength() int {
	if int(h.Length) <= HeaderSize {
		return 0
	}
	return int(h.Length) - HeaderSize
}

// Marshal serializes the header to an 8-byte slice.
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

// ReadHeader reads an 8-byte TDS header from the reader.
func ReadHeader(r io.Reader) (*Header, error) {
	buf := make([]byte, HeaderSize)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return ParseHeader(buf)
}

// ParseHeader parses an 8-byte buffer into a Header.
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

// ReadPacket reads a full TDS packet (header + payload) from the reader.
// Returns the header and the full packet bytes (including header).
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

// ReadMessage reads a complete TDS message (one or more packets until EOM).
// Returns the packet type, assembled payload (without headers), and all raw packets.
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

// WritePackets writes raw packet bytes to a writer.
func WritePackets(w io.Writer, packets [][]byte) error {
	for _, pkt := range packets {
		if _, err := w.Write(pkt); err != nil {
			return err
		}
	}
	return nil
}

// BuildPackets splits a payload into one or more TDS packets with proper headers.
// packetSize is the maximum size of each packet (including header).
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
