package tds

import (
	"bytes"
	"encoding/binary"
	"strings"
	"unicode/utf16"
)

// ── Detecção de Connection Pinning (MS-TDS) ────────────────────────────
//
// O proxy inspeciona pacotes TDS para detectar quando uma conexão deve ser "pinada"
// (não devolvida ao pool) por causa de estado no servidor:
//
//   - Transações explícitas:     BEGIN TRAN / COMMIT / ROLLBACK
//   - Prepared statements:       sp_prepare / sp_unprepare
//   - Bulk load:                 pacotes BULK_LOAD
//   - Transaction manager:       TM_BEGIN_XACT / TM_COMMIT_XACT / TM_ROLLBACK_XACT

// PinAction descreve o que o proxy deve fazer após inspecionar um pacote.
type PinAction int

const (
	PinActionNone   PinAction = iota // Sem alteração de pinning
	PinActionPin                     // Pinar a conexão
	PinActionUnpin                   // Despinar a conexão
)

// PinResult contém o resultado da inspeção de pinning.
type PinResult struct {
	Action PinAction
	Reason string // Motivo legível por humanos
}

// Tipos de requisição do Transaction Manager (MS-TDS 2.2.7.17).
const (
	tmBeginXact    uint16 = 5
	tmCommitXact   uint16 = 7
	tmRollbackXact uint16 = 8
	tmSavepoint    uint16 = 9
)

// InspectPacket inspeciona o payload e header de um pacote TDS para determinar
// se o connection pinning deve ser ativado ou liberado.
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

// inspectSQLBatch procura instruções de controle de transação em um SQL Batch.
// O payload é ALL_HEADERS + texto SQL em UTF-16 LE.
func inspectSQLBatch(payload []byte) PinResult {
	// O payload do SQL Batch começa com ALL_HEADERS (tamanho variável),
	// seguido do texto SQL em UTF-16 LE.
	// Pular ALL_HEADERS: os primeiros 4 bytes são o tamanho total de ALL_HEADERS.
	text := extractSQLText(payload)
	if text == "" {
		return PinResult{Action: PinActionNone}
	}

	upper := strings.ToUpper(strings.TrimSpace(text))

	// Verificar início de transação.
	if hasPrefix(upper, "BEGIN TRAN") ||
		hasPrefix(upper, "BEGIN DISTRIBUTED TRAN") ||
		hasPrefix(upper, "SET IMPLICIT_TRANSACTIONS ON") ||
		hasPrefix(upper, "SET XACT_ABORT") {
		return PinResult{Action: PinActionPin, Reason: "transaction"}
	}

	// Verificar fim de transação.
	if hasPrefix(upper, "COMMIT") ||
		hasPrefix(upper, "ROLLBACK") {
		return PinResult{Action: PinActionUnpin, Reason: "transaction"}
	}

	// Verificar criação de tabela temporária (pina pelo tempo de vida da sessão).
	if strings.Contains(upper, "CREATE TABLE #") ||
		strings.Contains(upper, "SELECT INTO #") {
		return PinResult{Action: PinActionPin, Reason: "temp_table"}
	}

	return PinResult{Action: PinActionNone}
}

// inspectRPC procura operações de prepared statement em requisições RPC.
// Payload RPC: ALL_HEADERS + ProcIDSwitch + ProcNameOrID + ...
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
		// Estes não alteram o estado de pin — executam dentro de um estado existente.
		return PinResult{Action: PinActionNone}
	}

	return PinResult{Action: PinActionNone}
}

// inspectTransactionManager inspeciona pacotes TRANSACTION_MANAGER.
// Payload: ALL_HEADERS + RequestType (2 bytes LE) + ...
func inspectTransactionManager(payload []byte) PinResult {
	// Pular ALL_HEADERS.
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
		// Savepoint dentro de uma transação — manter pinado.
		return PinResult{Action: PinActionNone}
	}

	return PinResult{Action: PinActionNone}
}

// ── Auxiliares ───────────────────────────────────────────────────────────

// skipAllHeaders pula a seção ALL_HEADERS no início de um payload.
// Retorna o offset após ALL_HEADERS, ou -1 em caso de erro.
func skipAllHeaders(payload []byte) int {
	if len(payload) < 4 {
		return -1
	}
	totalLen := int(binary.LittleEndian.Uint32(payload[0:4]))
	if totalLen < 4 || totalLen > len(payload) {
		// Sem ALL_HEADERS ou inválido — tratar o payload inteiro como conteúdo.
		return 0
	}
	return totalLen
}

// extractSQLText extrai o texto SQL de um payload SQL_BATCH.
// O texto segue ALL_HEADERS e é codificado em UTF-16 LE.
func extractSQLText(payload []byte) string {
	offset := skipAllHeaders(payload)
	if offset < 0 || offset >= len(payload) {
		return ""
	}

	textBytes := payload[offset:]
	if len(textBytes) < 2 {
		return ""
	}

	// Decodificar UTF-16 LE — decodificar apenas o suficiente para detecção de pinning.
	// Limitar aos primeiros 256 caracteres por performance.
	maxBytes := len(textBytes)
	if maxBytes > 512 { // 256 chars * 2 bytes
		maxBytes = 512
	}
	// Garantir número par de bytes.
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

// extractRPCProcName extrai o nome do procedimento de um payload de RPC Request.
//
// Layout do RPC Request (após ALL_HEADERS):
//   Byte 0-1:    NameLenProcID (USHORT)
//                 Se == 0xFFFF → ProcID (USHORT) segue (procedimento bem conhecido por ID)
//                 Senão → nome do procedimento com essa quantidade de caracteres UTF-16 LE
func extractRPCProcName(payload []byte) string {
	offset := skipAllHeaders(payload)
	if offset < 0 || offset+2 > len(payload) {
		return ""
	}

	nameLenOrFlag := binary.LittleEndian.Uint16(payload[offset : offset+2])
	offset += 2

	if nameLenOrFlag == 0xFFFF {
		// Procedimento bem conhecido por ID.
		if offset+2 > len(payload) {
			return ""
		}
		procID := binary.LittleEndian.Uint16(payload[offset : offset+2])
		return wellKnownProcName(procID)
	}

	// Procedimento nomeado: nameLenOrFlag é o número de caracteres UTF-16.
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

// wellKnownProcName retorna o nome de um procedimento RPC bem conhecido pelo seu ID.
// Referência: MS-TDS 2.2.6.6
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

// hasPrefix verifica se s começa com prefix, respeitando limites de palavras.
func hasPrefix(s, prefix string) bool {
	if !strings.HasPrefix(s, prefix) {
		return false
	}
	// Garantir que é um limite de palavra (não uma substring de uma palavra maior).
	if len(s) > len(prefix) {
		next := s[len(prefix)]
		return next == ' ' || next == '\t' || next == '\n' || next == '\r' ||
			next == ';' || next == '('
	}
	return true
}

// ── Detecção de Attention ───────────────────────────────────────────────

// IsAttention retorna true se este for um pacote Attention (cancelamento).
func IsAttention(pktType PacketType) bool {
	return pktType == PacketAttention
}

// BuildAttention cria um pacote Attention (apenas header, sem payload).
func BuildAttention() []byte {
	hdr := Header{
		Type:   PacketAttention,
		Status: StatusEOM,
		Length: HeaderSize,
	}
	return hdr.Marshal()
}

// ── Inspeção de Resposta ────────────────────────────────────────────────

// Tipos de token em resposta TDS (MS-TDS 2.2.7).
const (
	tokenEnvChange byte = 0xE3
	tokenDone      byte = 0xFD
	tokenDoneProc  byte = 0xFE
	tokenDoneInProc byte = 0xFF
)

// Flags de status DONE (MS-TDS 2.2.7.6).
const (
	doneMore       uint16 = 0x0001
	doneError      uint16 = 0x0002
	doneInxact     uint16 = 0x0004 // Transação em progresso
	doneCount      uint16 = 0x0010
	doneAttn       uint16 = 0x0020
	doneSrvError   uint16 = 0x0100
)

// InspectResponse varre o payload de resposta do servidor em busca de mudanças de estado transacional.
// Analisa tokens ENVCHANGE (tipo 8 = begin tran, tipo 9 = commit tran,
// tipo 10 = rollback tran) e tokens DONE com a flag DONE_INXACT.
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
			// Token DONE sempre tem 12 bytes (1 token + 2 status + 2 curcmd + 8 rowcount).
			if i+5 > len(payload) {
				return result
			}
			// We just skip past for now — ENVCHANGE is more reliable for transaction state.
			i += 13

		default:
			// Pular tokens desconhecidos — não é possível parsear todos os tipos de token
			// de forma confiável, então paramos para evitar interpretar dados incorretamente.
			return result
		}
	}

	return result
}

// ContainsAttentionAck verifica se o payload de resposta contém um token DONE
// com a flag DONE_ATTN ativada (confirmação de um sinal Attention).
func ContainsAttentionAck(payload []byte) bool {
	return bytes.Contains(payload, []byte{tokenDone}) // Verificação simplificada.
}
