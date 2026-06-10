// Package client is the attach side: dial the daemon socket, spawning the
// daemon on demand (spec: on-demand spawn, single binary), then speak
// protocol.
package client

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/calper-ql/tide/internal/paths"
	"github.com/calper-ql/tide/internal/protocol"
)

// handshakeTimeout bounds every request/reply exchange and the hello, so a
// wedged daemon (SIGSTOPped, deadlocked) cannot hang a tide command forever.
const handshakeTimeout = 5 * time.Second

// Dial connects and handshakes; it never spawns a daemon.
func Dial(runtimeDir string) (*protocol.Conn, error) {
	nc, err := net.DialTimeout("unix", paths.SocketPath(runtimeDir), time.Second)
	if err != nil {
		return nil, err
	}
	c := protocol.NewConn(nc)
	_ = c.SetDeadline(time.Now().Add(handshakeTimeout))
	if _, err := c.ClientHandshake(); err != nil {
		c.Close()
		return nil, err
	}
	_ = c.SetDeadline(time.Time{})
	return c, nil
}

// EnsureDaemon dials, re-execing this binary as a detached daemon when no
// one is listening. Spawning retries inside the loop: the first spawn can
// lose the flock to a daemon that is still dying (e.g. mid-restart), and
// spawning is idempotent — losers yield cheaply.
func EnsureDaemon(runtimeDir string) (*protocol.Conn, error) {
	c, err := Dial(runtimeDir)
	if err == nil {
		return c, nil
	}
	var mm *protocol.MismatchError
	if errors.As(err, &mm) {
		return nil, err // a daemon is alive, just incompatible — don't race it
	}
	deadline := time.Now().Add(5 * time.Second)
	var nextSpawn time.Time
	for {
		if !time.Now().Before(nextSpawn) {
			if err := SpawnDaemon(); err != nil {
				return nil, fmt.Errorf("spawning daemon: %w", err)
			}
			nextSpawn = time.Now().Add(time.Second)
		}
		c, err = Dial(runtimeDir)
		if err == nil {
			return c, nil
		}
		if errors.As(err, &mm) {
			return nil, err
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("daemon did not come up (check %s): %w", paths.LogPath(runtimeDir), err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// SpawnDaemon re-execs this binary as `tide --daemon` in its own session,
// fully detached from the invoking terminal. The environment passes
// through, so TIDE_RUNTIME_DIR/TIDE_STATE_DIR overrides reach the daemon.
func SpawnDaemon() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, "--daemon")
	cmd.Dir = "/" // never pin the invoking client's cwd for the daemon's lifetime
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	// stdio defaults to /dev/null; the daemon logs into its runtime dir.
	if err := cmd.Start(); err != nil {
		return err
	}
	// Reap the daemon if it ever exits while this client is still alive; if
	// the client exits first, init reaps. No process-global signal state —
	// signal.Ignore(SIGCHLD) would silently break every future cmd.Wait in
	// this process.
	go func() { _ = cmd.Wait() }()
	return nil
}

// Attach joins (or creates) root's session on an established connection,
// reporting the client terminal size. The returned snapshot, written to the
// client terminal verbatim, recreates the pane exactly; after it, the
// connection is a stream (input/resize out, output/exit/killed in).
func Attach(c *protocol.Conn, root string, cols, rows int) (protocol.SessionInfo, []byte, error) {
	m, err := rpc(c, protocol.Message{Type: protocol.TypeAttach, Root: root, Cols: cols, Rows: rows})
	if err != nil {
		return protocol.SessionInfo{}, nil, err
	}
	if m.Session == nil {
		return protocol.SessionInfo{}, nil, errors.New("daemon sent ok without session info")
	}
	return *m.Session, m.Data, nil
}

// SendInput forwards keyboard bytes to the attached pane (fire-and-forget).
func SendInput(c *protocol.Conn, data []byte) error {
	return c.Send(protocol.Message{Type: protocol.TypeInput, Data: data})
}

// SendResize reports a new client terminal size (fire-and-forget).
func SendResize(c *protocol.Conn, cols, rows int) error {
	return c.Send(protocol.Message{Type: protocol.TypeResize, Cols: cols, Rows: rows})
}

// Ls lists live sessions.
func Ls(c *protocol.Conn) ([]protocol.SessionInfo, error) {
	m, err := rpc(c, protocol.Message{Type: protocol.TypeLs})
	if err != nil {
		return nil, err
	}
	return m.Sessions, nil
}

// Kill explicitly ends root's session — the only way a session ends.
func Kill(c *protocol.Conn, root string) error {
	_, err := rpc(c, protocol.Message{Type: protocol.TypeKill, Root: root})
	return err
}

// Shutdown asks the daemon to checkpoint and exit (`tide restart`).
func Shutdown(c *protocol.Conn) error {
	_, err := rpc(c, protocol.Message{Type: protocol.TypeShutdown})
	return err
}

var rpcSeq atomic.Int64

// rpc is one deadline-bounded request/reply exchange. The deadline is
// cleared on success so an attach connection can live on as a stream.
func rpc(c *protocol.Conn, req protocol.Message) (protocol.Message, error) {
	req.Seq = rpcSeq.Add(1)
	_ = c.SetDeadline(time.Now().Add(handshakeTimeout))
	if err := c.Send(req); err != nil {
		return protocol.Message{}, err
	}
	m, err := c.Recv()
	if err != nil {
		return protocol.Message{}, err
	}
	_ = c.SetDeadline(time.Time{})
	switch m.Type {
	case protocol.TypeOK, protocol.TypeSessions:
		return m, nil
	case protocol.TypeError:
		return m, errors.New(m.Err)
	case protocol.TypeKilled:
		// A concurrent `tide kill` can race our reply onto the wire; that
		// is an answer, not a protocol violation.
		return m, errors.New("session was killed while this request was in flight")
	default:
		return m, fmt.Errorf("unexpected reply %q", m.Type)
	}
}
