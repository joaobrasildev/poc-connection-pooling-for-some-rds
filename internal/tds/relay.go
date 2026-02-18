package tds

import (
	"io"
	"log"
	"sync"
)

// ── Bidirectional TDS Relay ─────────────────────────────────────────────
//
// The relay copies TDS packets between client and server in both directions.
// It also inspects packets for connection pinning state changes.

// PacketCallback is called for every TDS packet relayed.
// direction is "client_to_server" or "server_to_client".
// Return an error to abort the relay.
type PacketCallback func(direction string, pktType PacketType, payload []byte) error

// Relay performs bidirectional TDS packet relay between client and backend.
// It runs until either side closes the connection or an error occurs.
// The callback is invoked for every packet for pinning inspection.
// Returns the first error that caused the relay to stop.
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

	// Client → Server
	go func() {
		err := relayDirection(client, backend, "client_to_server", callback)
		setResult(err)
	}()

	// Server → Client
	go func() {
		err := relayDirection(backend, client, "server_to_client", callback)
		setResult(err)
	}()

	<-done
	return result
}

// relayDirection copies TDS packets from src to dst in one direction.
func relayDirection(src io.Reader, dst io.Writer, direction string, callback PacketCallback) error {
	for {
		// Read one full TDS packet.
		hdr, pkt, err := ReadPacket(src)
		if err != nil {
			return err
		}

		// Invoke callback for pinning inspection.
		if callback != nil {
			payload := pkt[HeaderSize:]
			if err := callback(direction, hdr.Type, payload); err != nil {
				return err
			}
		}

		// Forward packet to destination.
		if _, err := dst.Write(pkt); err != nil {
			return err
		}
	}
}

// RelayMessage reads a complete TDS message (all packets until EOM) from src,
// forwards all packets to dst, and returns the assembled payload and packet type.
// This is used during the login phase where we need to read full messages.
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

// RelayUntilEOM reads packets from src and forwards them to dst until an
// EOM (End of Message) packet is received. Returns all packets forwarded.
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

// DrainResponse reads and discards a complete TDS response message from the reader.
// Used when the proxy needs to consume a response without forwarding it.
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

// ForwardLogin7 reads a Login7 message from the client, parses it for routing,
// and forwards it to the backend. Returns the parsed Login7Info.
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

	// Forward the Login7 to backend.
	if err := WritePackets(backend, packets); err != nil {
		return nil, err
	}

	return login7, nil
}

// ProtocolError represents a TDS protocol violation.
type ProtocolError struct {
	Message string
	Got     PacketType
	Want    PacketType
}

func (e *ProtocolError) Error() string {
	return e.Message + ": got " + e.Got.String() + ", want " + e.Want.String()
}
