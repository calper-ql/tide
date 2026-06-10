package protocol

import (
	"errors"
	"net"
	"testing"
	"time"

	"github.com/calper-ql/tide/internal/version"
)

// pipe returns two connected Conns. net.Pipe is synchronous, which also
// proves the handshake's fixed ordering cannot deadlock on an unbuffered
// transport.
func pipe() (*Conn, *Conn) {
	a, b := net.Pipe()
	return NewConn(a), NewConn(b)
}

func TestHandshakeMatch(t *testing.T) {
	server, client := pipe()
	errc := make(chan error, 1)
	go func() {
		peer, err := server.ServerHandshake()
		if err == nil && peer.ProtocolVersion != version.Protocol {
			err = errors.New("server saw wrong protocol version")
		}
		errc <- err
	}()

	peer, err := client.ClientHandshake()
	if err != nil {
		t.Fatal(err)
	}
	if peer.BinaryVersion != version.Binary || peer.ProtocolVersion != version.Protocol {
		t.Fatalf("client saw peer %+v", peer)
	}
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
}

func TestHandshakeMismatchIsTyped(t *testing.T) {
	server, client := pipe()
	go func() {
		// A daemon from the future: hello stays parseable, version differs.
		_ = server.Send(Message{Type: TypeHello, BinaryVersion: "9.9.9", ProtocolVersion: version.Protocol + 1})
		_, _ = server.Recv()
	}()

	_, err := client.ClientHandshake()
	var mm *MismatchError
	if !errors.As(err, &mm) {
		t.Fatalf("err = %v, want MismatchError", err)
	}
	if mm.PeerProtocol != version.Protocol+1 || mm.PeerBinary != "9.9.9" {
		t.Fatalf("mismatch detail = %+v", mm)
	}
}

func TestMessageRoundtrip(t *testing.T) {
	server, client := pipe()
	want := Message{
		Type: TypeSessions,
		Sessions: []SessionInfo{
			{Root: "/home/u/proj", CreatedAt: time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC), Clients: 2},
		},
	}
	go func() { _ = server.Send(want) }()

	got, err := client.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if got.Type != want.Type || len(got.Sessions) != 1 {
		t.Fatalf("got %+v", got)
	}
	s := got.Sessions[0]
	if s.Root != "/home/u/proj" || s.Clients != 2 || !s.CreatedAt.Equal(want.Sessions[0].CreatedAt) {
		t.Fatalf("session = %+v", s)
	}
}
