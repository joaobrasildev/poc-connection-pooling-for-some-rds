package tds

import (
	"encoding/binary"
	"fmt"
	"io"
)

// ── Pre-Login Token Types (MS-TDS 2.2.6.5) ──────────────────────────

// PreLoginOptionToken identifies fields in a Pre-Login packet.
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

// Encryption options.
const (
	EncryptOff    byte = 0x00
	EncryptOn     byte = 0x01
	EncryptNotSup byte = 0x02
	EncryptReq    byte = 0x03
)

// PreLoginOption represents a single option in the Pre-Login packet.
type PreLoginOption struct {
	Token  PreLoginOptionToken
	Data   []byte
}

// PreLoginMsg holds parsed Pre-Login options.
type PreLoginMsg struct {
	Options []PreLoginOption
}

// ParsePreLogin parses the payload of a Pre-Login packet (without TDS header).
func ParsePreLogin(payload []byte) (*PreLoginMsg, error) {
	if len(payload) < 1 {
		return nil, fmt.Errorf("prelogin payload is empty")
	}

	msg := &PreLoginMsg{}

	// Phase 1: parse option headers (token, offset, length).
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

	// Phase 2: extract option data.
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

// Encryption returns the encryption flag from the Pre-Login options.
func (m *PreLoginMsg) Encryption() byte {
	for _, opt := range m.Options {
		if opt.Token == PreLoginEncryption && len(opt.Data) > 0 {
			return opt.Data[0]
		}
	}
	return EncryptNotSup
}

// SetEncryption updates the encryption option in the Pre-Login message.
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

// Marshal serializes the Pre-Login message back to bytes.
func (m *PreLoginMsg) Marshal() []byte {
	// Calculate header size: 5 bytes per option + 1 byte terminator.
	headerSize := len(m.Options)*5 + 1

	// Calculate total size.
	totalSize := headerSize
	for _, opt := range m.Options {
		totalSize += len(opt.Data)
	}

	buf := make([]byte, totalSize)

	// Write option headers.
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

// BuildPreLoginResponse creates a minimal Pre-Login response payload.
// The proxy responds with the same version and ENCRYPT_NOT_SUP for the POC.
func BuildPreLoginResponse(clientPreLogin *PreLoginMsg) []byte {
	resp := &PreLoginMsg{}

	// Copy version from client or use a default.
	var versionData []byte
	for _, opt := range clientPreLogin.Options {
		if opt.Token == PreLoginVersion {
			versionData = make([]byte, len(opt.Data))
			copy(versionData, opt.Data)
			break
		}
	}
	if versionData == nil {
		// SQL Server 2022 version: 16.0.4236
		versionData = []byte{0x10, 0x00, 0x10, 0x8C, 0x00, 0x00}
	}
	resp.Options = append(resp.Options, PreLoginOption{Token: PreLoginVersion, Data: versionData})

	// Respond with encryption off for POC.
	resp.Options = append(resp.Options, PreLoginOption{Token: PreLoginEncryption, Data: []byte{EncryptNotSup}})

	// MARS disabled.
	resp.Options = append(resp.Options, PreLoginOption{Token: PreLoginMARS, Data: []byte{0x00}})

	return resp.Marshal()
}

// ForwardPreLogin reads a Pre-Login message from the client, forwards it to
// the backend, reads the backend response, and sends it back to the client.
// Returns the parsed client PreLogin for inspection.
func ForwardPreLogin(client io.ReadWriter, backend io.ReadWriter) (*PreLoginMsg, error) {
	// Read Pre-Login from client.
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

	// Forward Pre-Login to backend.
	fwdPackets := BuildPackets(PacketPreLogin, payload, 4096)
	if err := WritePackets(backend, fwdPackets); err != nil {
		return nil, fmt.Errorf("forwarding prelogin to backend: %w", err)
	}

	// Read Pre-Login response from backend.
	respType, _, respPackets, err := ReadMessage(backend)
	if err != nil {
		return nil, fmt.Errorf("reading backend prelogin response: %w", err)
	}
	_ = respType // Pre-Login response is type 0x04 (Reply)

	// Forward backend response to client.
	if err := WritePackets(client, respPackets); err != nil {
		return nil, fmt.Errorf("forwarding prelogin response to client: %w", err)
	}

	return clientPL, nil
}
