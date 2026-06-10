// Package daemon implements the tide session daemon — the single process
// that owns all session state and survives any client death (spec: core
// foundation §1; central daemon, tmux-style).
package daemon

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/calper-ql/tide/internal/paths"
	"github.com/calper-ql/tide/internal/protocol"
	"github.com/calper-ql/tide/internal/session"
)

// Options configure a daemon run; tests inject private directories.
type Options struct {
	RuntimeDir string
	StatePath  string
	Log        io.Writer
}

type daemon struct {
	logf     *log.Logger
	registry *session.Registry

	mu       sync.Mutex
	attached map[string]map[*protocol.Conn]struct{} // root → live client conns

	shutdown chan struct{}
	once     sync.Once
}

// lockExclusive takes a non-blocking exclusive flock on path, then verifies
// the path still names the locked inode — flock binds to the inode, and a
// tmp cleaner can unlink a lock file out from under its holder, which would
// otherwise let two daemons serve at once. won=false means a live holder
// exists.
func lockExclusive(path string) (f *os.File, won bool, err error) {
	for range 5 {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return nil, false, err
		}
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			f.Close()
			if errors.Is(err, syscall.EWOULDBLOCK) {
				return nil, false, nil
			}
			return nil, false, fmt.Errorf("locking %s: %w", path, err)
		}
		pathInfo, perr := os.Stat(path)
		fileInfo, ferr := f.Stat()
		if perr == nil && ferr == nil && os.SameFile(pathInfo, fileInfo) {
			return f, true, nil
		}
		f.Close() // unlinked or replaced under us; take the new inode instead
	}
	return nil, false, fmt.Errorf("lock file %s keeps changing", path)
}

// Run serves until an explicit shutdown (protocol request or SIGTERM).
// Spawn races are settled by an exclusive flock on the lock file: the
// winner clears any stale socket and binds; a loser returns nil immediately
// so the surviving daemon serves the client that spawned both
// (spec: on-demand spawn, first-to-bind wins).
func Run(opts Options) error {
	logf := log.New(opts.Log, "", log.LstdFlags)

	lockFile, won, err := lockExclusive(paths.LockPath(opts.RuntimeDir))
	if err != nil {
		return err
	}
	if !won {
		return nil // another daemon already owns this runtime dir
	}
	defer lockFile.Close()
	// The pid lets `tide restart` signal a daemon it cannot talk to.
	if err := lockFile.Truncate(0); err == nil {
		fmt.Fprintf(lockFile, "%d\n", os.Getpid())
	}

	// A second, independent lock on the checkpoint file: runtime dirs can
	// diverge (env overrides, container vs login), and two daemons sharing
	// one state file would silently clobber each other's checkpoints.
	stateLock, won, err := lockExclusive(opts.StatePath + ".lock")
	if err != nil {
		return err
	}
	if !won {
		return fmt.Errorf("state file %s is owned by another daemon (split runtime dirs?)", opts.StatePath)
	}
	defer stateLock.Close()

	// We hold the locks, so any existing socket file is stale.
	sock := paths.SocketPath(opts.RuntimeDir)
	if err := os.Remove(sock); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	l, err := net.Listen("unix", sock)
	if err != nil {
		return err
	}
	defer l.Close() // also unlinks the socket file
	if err := os.Chmod(sock, 0o600); err != nil {
		return err
	}

	registry := session.NewRegistry(opts.StatePath)
	quarantined, err := registry.Load()
	if err != nil {
		return err
	}
	if quarantined != "" {
		logf.Printf("WARNING: state file was unreadable; quarantined to %s; starting with an empty registry", quarantined)
	}

	d := &daemon{
		logf:     logf,
		registry: registry,
		attached: map[string]map[*protocol.Conn]struct{}{},
		shutdown: make(chan struct{}),
	}
	logf.Printf("daemon up: socket=%s sessions=%d", sock, registry.Len())

	// SIGTERM is the version-independent shutdown path: `tide restart`
	// uses it when the running daemon speaks a different protocol.
	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(sigc)
	go func() {
		select {
		case <-sigc:
			d.stop()
		case <-d.shutdown:
		}
	}()

	go func() {
		<-d.shutdown
		l.Close() // unblocks Accept
	}()

	for {
		conn, err := l.Accept()
		if err != nil {
			select {
			case <-d.shutdown:
				logf.Printf("daemon down: explicit shutdown, sessions checkpointed")
				return nil
			default:
				return err
			}
		}
		go d.serve(protocol.NewConn(conn))
	}
}

func (d *daemon) stop() { d.once.Do(func() { close(d.shutdown) }) }

func (d *daemon) serve(c *protocol.Conn) {
	defer c.Close()

	if _, err := c.ServerHandshake(); err != nil {
		var mm *protocol.MismatchError
		if errors.As(err, &mm) {
			// Never kill anything implicitly; tell the client what to run.
			_ = c.Send(protocol.Message{Type: protocol.TypeError, Err: mm.Error()})
		}
		d.logf.Printf("handshake refused: %v", err)
		return
	}

	var attachedRoot string
	defer func() {
		// A vanished client — closed terminal included — is a detach,
		// never a session end (prime rule).
		if attachedRoot != "" {
			d.detach(attachedRoot, c)
			d.logf.Printf("detach root=%s", attachedRoot)
		}
	}()

	for {
		m, err := c.Recv()
		if err != nil {
			return
		}
		switch m.Type {
		case protocol.TypeAttach:
			if m.Root == "" {
				_ = c.Send(protocol.Message{Type: protocol.TypeError, Err: "attach requires a root"})
				continue
			}
			if attachedRoot != "" {
				_ = c.Send(protocol.Message{Type: protocol.TypeError, Err: "already attached to " + attachedRoot})
				continue
			}
			s, created, clients, err := d.attachSession(m.Root, c)
			if err != nil {
				_ = c.Send(protocol.Message{Type: protocol.TypeError, Err: err.Error()})
				continue
			}
			attachedRoot = m.Root
			d.logf.Printf("attach root=%s clients=%d created=%v", m.Root, clients, created)
			_ = c.Send(protocol.Message{Type: protocol.TypeOK, Session: &protocol.SessionInfo{
				Root: s.Root, CreatedAt: s.CreatedAt, Clients: clients,
			}})

		case protocol.TypeLs:
			_ = c.Send(protocol.Message{Type: protocol.TypeSessions, Sessions: d.list()})

		case protocol.TypeKill:
			if m.Root == "" {
				_ = c.Send(protocol.Message{Type: protocol.TypeError, Err: "kill requires a root"})
				continue
			}
			killed, conns, err := d.killSession(m.Root)
			if err != nil {
				_ = c.Send(protocol.Message{Type: protocol.TypeError, Err: err.Error()})
				continue
			}
			if !killed {
				_ = c.Send(protocol.Message{Type: protocol.TypeError, Err: "no session for " + m.Root})
				continue
			}
			for conn := range conns {
				if conn == c {
					continue // the requester gets its ok below, not a hangup
				}
				_ = conn.Send(protocol.Message{Type: protocol.TypeKilled, Root: m.Root})
				_ = conn.Close()
			}
			if attachedRoot == m.Root {
				attachedRoot = ""
			}
			d.logf.Printf("kill root=%s", m.Root)
			_ = c.Send(protocol.Message{Type: protocol.TypeOK})

		case protocol.TypeShutdown:
			_ = c.Send(protocol.Message{Type: protocol.TypeOK})
			d.logf.Printf("shutdown requested")
			d.stop()
			return

		default:
			_ = c.Send(protocol.Message{Type: protocol.TypeError, Err: "unknown message type " + m.Type})
		}
	}
}

// attachSession ensures the session and registers the client in one
// critical section. Atomicity with killSession matters: split in two, a
// conn could join a session mid-kill and sit attached to nothing, never
// receiving TypeKilled. (registry's own mutex nests inside d.mu and is
// never held across it.)
func (d *daemon) attachSession(root string, c *protocol.Conn) (s session.Session, created bool, clients int, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	s, created, err = d.registry.Ensure(root)
	if err != nil {
		return session.Session{}, false, 0, err
	}
	set, ok := d.attached[root]
	if !ok {
		set = map[*protocol.Conn]struct{}{}
		d.attached[root] = set
	}
	set[c] = struct{}{}
	return s, created, len(set), nil
}

// killSession removes the session and claims its attached conns in the
// same critical section; the caller owns notifying and closing them.
func (d *daemon) killSession(root string) (killed bool, conns map[*protocol.Conn]struct{}, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	killed, err = d.registry.Kill(root)
	if err != nil || !killed {
		return killed, nil, err
	}
	conns = d.attached[root]
	delete(d.attached, root)
	return true, conns, nil
}

func (d *daemon) detach(root string, c *protocol.Conn) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if set, ok := d.attached[root]; ok {
		delete(set, c)
		if len(set) == 0 {
			delete(d.attached, root)
		}
	}
}

func (d *daemon) list() []protocol.SessionInfo {
	sessions := d.registry.List()
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]protocol.SessionInfo, 0, len(sessions))
	for _, s := range sessions {
		out = append(out, protocol.SessionInfo{
			Root: s.Root, CreatedAt: s.CreatedAt, Clients: len(d.attached[s.Root]),
		})
	}
	return out
}
