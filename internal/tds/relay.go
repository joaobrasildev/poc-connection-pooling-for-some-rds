package tds

import (
	"io"
	"log"
	"sync"
)

// ── Relay TDS Bidirecional ───────────────────────────────────────────
//
// O relay copia pacotes TDS entre cliente e servidor em ambas as direções.
// Também inspeciona pacotes para mudanças de estado de pinning de conexão.

// PacketCallback é chamado para cada pacote TDS retransmitido.
// direction é "client_to_server" ou "server_to_client".
// Retorne um erro para abortar o relay.
type PacketCallback func(direction string, pktType PacketType, payload []byte) error

// Relay realiza relay bidirecional de pacotes TDS entre cliente e backend.
// Executa até que um dos lados feche a conexão ou ocorra um erro.
// O callback é invocado para cada pacote para inspeção de pinning.
// Retorna o primeiro erro que causou a parada do relay.
func Relay(client io.ReadWriter, backend io.ReadWriter, callback PacketCallback) error {
	var (
		once    sync.Once
		result  error
		done    = make(chan struct{})
	)

	setResult := func(err error) {
		once.Do(func() {
			result = err
			close(done)
		})
	}

	// Cliente → Servidor
	go func() {
		err := relayDirection(client, backend, "client_to_server", callback)
		setResult(err)
	}()

	// Servidor → Cliente
	go func() {
		err := relayDirection(backend, client, "server_to_client", callback)
		setResult(err)
	}()

	<-done
	return result
}

// relayDirection copia pacotes TDS de src para dst em uma direção.
func relayDirection(src io.Reader, dst io.Writer, direction string, callback PacketCallback) error {
	for {
		// Ler um pacote TDS completo.
		hdr, pkt, err := ReadPacket(src)
		if err != nil {
			return err
		}

		// Invocar callback para inspeção de pinning.
		if callback != nil {
			payload := pkt[HeaderSize:]
			if err := callback(direction, hdr.Type, payload); err != nil {
				return err
			}
		}

		// Encaminhar pacote ao destino.
		if _, err := dst.Write(pkt); err != nil {
			return err
		}
	}
}

// RelayMessage lê uma mensagem TDS completa (todos os pacotes até EOM) de src,
// encaminha todos os pacotes para dst, e retorna o payload montado e o tipo de pacote.
// Usado durante a fase de login onde precisamos ler mensagens completas.
func RelayMessage(src io.Reader, dst io.Writer) (PacketType, []byte, error) {
	pktType, payload, packets, err := ReadMessage(src)
	if err != nil {
		return 0, nil, err
	}

	if err := WritePackets(dst, packets); err != nil {
		return 0, nil, err
	}

	return pktType, payload, nil
}

// RelayUntilEOM lê pacotes de src e os encaminha para dst até que um
// pacote EOM (End of Message) seja recebido. Retorna todos os pacotes encaminhados.
func RelayUntilEOM(src io.Reader, dst io.Writer) ([][]byte, error) {
	var packets [][]byte

	for {
		hdr, pkt, err := ReadPacket(src)
		if err != nil {
			return nil, err
		}

		if _, err := dst.Write(pkt); err != nil {
			return nil, err
		}

		packets = append(packets, pkt)

		if hdr.IsEOM() {
			break
		}
	}

	return packets, nil
}

// DrainResponse lê e descarta uma mensagem de resposta TDS completa do reader.
// Usado quando o proxy precisa consumir uma resposta sem encaminhá-la.
func DrainResponse(r io.Reader) error {
	for {
		hdr, _, err := ReadPacket(r)
		if err != nil {
			return err
		}
		if hdr.IsEOM() {
			return nil
		}
	}
}

// ForwardLogin7 lê uma mensagem Login7 do cliente, faz o parse para roteamento,
// e a encaminha ao backend. Retorna o Login7Info parseado.
func ForwardLogin7(client io.Reader, backend io.Writer) (*Login7Info, error) {
	pktType, payload, packets, err := ReadMessage(client)
	if err != nil {
		return nil, err
	}

	if pktType != PacketLogin7 {
		return nil, &ProtocolError{
			Message: "expected LOGIN7 packet",
			Got:     pktType,
			Want:    PacketLogin7,
		}
	}

	login7, err := ParseLogin7(payload)
	if err != nil {
		return nil, err
	}

	log.Printf("[tds] Login7: user=%q, database=%q, server=%q, app=%q",
		login7.UserName, login7.Database, login7.ServerName, login7.AppName)

	// Encaminhar o Login7 ao backend.
	if err := WritePackets(backend, packets); err != nil {
		return nil, err
	}

	return login7, nil
}

// ProtocolError representa uma violação do protocolo TDS.
type ProtocolError struct {
	Message string
	Got     PacketType
	Want    PacketType
}

func (e *ProtocolError) Error() string {
	return e.Message + ": got " + e.Got.String() + ", want " + e.Want.String()
}
