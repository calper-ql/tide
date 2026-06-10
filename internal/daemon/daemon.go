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
	"path/filepath"
	"sync"
	"syscall"
	"time"

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
	socket   string

	mu      sync.Mutex
	panes   map[string]*pane // root → live pane
	closing bool             // shutdownPanes started; no new panes

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
		socket:   sock,
		panes:    map[string]*pane{},
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

	failures := 0
	for {
		conn, err := l.Accept()
		if err != nil {
			select {
			case <-d.shutdown:
				d.shutdownPanes()
				logf.Printf("daemon down: explicit shutdown, sessions checkpointed")
				return nil
			default:
			}
			// Transient accept errors (EMFILE under fd pressure) must not
			// take every session's shell down with them.
			failures++
			if failures > 100 {
				d.shutdownPanes()
				return fmt.Errorf("accept keeps failing: %w", err)
			}
			logf.Printf("accept error (%d/100): %v", failures, err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		failures = 0
		go d.serve(protocol.NewConn(conn))
	}
}

func (d *daemon) stop() { d.once.Do(func() { close(d.shutdown) }) }

// saveCheckpoint binds pane checkpoints to the registry. A pane that is no
// longer current — killed, or replaced after a kill+recreate — must not
// write: its dying checkpoint would resurrect content the user explicitly
// ended. Failures are logged, never fatal; the next debounce retries.
func (d *daemon) saveCheckpoint(root, uuid string, cols, rows int, snapshot []byte, history [][]byte) {
	d.mu.Lock()
	p := d.panes[root]
	current := p != nil && p.uuid == uuid
	d.mu.Unlock()
	if !current {
		return
	}
	if err := d.registry.UpdatePane(root, uuid, cols, rows, snapshot, history); err != nil {
		d.logf.Printf("checkpoint failed root=%s: %v", root, err)
	}
}

// shutdownPanes checkpoints every pane and hangs up its shell. Restart's
// warning ("sessions will be shut down") is about exactly this: processes
// die, content survives to the checkpoint. The panes map stays intact while
// the final checkpoints run (saveCheckpoint requires currency) and the
// closing flag keeps racing attaches from spawning orphan shells.
func (d *daemon) shutdownPanes() {
	d.mu.Lock()
	d.closing = true
	panes := make([]*pane, 0, len(d.panes))
	for _, p := range d.panes {
		panes = append(panes, p)
	}
	d.mu.Unlock()
	for _, p := range panes {
		p.takeClients() // writers stop; conns die with the process
		p.shutdown()
	}
	d.mu.Lock()
	d.panes = map[string]*pane{}
	d.mu.Unlock()
}

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
	var attachedPane *pane
	defer func() {
		// A vanished client — closed terminal included — is a detach,
		// never a session end (prime rule).
		if attachedPane != nil {
			attachedPane.removeClient(c)
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
			if err := validRoot(m.Root); err != nil {
				_ = c.Send(protocol.Message{Type: protocol.TypeError, Seq: m.Seq, Err: err.Error()})
				continue
			}
			if attachedPane != nil {
				_ = c.Send(protocol.Message{Type: protocol.TypeError, Seq: m.Seq, Err: "already attached to " + attachedRoot})
				continue
			}
			cols, rows := m.Cols, m.Rows
			if cols < 1 || rows < 1 {
				cols, rows = 80, 24
			}
			p, clients, err := d.attachSession(c, m.Root, cols, rows, m.Seq)
			if err != nil {
				_ = c.Send(protocol.Message{Type: protocol.TypeError, Seq: m.Seq, Err: err.Error()})
				continue
			}
			// The ok reply is already in the client's outbox (first frame,
			// enqueued under the pane lock) — sending it here instead would
			// let an output frame race ahead of it on the wire.
			attachedRoot, attachedPane = m.Root, p
			d.logf.Printf("attach root=%s clients=%d size=%dx%d", m.Root, clients, cols, rows)

		case protocol.TypeInput:
			if attachedPane != nil {
				attachedPane.input(m.Data)
			}

		case protocol.TypeResize:
			if attachedPane != nil {
				attachedPane.resize(m.Cols, m.Rows)
			}

		case protocol.TypeLs:
			_ = c.Send(protocol.Message{Type: protocol.TypeSessions, Seq: m.Seq, Sessions: d.list()})

		case protocol.TypeKill:
			if err := validRoot(m.Root); err != nil {
				_ = c.Send(protocol.Message{Type: protocol.TypeError, Seq: m.Seq, Err: err.Error()})
				continue
			}
			killed, p, err := d.killSession(m.Root)
			if err != nil {
				_ = c.Send(protocol.Message{Type: protocol.TypeError, Seq: m.Seq, Err: err.Error()})
				continue
			}
			if !killed {
				_ = c.Send(protocol.Message{Type: protocol.TypeError, Seq: m.Seq, Err: "no session for " + m.Root})
				continue
			}
			if p != nil {
				for _, conn := range p.takeClients() {
					if conn == c {
						continue // the requester gets its ok below, not a hangup
					}
					// Deadline-bounded: a non-reading client must not block
					// the kill (and with it the shell teardown) forever.
					_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
					_ = conn.Send(protocol.Message{Type: protocol.TypeKilled, Root: m.Root})
					_ = conn.Close()
				}
				p.shutdown()
			}
			if attachedRoot == m.Root {
				attachedRoot, attachedPane = "", nil
			}
			d.logf.Printf("kill root=%s", m.Root)
			_ = c.Send(protocol.Message{Type: protocol.TypeOK, Seq: m.Seq})

		case protocol.TypeShutdown:
			_ = c.Send(protocol.Message{Type: protocol.TypeOK, Seq: m.Seq})
			d.logf.Printf("shutdown requested")
			d.stop()
			return

		default:
			_ = c.Send(protocol.Message{Type: protocol.TypeError, Seq: m.Seq, Err: "unknown message type " + m.Type})
		}
	}
}

// validRoot guards the protocol boundary: session identities are absolute,
// clean paths, never interpreted relative to the daemon's own cwd.
func validRoot(root string) error {
	if root == "" {
		return errors.New("a root path is required")
	}
	if !filepath.IsAbs(root) || root != filepath.Clean(root) {
		return fmt.Errorf("root must be an absolute clean path, got %q", root)
	}
	return nil
}

// attachSession ensures the session and its pane, then registers the client
// — one critical section, atomic with killSession, so a conn can never join
// a session mid-kill. (registry's and pane's own mutexes nest inside d.mu
// and are never held across it.) The attach reply enters the client's
// outbox inside pane.attachClient.
func (d *daemon) attachSession(conn *protocol.Conn, root string, cols, rows int, seq int64) (*pane, int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closing {
		return nil, 0, errors.New("daemon is shutting down")
	}
	s, created, err := d.registry.Ensure(root)
	if err != nil {
		return nil, 0, err
	}
	p := d.panes[root]
	if p == nil {
		stored, _ := d.registry.Get(root)
		p, err = newPane(stored, cols, rows, d.socket, d.logf, d.saveCheckpoint)
		if err != nil {
			if created {
				// The session was created for this attach; do not leave a
				// shell-less husk behind.
				_, _ = d.registry.Kill(root)
			}
			return nil, 0, err
		}
		d.panes[root] = p
	}
	clients, err := p.attachClient(conn, cols, rows, d.socket, func(snapshot []byte, clients int) protocol.Message {
		return protocol.Message{Type: protocol.TypeOK, Seq: seq, Data: snapshot, Session: &protocol.SessionInfo{
			Root: s.Root, CreatedAt: s.CreatedAt, Clients: clients,
		}}
	})
	if err != nil {
		return nil, 0, err
	}
	return p, clients, nil
}

// killSession removes the session and claims its pane in the same critical
// section; the caller owns notifying clients and stopping the shell.
func (d *daemon) killSession(root string) (killed bool, p *pane, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	killed, err = d.registry.Kill(root)
	if err != nil || !killed {
		return killed, nil, err
	}
	p = d.panes[root]
	delete(d.panes, root)
	return true, p, nil
}

func (d *daemon) list() []protocol.SessionInfo {
	sessions := d.registry.List()
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]protocol.SessionInfo, 0, len(sessions))
	for _, s := range sessions {
		clients := 0
		if p := d.panes[s.Root]; p != nil {
			clients = p.clientCount()
		}
		out = append(out, protocol.SessionInfo{
			Root: s.Root, CreatedAt: s.CreatedAt, Clients: clients,
		})
	}
	return out
}
