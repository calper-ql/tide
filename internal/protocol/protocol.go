// Package protocol is the wire contract between tide clients and the
// daemon: newline-delimited JSON frames over a user-private unix socket,
// opened by a hello exchange (spec: version handshake first). Tide-family
// tools target this package; the message set, not the binary, is the
// coherence boundary.
package protocol

import (
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/calper-ql/tide/internal/version"
)

// Message types. Hello must stay parseable across all future protocol
// versions — it is how mismatches are detected at all.
const (
	TypeHello    = "hello"    // both directions, first frame on every conn
	TypeAttach   = "attach"   // client → daemon: join (or create) session Root
	TypeLs       = "ls"       // client → daemon: list sessions
	TypeKill     = "kill"     // client → daemon: end session Root explicitly
	TypeShutdown = "shutdown" // client → daemon: checkpoint and exit (tide restart)
	TypeOK       = "ok"       // daemon → client: request succeeded
	TypeError    = "error"    // daemon → client: request failed, see Err
	TypeSessions = "sessions" // daemon → client: ls reply
	TypeKilled   = "killed"   // daemon → attached clients: session ended

	// Stream frames; they flow on an attached connection and carry no Seq.
	TypeInput    = "input"    // client → daemon: raw terminal input bytes (keys, mouse, paste)
	TypeResize   = "resize"   // client → daemon: the client terminal is Cols x Rows
	TypeRender   = "render"   // daemon → attached clients: composed screen bytes to write verbatim
	TypeDetached = "detached" // daemon → one client: detached via UI ('-' button, Ctrl+Shift+E)
	TypeDropped  = "dropped"  // daemon → one client: evicted for not keeping up, see Err
	TypeCopy     = "copy"     // daemon → one client: copied text (Data) for the native clipboard, see Target
)

// Copy targets: which system selection a TypeCopy frame feeds. They mirror
// the OSC 52 targets, which remain on the render stream for terminals that
// honor them; TypeCopy exists because many (Terminal.app, older VTE) don't.
const (
	CopyClipboard = "clipboard"
	CopyPrimary   = "primary"
)

// Message is the single envelope for every frame; Type selects which fields
// are meaningful.
type Message struct {
	Type            string        `json:"type"`
	Seq             int64         `json:"seq,omitempty"` // request id, echoed in the reply
	BinaryVersion   string        `json:"binary_version,omitempty"`
	ProtocolVersion int           `json:"protocol_version,omitempty"`
	Root            string        `json:"root,omitempty"`
	Err             string        `json:"err,omitempty"`
	Session         *SessionInfo  `json:"session,omitempty"`
	Sessions        []SessionInfo `json:"sessions,omitempty"`
	Data            []byte        `json:"data,omitempty"`   // input/output bytes, attach snapshot, copied text
	Target          string        `json:"target,omitempty"` // copy: CopyClipboard or CopyPrimary
	Cols            int           `json:"cols,omitempty"`
	Rows            int           `json:"rows,omitempty"`
	ExitStatus      int           `json:"exit_status,omitempty"`
}

// SessionInfo is the client-visible view of a session (spec: `tide ls`
// lists path, panes, since-when).
type SessionInfo struct {
	Root      string    `json:"root"`
	CreatedAt time.Time `json:"created_at"`
	Clients   int       `json:"clients"`
	Panes     int       `json:"panes,omitempty"`
}

// Conn frames Messages over a net.Conn. Sends are serialized so the daemon
// can broadcast to a connection from several goroutines.
type Conn struct {
	conn net.Conn
	dec  *json.Decoder

	mu  sync.Mutex
	enc *json.Encoder
}

func NewConn(c net.Conn) *Conn {
	return &Conn{conn: c, dec: json.NewDecoder(c), enc: json.NewEncoder(c)}
}

func (c *Conn) Send(m Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.enc.Encode(m)
}

func (c *Conn) Recv() (Message, error) {
	var m Message
	err := c.dec.Decode(&m)
	return m, err
}

func (c *Conn) Close() error { return c.conn.Close() }

// SetDeadline bounds reads and writes on the underlying connection; the
// zero time clears it. Clients use it so a wedged daemon cannot hang a
// command forever.
func (c *Conn) SetDeadline(t time.Time) error { return c.conn.SetDeadline(t) }

// Hello is the first frame both sides send on every connection.
func Hello() Message {
	return Message{Type: TypeHello, BinaryVersion: version.Binary, ProtocolVersion: version.Protocol}
}

// MismatchError reports a protocol-version mismatch. Per the ratified
// ruling nothing is killed implicitly: the user is pointed at
// `tide restart`, which warns before shutting sessions down.
type MismatchError struct {
	PeerBinary   string
	PeerProtocol int
}

func (e *MismatchError) Error() string {
	return fmt.Sprintf(
		"protocol mismatch: peer %s speaks protocol %d, this binary speaks %d — run 'tide restart'",
		e.PeerBinary, e.PeerProtocol, version.Protocol)
}

// ServerHandshake (daemon side) sends our hello first, then reads and
// verifies the client's. The fixed write-then-read order on the server and
// read-then-write order on the client keeps the exchange deadlock-free on
// any transport.
func (c *Conn) ServerHandshake() (Message, error) {
	if err := c.Send(Hello()); err != nil {
		return Message{}, err
	}
	return c.recvHello()
}

// ClientHandshake reads the daemon's hello, verifies it, then sends ours.
// On mismatch nothing is sent: the daemon just sees the connection close.
func (c *Conn) ClientHandshake() (Message, error) {
	peer, err := c.recvHello()
	if err != nil {
		return peer, err
	}
	if err := c.Send(Hello()); err != nil {
		return peer, err
	}
	return peer, nil
}

func (c *Conn) recvHello() (Message, error) {
	m, err := c.Recv()
	if err != nil {
		return Message{}, err
	}
	if m.Type != TypeHello {
		return m, fmt.Errorf("expected hello, got %q", m.Type)
	}
	if m.ProtocolVersion != version.Protocol {
		return m, &MismatchError{PeerBinary: m.BinaryVersion, PeerProtocol: m.ProtocolVersion}
	}
	return m, nil
}
