package tds

import (
	"encoding/binary"
	"fmt"
	"unicode/utf16"
)

// ── Parser de Pacote Login7 (MS-TDS 2.2.6.4) ──────────────────────────────
//
// O pacote Login7 contém dados de autenticação e propriedades de conexão.
// O proxy só precisa extrair:
//   - Nome do banco   (para roteamento ao bucket correto)
//   - Username        (para logging/métricas)
//   - Nome do servidor (para roteamento alternativo)
//
// Login7 layout (fixed header at offset 0 within the payload):
//
//   Bytes 0-3:   Length (total packet data length, little-endian)
//   Bytes 4-7:   TDSVersion
//   Bytes 8-11:  PacketSize
//   Bytes 12-15: ClientProgVer
//   Bytes 16-19: ClientPID
//   Bytes 20-23: ConnectionID
//   Byte  24:    OptionFlags1
//   Byte  25:    OptionFlags2
//   Byte  26:    TypeFlags
//   Byte  27:    OptionFlags3
//   Bytes 28-31: ClientTimeZone
//   Bytes 32-35: ClientLCID
//
//   Variable-length fields are specified by (offset, length) pairs
//   starting at byte 36 of the Login7 data:
//
//   Offset 36:  ibHostName    / cchHostName
//   Offset 40:  ibUserName    / cchUserName
//   Offset 44:  ibPassword    / cchPassword
//   Offset 48:  ibAppName     / cchAppName
//   Offset 52:  ibServerName  / cchServerName
//   Offset 56:  ibUnused      / cchUnused     (or ibExtension / cbExtension)
//   Offset 60:  ibCltIntName  / cchCltIntName
//   Offset 64:  ibLanguage    / cchLanguage
//   Offset 68:  ibDatabase    / cchDatabase
//
//   Each (offset, length) is 2+2 bytes (uint16 LE), where offset is from
//   the start of the Login7 data, and length is in characters (UTF-16 code units).

// Login7Info contém campos extraídos de um pacote Login7.
type Login7Info struct {
	// TDSVersion extraída do Login7.
	TDSVersion uint32

	// Hostname do cliente.
	HostName string

	// Username para autenticação do SQL Server.
	UserName string

	// AppName é o nome da aplicação cliente.
	AppName string

	// ServerName é o nome do servidor que o cliente solicitou.
	ServerName string

	// Database é o nome do banco inicial — usado para roteamento.
	Database string

	// ClientInterfaceName é o nome da biblioteca cliente (ex: "go-mssqldb").
	ClientInterfaceName string
}

// ParseLogin7 faz o parse de um payload Login7 (os bytes após o header TDS)
// e extrai os campos que o proxy precisa para roteamento.
func ParseLogin7(payload []byte) (*Login7Info, error) {
	// Tamanho mínimo do Login7: 36 bytes de header fixo + pelo menos os pares
	// offset/length até ibDatabase (offset 68 + 4 = 72 bytes).
	const minLogin7Size = 72

	if len(payload) < minLogin7Size {
		return nil, fmt.Errorf("login7 payload too short: %d bytes (need >= %d)", len(payload), minLogin7Size)
	}

	info := &Login7Info{}

	// Extrair TDS Version (bytes 4-7, little-endian).
	info.TDSVersion = binary.LittleEndian.Uint32(payload[4:8])

	// Extrair campos de comprimento variável usando pares offset/length.
	// Cada par é: offset (uint16 LE na pos), length_in_chars (uint16 LE na pos+2).

	// Helper para ler uma string UTF-16 LE do payload.
	readField := func(offsetPos int) (string, error) {
		if offsetPos+4 > len(payload) {
			return "", fmt.Errorf("field descriptor at %d out of bounds", offsetPos)
		}
		ibField := int(binary.LittleEndian.Uint16(payload[offsetPos : offsetPos+2]))
		cchField := int(binary.LittleEndian.Uint16(payload[offsetPos+2 : offsetPos+4]))

		if cchField == 0 {
			return "", nil
		}

		// Cada caractere tem 2 bytes (UTF-16 LE).
		byteLen := cchField * 2
		if ibField+byteLen > len(payload) {
			return "", fmt.Errorf("field at offset %d, len %d chars overflows payload (%d bytes)",
				ibField, cchField, len(payload))
		}

		return decodeUTF16LE(payload[ibField : ibField+byteLen])
	}

	var err error

	info.HostName, err = readField(36)
	if err != nil {
		return nil, fmt.Errorf("login7 hostname: %w", err)
	}

	info.UserName, err = readField(40)
	if err != nil {
		return nil, fmt.Errorf("login7 username: %w", err)
	}

	// Password no offset 44 — não precisamos decodificá-la (está ofuscada com XOR).

	info.AppName, err = readField(48)
	if err != nil {
		return nil, fmt.Errorf("login7 appname: %w", err)
	}

	info.ServerName, err = readField(52)
	if err != nil {
		return nil, fmt.Errorf("login7 servername: %w", err)
	}

	info.ClientInterfaceName, err = readField(60)
	if err != nil {
		return nil, fmt.Errorf("login7 client interface name: %w", err)
	}

	info.Database, err = readField(68)
	if err != nil {
		return nil, fmt.Errorf("login7 database: %w", err)
	}

	return info, nil
}

// decodeUTF16LE decodifica um slice de bytes UTF-16 little-endian para uma string Go.
func decodeUTF16LE(b []byte) (string, error) {
	if len(b)%2 != 0 {
		return "", fmt.Errorf("UTF-16 LE data has odd length %d", len(b))
	}

	u16 := make([]uint16, len(b)/2)
	for i := 0; i < len(u16); i++ {
		u16[i] = binary.LittleEndian.Uint16(b[i*2 : i*2+2])
	}

	return string(utf16.Decode(u16)), nil
}

// encodeUTF16LE codifica uma string Go para bytes UTF-16 little-endian.
func encodeUTF16LE(s string) []byte {
	runes := []rune(s)
	u16 := utf16.Encode(runes)
	b := make([]byte, len(u16)*2)
	for i, v := range u16 {
		binary.LittleEndian.PutUint16(b[i*2:i*2+2], v)
	}
	return b
}
