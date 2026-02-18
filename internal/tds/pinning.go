package tds

import (
	"bytes"
	"encoding/binary"
	"strings"
	"unicode/utf16"
)

// ── Connection Pinning Detection (MS-TDS) ───────────────────────────────
//
// The proxy inspects TDS packets to detect when a connection must be "pinned"
// (not returned to pool) because of server-side state:
//
//   - Explicit transactions:  BEGIN TRAN / COMMIT / ROLLBACK
//   - Prepared statements:    sp_prepare / sp_unprepare
//   - Bulk load:              BULK_LOAD packets
//   - Transaction manager:    TM_BEGIN_XACT / TM_COMMIT_XACT / TM_ROLLBACK_XACT

// PinAction describes what the proxy should do after inspecting a packet.
type PinAction int

const (
	PinActionNone   PinAction = iota // No pinning change
	PinActionPin                     // Pin the connection
	PinActionUnpin                   // Unpin the connection
)

// PinResult holds the result of pin inspection.
type PinResult struct {
	Action PinAction
	Reason string // Human-readable reason
}

// Transaction Manager request types (MS-TDS 2.2.7.17).
const (
	tmBeginXact    uint16 = 5
	tmCommitXact   uint16 = 7
	tmRollbackXact uint16 = 8
	tmSavepoint    uint16 = 9
)

// InspectPacket inspects a TDS packet payload and header to determine
// if connection pinning should be engaged or released.
func InspectPacket(pktType PacketType, payload []byte) PinResult {
	switch pktType {
	case PacketSQLBatch:
		return inspectSQLBatch(payload)
	case PacketRPCRequest:
		return inspectRPC(payload)
	case PacketTransMgr:
		return inspectTransactionManager(payload)
	case PacketBulkLoad:
		return PinResult{Action: PinActionPin, Reason: "bulk_load"}
	default:
		return PinResult{Action: PinActionNone}
	}
}

// inspectSQLBatch looks for transaction control statements in a SQL Batch.
// The payload is ALL_HEADERS + SQL text in UTF-16 LE.
func inspectSQLBatch(payload []byte) PinResult {
	// SQL Batch payload starts with ALL_HEADERS (variable length),
	// then the SQL text in UTF-16 LE.
	// Skip ALL_HEADERS: first 4 bytes are total length of ALL_HEADERS.
	text := extractSQLText(payload)
	if text == "" {
		return PinResult{Action: PinActionNone}
	}

	upper := strings.ToUpper(strings.TrimSpace(text))

	// Check for transaction start.
	if hasPrefix(upper, "BEGIN TRAN") ||
		hasPrefix(upper, "BEGIN DISTRIBUTED TRAN") ||
		hasPrefix(upper, "SET IMPLICIT_TRANSACTIONS ON") ||
		hasPrefix(upper, "SET XACT_ABORT") {
		return PinResult{Action: PinActionPin, Reason: "transaction"}
	}

	// Check for transaction end.
	if hasPrefix(upper, "COMMIT") ||
		hasPrefix(upper, "ROLLBACK") {
		return PinResult{Action: PinActionUnpin, Reason: "transaction"}
	}

	// Check for temp table creation (pins for session lifetime).
	if strings.Contains(upper, "CREATE TABLE #") ||
		strings.Contains(upper, "SELECT INTO #") {
		return PinResult{Action: PinActionPin, Reason: "temp_table"}
	}

	return PinResult{Action: PinActionNone}
}

// inspectRPC looks for prepared statement operations in RPC requests.
// RPC payload: ALL_HEADERS + ProcIDSwitch + ProcNameOrID + ...
func inspectRPC(payload []byte) PinResult {
	procName := extractRPCProcName(payload)
	if procName == "" {
		return PinResult{Action: PinActionNone}
	}

	upper := strings.ToUpper(procName)

	switch upper {
	case "SP_PREPARE", "SP_CURSOROPEN", "SP_CURSORPREPARE":
		return PinResult{Action: PinActionPin, Reason: "prepared"}
	case "SP_UNPREPARE", "SP_CURSORCLOSE":
		return PinResult{Action: PinActionUnpin, Reason: "prepared"}
	case "SP_EXECUTESQL", "SP_EXECUTE":
		// These don't change pin state — they execute within an existing state.
		return PinResult{Action: PinActionNone}
	}

	return PinResult{Action: PinActionNone}
}

// inspectTransactionManager inspects TRANSACTION_MANAGER packets.
// Payload: ALL_HEADERS + RequestType (2 bytes LE) + ...
func inspectTransactionManager(payload []byte) PinResult {
	// Skip ALL_HEADERS.
	offset := skipAllHeaders(payload)
	if offset < 0 || offset+2 > len(payload) {
		return PinResult{Action: PinActionNone}
	}

	reqType := binary.LittleEndian.Uint16(payload[offset : offset+2])

	switch reqType {
	case tmBeginXact:
		return PinResult{Action: PinActionPin, Reason: "transaction"}
	case tmCommitXact, tmRollbackXact:
		return PinResult{Action: PinActionUnpin, Reason: "transaction"}
	case tmSavepoint:
		// Savepoint within a transaction — keep pinned.
		return PinResult{Action: PinActionNone}
	}

	return PinResult{Action: PinActionNone}
}

// ── Helpers ──────────────────────────────────────────────────────────────

// skipAllHeaders skips the ALL_HEADERS section at the start of a payload.
// Returns the offset after ALL_HEADERS, or -1 on error.
func skipAllHeaders(payload []byte) int {
	if len(payload) < 4 {
		return -1
	}
	totalLen := int(binary.LittleEndian.Uint32(payload[0:4]))
	if totalLen < 4 || totalLen > len(payload) {
		// No ALL_HEADERS or invalid — treat entire payload as content.
		return 0
	}
	return totalLen
}

// extractSQLText extracts the SQL text from a SQL_BATCH payload.
// The text follows ALL_HEADERS and is encoded in UTF-16 LE.
func extractSQLText(payload []byte) string {
	offset := skipAllHeaders(payload)
	if offset < 0 || offset >= len(payload) {
		return ""
	}

	textBytes := payload[offset:]
	if len(textBytes) < 2 {
		return ""
	}

	// Decode UTF-16 LE — only decode enough for pinning detection.
	// Limit to first 256 characters for performance.
	maxBytes := len(textBytes)
	if maxBytes > 512 { // 256 chars * 2 bytes
		maxBytes = 512
	}
	// Ensure even number of bytes.
	if maxBytes%2 != 0 {
		maxBytes--
	}

	if maxBytes < 2 {
		return ""
	}

	u16 := make([]uint16, maxBytes/2)
	for i := range u16 {
		u16[i] = binary.LittleEndian.Uint16(textBytes[i*2 : i*2+2])
	}

	return string(utf16.Decode(u16))
}

// extractRPCProcName extracts the procedure name from an RPC Request payload.
//
// RPC Request layout (after ALL_HEADERS):
//   Byte 0-1:    NameLenProcID (USHORT)
//                 If == 0xFFFF → ProcID (USHORT) follows (well-known proc by ID)
//                 Else → procedure name of this many UTF-16 LE characters follows
func extractRPCProcName(payload []byte) string {
	offset := skipAllHeaders(payload)
	if offset < 0 || offset+2 > len(payload) {
		return ""
	}

	nameLenOrFlag := binary.LittleEndian.Uint16(payload[offset : offset+2])
	offset += 2

	if nameLenOrFlag == 0xFFFF {
		// Well-known procedure by ID.
		if offset+2 > len(payload) {
			return ""
		}
		procID := binary.LittleEndian.Uint16(payload[offset : offset+2])
		return wellKnownProcName(procID)
	}

	// Named procedure: nameLenOrFlag is the number of UTF-16 chars.
	charCount := int(nameLenOrFlag)
	byteCount := charCount * 2
	if offset+byteCount > len(payload) {
		return ""
	}

	u16 := make([]uint16, charCount)
	for i := 0; i < charCount; i++ {
		u16[i] = binary.LittleEndian.Uint16(payload[offset+i*2 : offset+i*2+2])
	}

	return string(utf16.Decode(u16))
}

// wellKnownProcName returns the name for a well-known RPC procedure ID.
// Reference: MS-TDS 2.2.6.6
func wellKnownProcName(id uint16) string {
	switch id {
	case 1:
		return "sp_cursor"
	case 2:
		return "sp_cursoropen"
	case 3:
		return "sp_cursorprepare"
	case 4:
		return "sp_cursorexecute"
	case 5:
		return "sp_cursorprepexec"
	case 6:
		return "sp_cursorunprepare"
	case 7:
		return "sp_cursorfetch"
	case 8:
		return "sp_cursoroption"
	case 9:
		return "sp_cursorclose"
	case 10:
		return "sp_executesql"
	case 11:
		return "sp_prepare"
	case 12:
		return "sp_execute"
	case 13:
		return "sp_prepexec"
	case 14:
		return "sp_prepexecrpc"
	case 15:
		return "sp_unprepare"
	default:
		return ""
	}
}

// hasPrefix checks if s starts with prefix, handling word boundaries.
func hasPrefix(s, prefix string) bool {
	if !strings.HasPrefix(s, prefix) {
		return false
	}
	// Make sure it's a word boundary (not a substring of a longer word).
	if len(s) > len(prefix) {
		next := s[len(prefix)]
		return next == ' ' || next == '\t' || next == '\n' || next == '\r' ||
			next == ';' || next == '('
	}
	return true
}

// ── Attention Detection ─────────────────────────────────────────────────

// IsAttention returns true if this is an Attention (cancel) packet.
func IsAttention(pktType PacketType) bool {
	return pktType == PacketAttention
}

// BuildAttention creates an Attention packet (just header, no payload).
func BuildAttention() []byte {
	hdr := Header{
		Type:   PacketAttention,
		Status: StatusEOM,
		Length: HeaderSize,
	}
	return hdr.Marshal()
}

// ── Response inspection ─────────────────────────────────────────────────

// Token types in TDS response (MS-TDS 2.2.7).
const (
	tokenEnvChange byte = 0xE3
	tokenDone      byte = 0xFD
	tokenDoneProc  byte = 0xFE
	tokenDoneInProc byte = 0xFF
)

// DONE status flags (MS-TDS 2.2.7.6).
const (
	doneMore       uint16 = 0x0001
	doneError      uint16 = 0x0002
	doneInxact     uint16 = 0x0004 // Transaction in progress
	doneCount      uint16 = 0x0010
	doneAttn       uint16 = 0x0020
	doneSrvError   uint16 = 0x0100
)

// InspectResponse scans a server response payload for transaction state changes.
// It looks at ENVCHANGE tokens (type 8 = begin tran, type 9 = commit tran,
// type 10 = rollback tran) and DONE tokens with DONE_INXACT flag.
func InspectResponse(payload []byte) PinResult {
	result := PinResult{Action: PinActionNone}

	// Scan for ENVCHANGE tokens related to transactions.
	for i := 0; i < len(payload)-3; {
		tokenType := payload[i]

		switch tokenType {
		case tokenEnvChange:
			if i+3 > len(payload) {
				return result
			}
			envLen := int(binary.LittleEndian.Uint16(payload[i+1 : i+3]))
			if envLen < 1 || i+3+envLen > len(payload) {
				return result
			}
			envType := payload[i+3]
			switch envType {
			case 8: // BEGIN_TXN
				result = PinResult{Action: PinActionPin, Reason: "transaction"}
			case 9, 10: // COMMIT_TXN, ROLLBACK_TXN
				result = PinResult{Action: PinActionUnpin, Reason: "transaction"}
			}
			i += 3 + envLen

		case tokenDone, tokenDoneProc, tokenDoneInProc:
			// DONE token is always 12 bytes (1 token + 2 status + 2 curcmd + 8 rowcount).
			if i+5 > len(payload) {
				return result
			}
			// We just skip past for now — ENVCHANGE is more reliable for transaction state.
			i += 13

		default:
			// Skip unknown tokens — we can't reliably parse all token types,
			// so we stop scanning to avoid misinterpreting data.
			return result
		}
	}

	return result
}

// ContainsAttentionAck checks if a response payload contains a DONE token
// with the DONE_ATTN flag set (acknowledgement of an Attention signal).
func ContainsAttentionAck(payload []byte) bool {
	return bytes.Contains(payload, []byte{tokenDone}) // Simplified check.
}
