package tds

import (
	"encoding/binary"
)

// ── TDS Error Token Builder ─────────────────────────────────────────────
//
// Quando o proxy precisa enviar um erro de volta ao cliente (ex: pool
// esgotado, timeout de fila, falha de roteamento), ele constrói uma
// resposta TDS mínima contendo um token ERROR e um token DONE.
//
// Referência: MS-TDS 2.2.7.9 (ERROR token)

// Níveis de severidade de erro.
const (
	SeverityInfo    uint8 = 10
	SeverityWarning uint8 = 11
	SeverityError   uint8 = 16
	SeverityFatal   uint8 = 20
)

// Constantes de tipo de token.
const (
	tokenError      byte = 0xAA
	tokenLoginAck   byte = 0xAD
	tokenEnvchange  byte = 0xE3
)

// BuildErrorResponse cria uma resposta TDS contendo um token ERROR
// e um token DONE(ERROR), adequada para envio ao cliente.
func BuildErrorResponse(msgNumber uint32, severity uint8, message string, serverName string) []byte {
	errorToken := buildErrorToken(msgNumber, severity, message, serverName)
	doneToken := buildDoneError()

	payload := make([]byte, 0, len(errorToken)+len(doneToken))
	payload = append(payload, errorToken...)
	payload = append(payload, doneToken...)

	return buildResponsePackets(payload)
}

// buildErrorToken constrói um token ERROR (0xAA).
//
// Layout:
//   Byte 0:     Tipo do token (0xAA)
//   Byte 1-2:   Comprimento (uint16 LE)
//   Byte 3-6:   Número (uint32 LE) — número do erro
//   Byte 7:     State (uint8)
//   Byte 8:     Class/Severidade (uint8)
//   Byte 9-10:  MsgTextLength (uint16 LE) — caracteres, não bytes
//   Byte 11+:   MsgText (UTF-16 LE)
//   Após texto: ServerNameLength (uint8) + ServerName (UTF-16 LE)
//   Após nome:  ProcNameLength (uint8) + ProcName (UTF-16 LE)
//   Após proc:  LineNumber (uint32 LE)
func buildErrorToken(number uint32, severity uint8, message string, serverName string) []byte {
	msgUTF16 := encodeUTF16LE(message)
	srvUTF16 := encodeUTF16LE(serverName)
	procUTF16 := encodeUTF16LE("") // Sem nome de proc.

	// Calcula o comprimento total dos dados do token (tudo após o header de 3 bytes).
	dataLen := 4 + // Number
		1 + // State
		1 + // Class
		2 + len(msgUTF16) + // MsgTextLength + MsgText
		1 + len(srvUTF16) + // ServerNameLength + ServerName
		1 + len(procUTF16) + // ProcNameLength + ProcName
		4 // LineNumber

	buf := make([]byte, 0, 3+dataLen)

	// Tipo do token.
	buf = append(buf, tokenError)

	// Comprimento (uint16 LE).
	lenBytes := make([]byte, 2)
	binary.LittleEndian.PutUint16(lenBytes, uint16(dataLen))
	buf = append(buf, lenBytes...)

	// Número (uint32 LE).
	numBytes := make([]byte, 4)
	binary.LittleEndian.PutUint32(numBytes, number)
	buf = append(buf, numBytes...)

	// State.
	buf = append(buf, 1) // State 1 é genérico.

	// Class (severidade).
	buf = append(buf, severity)

	// Comprimento do MsgText em caracteres (uint16 LE).
	msgLenBytes := make([]byte, 2)
	binary.LittleEndian.PutUint16(msgLenBytes, uint16(len(message)))
	buf = append(buf, msgLenBytes...)
	buf = append(buf, msgUTF16...)

	// Comprimento do ServerName em caracteres (uint8).
	buf = append(buf, uint8(len([]rune(serverName))))
	buf = append(buf, srvUTF16...)

	// Comprimento do ProcName em caracteres (uint8).
	buf = append(buf, 0) // Nome de proc vazio.
	buf = append(buf, procUTF16...)

	// LineNumber (uint32 LE).
	lineBytes := make([]byte, 4)
	binary.LittleEndian.PutUint32(lineBytes, 0)
	buf = append(buf, lineBytes...)

	return buf
}

// buildDoneError constrói um token DONE com a flag ERROR definida.
//
// Layout (12 bytes no total):
//   Byte 0:     Tipo do token (0xFD)
//   Byte 1-2:   Status (uint16 LE) — DONE_ERROR = 0x0002, DONE_FINAL = 0x0000
//   Byte 3-4:   CurCmd (uint16 LE)
//   Byte 5-12:  RowCount (uint64 LE)
func buildDoneError() []byte {
	buf := make([]byte, 13)
	buf[0] = tokenDone
	binary.LittleEndian.PutUint16(buf[1:3], doneError) // Status: DONE_ERROR
	binary.LittleEndian.PutUint16(buf[3:5], 0)         // CurCmd
	// RowCount tem 8 bytes, deixados como zeros.
	return buf
}

// buildResponsePackets encapsula um payload de token stream em pacotes TDS Reply.
func buildResponsePackets(payload []byte) []byte {
	packets := BuildPackets(PacketReply, payload, 4096)
	var result []byte
	for _, pkt := range packets {
		result = append(result, pkt...)
	}
	return result
}

// ── Mensagens de erro pré-construídas ───────────────────────────────────

// ErrPoolExhausted constrói uma resposta de erro para quando o connection pool está cheio.
func ErrPoolExhausted(bucketID string) []byte {
	return BuildErrorResponse(
		50001,
		SeverityError,
		"Connection pool exhausted for bucket '"+bucketID+"'. All connections are in use and the queue timed out.",
		"proxy",
	)
}

// ErrRoutingFailed constrói uma resposta de erro para quando o roteamento falha.
func ErrRoutingFailed(database string) []byte {
	return BuildErrorResponse(
		50002,
		SeverityError,
		"No bucket configured for database '"+database+"'. Check proxy routing configuration.",
		"proxy",
	)
}

// ErrBackendUnavailable constrói uma resposta de erro para quando o backend está inacessível.
func ErrBackendUnavailable(bucketID string) []byte {
	return BuildErrorResponse(
		50003,
		SeverityFatal,
		"Backend SQL Server for bucket '"+bucketID+"' is unavailable.",
		"proxy",
	)
}

// ErrInternalError constrói uma resposta genérica de erro interno.
func ErrInternalError(message string) []byte {
	return BuildErrorResponse(
		50000,
		SeverityError,
		"Internal proxy error: "+message,
		"proxy",
	)
}

// ErrQueueTimeout constrói uma resposta de erro para quando uma requisição esperou na
// fila por uma conexão mas o timeout expirou antes de uma ficar disponível.
func ErrQueueTimeout(bucketID string) []byte {
	return BuildErrorResponse(
		50004,
		SeverityError,
		"Connection queue timed out for bucket '"+bucketID+"'. All connections are in use and the wait period has expired. Try again later.",
		"proxy",
	)
}

// ErrQueueFull constrói uma resposta de erro para quando a fila de conexões
// atingiu seu tamanho máximo (circuit breaker). A requisição é rejeitada
// imediatamente sem esperar.
func ErrQueueFull(bucketID string) []byte {
	return BuildErrorResponse(
		50005,
		SeverityError,
		"Connection queue is full for bucket '"+bucketID+"'. Too many requests are already waiting. Try again later.",
		"proxy",
	)
}
