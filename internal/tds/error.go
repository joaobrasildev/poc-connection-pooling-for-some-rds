package tds

import (
	"encoding/binary"
)

// ── TDS Error Token Builder ─────────────────────────────────────────────
//
// When the proxy needs to send an error back to the client (e.g., pool
// exhausted, queue timeout, routing failure), it builds a minimal TDS
// response containing an ERROR token and a DONE token.
//
// Reference: MS-TDS 2.2.7.9 (ERROR token)

// Error severity levels.
const (
	SeverityInfo    uint8 = 10
	SeverityWarning uint8 = 11
	SeverityError   uint8 = 16
	SeverityFatal   uint8 = 20
)

// Token type constants.
const (
	tokenError      byte = 0xAA
	tokenLoginAck   byte = 0xAD
	tokenEnvchange  byte = 0xE3
)

// BuildErrorResponse creates a TDS response containing an ERROR token
// and a DONE(ERROR) token, suitable for sending to the client.
func BuildErrorResponse(msgNumber uint32, severity uint8, message string, serverName string) []byte {
	errorToken := buildErrorToken(msgNumber, severity, message, serverName)
	doneToken := buildDoneError()

	payload := make([]byte, 0, len(errorToken)+len(doneToken))
	payload = append(payload, errorToken...)
	payload = append(payload, doneToken...)

	return buildResponsePackets(payload)
}

// buildErrorToken builds an ERROR token (0xAA).
//
// Layout:
//   Byte 0:     Token type (0xAA)
//   Byte 1-2:   Length (uint16 LE)
//   Byte 3-6:   Number (uint32 LE) — error number
//   Byte 7:     State (uint8)
//   Byte 8:     Class/Severity (uint8)
//   Byte 9-10:  MsgTextLength (uint16 LE) — chars, not bytes
//   Byte 11+:   MsgText (UTF-16 LE)
//   After text: ServerNameLength (uint8) + ServerName (UTF-16 LE)
//   After name: ProcNameLength (uint8) + ProcName (UTF-16 LE)
//   After proc: LineNumber (uint32 LE)
func buildErrorToken(number uint32, severity uint8, message string, serverName string) []byte {
	msgUTF16 := encodeUTF16LE(message)
	srvUTF16 := encodeUTF16LE(serverName)
	procUTF16 := encodeUTF16LE("") // No proc name.

	// Calculate total token data length (everything after the 3-byte token header).
	dataLen := 4 + // Number
		1 + // State
		1 + // Class
		2 + len(msgUTF16) + // MsgTextLength + MsgText
		1 + len(srvUTF16) + // ServerNameLength + ServerName
		1 + len(procUTF16) + // ProcNameLength + ProcName
		4 // LineNumber

	buf := make([]byte, 0, 3+dataLen)

	// Token type.
	buf = append(buf, tokenError)

	// Length (uint16 LE).
	lenBytes := make([]byte, 2)
	binary.LittleEndian.PutUint16(lenBytes, uint16(dataLen))
	buf = append(buf, lenBytes...)

	// Number (uint32 LE).
	numBytes := make([]byte, 4)
	binary.LittleEndian.PutUint32(numBytes, number)
	buf = append(buf, numBytes...)

	// State.
	buf = append(buf, 1) // State 1 is generic.

	// Class (severity).
	buf = append(buf, severity)

	// MsgText length in chars (uint16 LE).
	msgLenBytes := make([]byte, 2)
	binary.LittleEndian.PutUint16(msgLenBytes, uint16(len(message)))
	buf = append(buf, msgLenBytes...)
	buf = append(buf, msgUTF16...)

	// ServerName length in chars (uint8).
	buf = append(buf, uint8(len([]rune(serverName))))
	buf = append(buf, srvUTF16...)

	// ProcName length in chars (uint8).
	buf = append(buf, 0) // Empty proc name.
	buf = append(buf, procUTF16...)

	// LineNumber (uint32 LE).
	lineBytes := make([]byte, 4)
	binary.LittleEndian.PutUint32(lineBytes, 0)
	buf = append(buf, lineBytes...)

	return buf
}

// buildDoneError builds a DONE token with the ERROR flag set.
//
// Layout (12 bytes total):
//   Byte 0:     Token type (0xFD)
//   Byte 1-2:   Status (uint16 LE) — DONE_ERROR = 0x0002, DONE_FINAL = 0x0000
//   Byte 3-4:   CurCmd (uint16 LE)
//   Byte 5-12:  RowCount (uint64 LE)
func buildDoneError() []byte {
	buf := make([]byte, 13)
	buf[0] = tokenDone
	binary.LittleEndian.PutUint16(buf[1:3], doneError) // Status: DONE_ERROR
	binary.LittleEndian.PutUint16(buf[3:5], 0)         // CurCmd
	// RowCount is 8 bytes, left as zeros.
	return buf
}

// buildResponsePackets wraps a token stream payload into TDS Reply packets.
func buildResponsePackets(payload []byte) []byte {
	packets := BuildPackets(PacketReply, payload, 4096)
	var result []byte
	for _, pkt := range packets {
		result = append(result, pkt...)
	}
	return result
}

// ── Pre-built error messages ────────────────────────────────────────────

// ErrPoolExhausted builds an error response for when the connection pool is full.
func ErrPoolExhausted(bucketID string) []byte {
	return BuildErrorResponse(
		50001,
		SeverityError,
		"Connection pool exhausted for bucket '"+bucketID+"'. All connections are in use and the queue timed out.",
		"proxy",
	)
}

// ErrRoutingFailed builds an error response for when routing fails.
func ErrRoutingFailed(database string) []byte {
	return BuildErrorResponse(
		50002,
		SeverityError,
		"No bucket configured for database '"+database+"'. Check proxy routing configuration.",
		"proxy",
	)
}

// ErrBackendUnavailable builds an error response for when the backend is unreachable.
func ErrBackendUnavailable(bucketID string) []byte {
	return BuildErrorResponse(
		50003,
		SeverityFatal,
		"Backend SQL Server for bucket '"+bucketID+"' is unavailable.",
		"proxy",
	)
}

// ErrInternalError builds a generic internal error response.
func ErrInternalError(message string) []byte {
	return BuildErrorResponse(
		50000,
		SeverityError,
		"Internal proxy error: "+message,
		"proxy",
	)
}

// ErrQueueTimeout builds an error response for when a request waited in the
// queue for a connection but timed out before one became available.
func ErrQueueTimeout(bucketID string) []byte {
	return BuildErrorResponse(
		50004,
		SeverityError,
		"Connection queue timed out for bucket '"+bucketID+"'. All connections are in use and the wait period has expired. Try again later.",
		"proxy",
	)
}

// ErrQueueFull builds an error response for when the connection queue has
// reached its maximum size (circuit breaker). The request is rejected
// immediately without waiting.
func ErrQueueFull(bucketID string) []byte {
	return BuildErrorResponse(
		50005,
		SeverityError,
		"Connection queue is full for bucket '"+bucketID+"'. Too many requests are already waiting. Try again later.",
		"proxy",
	)
}
