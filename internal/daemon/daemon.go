// Package daemon implements the tide session daemon — the single process
// that owns all session state and survives any client death (spec: core
// foundation §1; central daemon, tmux-style).
package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/calper-ql/tide/internal/layout"
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
	logf      *log.Logger
	registry  *session.Registry
	socket    string
	prefsPath string

	// The active theme is read by every workspace's render under its own
	// lock; an atomic pointer keeps theme switches free of any daemon/ws
	// lock nesting. nil means the default (tests build bare daemons).
	theme   atomic.Pointer[theme]
	prefsMu sync.Mutex // serializes prefs.json writers (rapid picker clicks)

	mu       sync.Mutex
	sessions map[string]*ws // root → live workspace
	closing  bool           // shutdown started; no new workspaces

	shutdown chan struct{}
	once     sync.Once
}

// themeNow returns the active theme, defaulting when none was ever set.
func (d *daemon) themeNow() *theme {
	if t := d.theme.Load(); t != nil {
		return t
	}
	return &themes[0]
}

// persistTheme checkpoints the active theme choice and repaints every
// session so all attached clients see it at once. It runs on its own
// goroutine (the picker's run closure holds a workspace lock): workspaces
// are collected under the daemon lock, then poked under their own — never
// nested, same discipline as killFromUI.
func (d *daemon) persistTheme() {
	if d.prefsPath != "" {
		// Read the theme UNDER the prefs mutex: rapid picker clicks spawn
		// unordered goroutines, and re-reading inside the critical section
		// guarantees the last write holds the newest choice (a stale
		// goroutine re-persists the same newest value, never an older one).
		d.prefsMu.Lock()
		t := d.themeNow()
		if err := session.SavePrefs(d.prefsPath, session.Prefs{Theme: strings.ToLower(t.name)}); err != nil {
			d.logf.Printf("theme prefs checkpoint failed: %v", err)
		}
		d.prefsMu.Unlock()
	}
	d.mu.Lock()
	list := make([]*ws, 0, len(d.sessions))
	for _, w := range d.sessions {
		list = append(list, w)
	}
	d.mu.Unlock()
	for _, w := range list {
		w.mu.Lock()
		w.markAllDirtyLocked()
		w.mu.Unlock()
	}
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
	} else {
		// Sweep stray pane-content files (crash between a structural
		// change and its cleanup); never after a quarantine, which still
		// references them.
		var ids []string
		for _, s := range registry.List() {
			var l layout.Layout
			if len(s.Layout) > 0 && json.Unmarshal(s.Layout, &l) == nil {
				ids = append(ids, l.PaneIDs()...)
			}
		}
		registry.SweepPaneFiles(ids)
	}

	d := &daemon{
		logf:      logf,
		registry:  registry,
		socket:    sock,
		prefsPath: session.PrefsPath(opts.StatePath),
		sessions:  map[string]*ws{},
		shutdown:  make(chan struct{}),
	}
	if p := session.LoadPrefs(d.prefsPath); p.Theme != "" {
		t, known := themeByName(p.Theme)
		if !known {
			logf.Printf("WARNING: prefs name an unknown theme %q; using %s", p.Theme, t.name)
		}
		d.theme.Store(&t)
	}
	logf.Printf("daemon up: socket=%s sessions=%d theme=%s", sock, registry.Len(), d.themeNow().name)

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
				d.shutdownSessions()
				logf.Printf("daemon down: explicit shutdown, sessions checkpointed")
				return nil
			default:
			}
			// Transient accept errors (EMFILE under fd pressure) must not
			// take every session's shell down with them.
			failures++
			if failures > 100 {
				d.shutdownSessions()
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

// shutdownSessions checkpoints every workspace and hangs up its shells.
// Restart's warning ("sessions will be shut down") is about exactly this:
// processes die, content survives to the checkpoint. The closing flag keeps
// racing attaches from spawning orphan shells.
func (d *daemon) shutdownSessions() {
	d.mu.Lock()
	d.closing = true
	list := make([]*ws, 0, len(d.sessions))
	for _, w := range d.sessions {
		list = append(list, w)
	}
	d.mu.Unlock()
	for _, w := range list {
		w.takeClients() // writers stop; conns die with the process
		w.teardown()
	}
	d.mu.Lock()
	d.sessions = map[string]*ws{}
	d.mu.Unlock()
}

// killFromUI is the workspace's kill path (context-menu "Kill Session",
// confirmed). It runs on its own goroutine, never under a workspace lock.
func (d *daemon) killFromUI(root string) {
	killed, w, last, err := d.killSession(root)
	if err != nil || !killed {
		return
	}
	if w != nil {
		w.markKilled()
		for _, conn := range w.takeClients() {
			_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
			_ = conn.Send(protocol.Message{Type: protocol.TypeKilled, Root: root})
			_ = conn.Close()
		}
		w.teardown()
	}
	d.logf.Printf("kill root=%s (ui)", root)
	if last {
		d.logf.Printf("last session ended; daemon exits (ruled 2026-06-10)")
		d.stop()
	}
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
	var attachedWS *ws
	defer func() {
		// A vanished client — closed terminal included — is a detach,
		// never a session end (prime rule).
		if attachedWS != nil {
			attachedWS.removeClient(c)
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
			if attachedWS != nil {
				_ = c.Send(protocol.Message{Type: protocol.TypeError, Seq: m.Seq, Err: "already attached to " + attachedRoot})
				continue
			}
			cols, rows := m.Cols, m.Rows
			if cols < 1 || rows < 1 {
				cols, rows = 80, 24
			}
			w, clients, err := d.attachSession(c, m.Root, cols, rows, m.Seq)
			if err != nil {
				_ = c.Send(protocol.Message{Type: protocol.TypeError, Seq: m.Seq, Err: err.Error()})
				continue
			}
			// The ok reply is already in the client's outbox (first frame,
			// enqueued under the workspace lock) — sending it here instead
			// would let a render frame race ahead of it on the wire.
			attachedRoot, attachedWS = m.Root, w
			d.logf.Printf("attach root=%s clients=%d size=%dx%d", m.Root, clients, cols, rows)

		case protocol.TypeInput:
			if attachedWS != nil {
				attachedWS.handleInput(c, m.Data)
			}

		case protocol.TypeResize:
			if attachedWS != nil {
				attachedWS.resizeClient(m.Cols, m.Rows)
			}

		case protocol.TypeLs:
			_ = c.Send(protocol.Message{Type: protocol.TypeSessions, Seq: m.Seq, Sessions: d.list()})

		case protocol.TypeKill:
			if err := validRoot(m.Root); err != nil {
				_ = c.Send(protocol.Message{Type: protocol.TypeError, Seq: m.Seq, Err: err.Error()})
				continue
			}
			killed, w, last, err := d.killSession(m.Root)
			if err != nil {
				_ = c.Send(protocol.Message{Type: protocol.TypeError, Seq: m.Seq, Err: err.Error()})
				continue
			}
			if !killed {
				_ = c.Send(protocol.Message{Type: protocol.TypeError, Seq: m.Seq, Err: "no session for " + m.Root})
				continue
			}
			if w != nil {
				w.markKilled()
				for _, conn := range w.takeClients() {
					if conn == c {
						continue // the requester gets its ok below, not a hangup
					}
					// Deadline-bounded: a non-reading client must not block
					// the kill (and with it the shell teardown) forever.
					_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
					_ = conn.Send(protocol.Message{Type: protocol.TypeKilled, Root: m.Root})
					_ = conn.Close()
				}
				w.teardown()
			}
			if attachedRoot == m.Root {
				attachedRoot, attachedWS = "", nil
			}
			d.logf.Printf("kill root=%s", m.Root)
			_ = c.Send(protocol.Message{Type: protocol.TypeOK, Seq: m.Seq})
			if last {
				d.logf.Printf("last session ended; daemon exits (ruled 2026-06-10)")
				d.stop()
				return
			}

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

// attachSession ensures the session and its workspace, then registers the
// client — one critical section, atomic with killSession, so a conn can
// never join a session mid-kill. (registry's and workspace mutexes nest
// inside d.mu and are never held across it.) The attach reply enters the
// client's outbox inside ws.attach.
func (d *daemon) attachSession(conn *protocol.Conn, root string, cols, rows int, seq int64) (*ws, int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closing {
		return nil, 0, errors.New("daemon is shutting down")
	}
	s, created, err := d.registry.Ensure(root)
	if err != nil {
		return nil, 0, err
	}
	w := d.sessions[root]
	if w == nil {
		stored, _ := d.registry.Get(root)
		w, err = newWS(d, root, stored, cols, rows)
		if err != nil {
			if created {
				// The session was created for this attach; do not leave a
				// shell-less husk behind.
				_, _ = d.registry.Kill(root, nil)
			}
			return nil, 0, err
		}
		d.sessions[root] = w
	}
	clients, err := w.attach(conn, cols, rows, func(firstFrame []byte, clients, panes int) protocol.Message {
		return protocol.Message{Type: protocol.TypeOK, Seq: seq, Data: firstFrame, Session: &protocol.SessionInfo{
			Root: s.Root, CreatedAt: s.CreatedAt, Clients: clients, Panes: panes,
		}}
	})
	if err != nil {
		return nil, 0, err
	}
	return w, clients, nil
}

// killSession removes the session and claims its workspace in the same
// critical section; the caller owns notifying clients and tearing the
// workspace down. last reports whether this was the final session — per
// the ratified ruling the daemon exits with it (closing is set here, under
// the lock, so no attach can slip in between).
func (d *daemon) killSession(root string) (killed bool, w *ws, last bool, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	w = d.sessions[root]
	var paneIDs []string
	if w != nil {
		paneIDs = w.paneIDs()
	} else if stored, ok := d.registry.Get(root); ok && len(stored.Layout) > 0 {
		var l layout.Layout
		if json.Unmarshal(stored.Layout, &l) == nil {
			paneIDs = l.PaneIDs()
		}
	}
	killed, err = d.registry.Kill(root, paneIDs)
	if err != nil || !killed {
		return killed, nil, false, err
	}
	delete(d.sessions, root)
	if d.registry.Len() == 0 && !d.closing {
		d.closing = true
		last = true
	}
	return true, w, last, nil
}

func (d *daemon) list() []protocol.SessionInfo {
	sessions := d.registry.List()
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]protocol.SessionInfo, 0, len(sessions))
	for _, s := range sessions {
		clients, panes := 0, 0
		if w := d.sessions[s.Root]; w != nil {
			clients = w.clientCount()
			panes = w.paneCount()
		} else if len(s.Layout) > 0 {
			var l layout.Layout
			if json.Unmarshal(s.Layout, &l) == nil {
				panes = l.CountPanes()
			}
		}
		out = append(out, protocol.SessionInfo{
			Root: s.Root, CreatedAt: s.CreatedAt, Clients: clients, Panes: panes,
		})
	}
	return out
}
