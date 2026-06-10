// The pane is one session's terminal: a shell on a PTY, parsed into a VT
// grid the daemon owns. Clients are spectators — they receive the raw PTY
// stream and, on attach, a snapshot that recreates the screen exactly
// (spec: core foundation §3; requirement 1, crash survival).
package daemon

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/creack/pty"

	"github.com/calper-ql/tide/internal/protocol"
	"github.com/calper-ql/tide/internal/session"
	"github.com/calper-ql/tide/internal/vt"
)

const (
	historyKeep     = 5000 // scrollback lines held in memory
	historyPersist  = 1000 // scrollback lines written per checkpoint
	historyReplay   = 2000 // scrollback lines replayed to an attaching client
	outboxSize      = 512  // frames buffered per client before it is dropped
	inboxSize       = 256  // input frames buffered before they are dropped
	answerbackSize  = 64   // VT query responses buffered toward the PTY
	checkpointDelay = time.Second
	maxDim          = 4096 // cols/rows bound; beyond this is hostile or absurd
)

// checkpointFunc persists a pane's content; the daemon binds it to the
// session registry and rejects checkpoints from panes that are no longer
// current.
type checkpointFunc func(root, uuid string, cols, rows int, snapshot []byte, history [][]byte)

type pane struct {
	root string
	uuid string
	save checkpointFunc
	logf *log.Logger

	// ptmx is read lock-free by the input/answerback writers; writing to a
	// closed PTY is harmless, blocking the pane mutex on a full PTY buffer
	// would not be.
	ptmx atomic.Pointer[os.File]

	quit       chan struct{} // closed once, at shutdown
	inputQ     chan []byte
	answerback chan []byte

	mu      sync.Mutex
	term    *vt.Term
	cmd     *exec.Cmd
	cols    int
	rows    int
	dead    bool // shell exited; respawned on the next attach
	closing bool
	clients map[*protocol.Conn]chan protocol.Message

	cpMu      sync.Mutex
	cpTimer   *time.Timer
	cpClosing bool
	cpSaveMu  sync.Mutex // spans snapshot capture + save; see checkpointNow

	shutdownOnce sync.Once
}

func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand failure is not a recoverable condition
	}
	return hex.EncodeToString(b[:])
}

func clampDim(v, fallback int) int {
	if v < 1 || v > maxDim {
		return fallback
	}
	return v
}

// newPane restores checkpointed content (if any) into a fresh VT and spawns
// the shell. socket is the daemon socket path injected as TIDE_SESSION.
func newPane(s session.Session, cols, rows int, socket string, logf *log.Logger, save checkpointFunc) (*pane, error) {
	uuid := s.PaneUUID
	if uuid == "" {
		uuid = newUUID()
	}
	cols, rows = clampDim(cols, 80), clampDim(rows, 24)
	p := &pane{
		root:       s.Root,
		uuid:       uuid,
		save:       save,
		logf:       logf,
		cols:       cols,
		rows:       rows,
		quit:       make(chan struct{}),
		inputQ:     make(chan []byte, inboxSize),
		answerback: make(chan []byte, answerbackSize),
		clients:    map[*protocol.Conn]chan protocol.Message{},
	}
	// The state file is same-user-owned, but its sizes are still inputs:
	// out-of-range values fall back to the client's size instead of
	// panicking or allocating absurd grids.
	restoreCols, restoreRows := clampDim(s.Cols, cols), clampDim(s.Rows, rows)
	p.term = vt.New(restoreCols, restoreRows, historyKeep, ptyWriter{p})
	if len(s.History) > 0 || len(s.Snapshot) > 0 {
		for _, l := range s.History {
			p.term.Write(append(l, '\r', '\n'))
		}
		for i := 0; i < restoreRows; i++ {
			p.term.Write([]byte{'\n'})
		}
		if len(s.Snapshot) > 0 {
			p.term.Write(s.Snapshot)
		}
		p.term.Write([]byte("\x1b[0m\r\n[tide] restored from checkpoint; starting a fresh shell\r\n"))
	}
	p.term.Resize(cols, rows)
	go p.inputLoop()
	go p.answerbackLoop()
	if err := p.spawnLocked(socket); err != nil {
		close(p.quit)
		return nil, err
	}
	return p, nil
}

// ptyWriter queues VT answerback (DSR/CPR responses) toward the PTY. The
// VT emits these while its own and the pane's locks are held, so the write
// must never block — a full PTY input buffer once deadlocked the whole
// daemon through this path.
type ptyWriter struct{ p *pane }

func (w ptyWriter) Write(b []byte) (int, error) {
	data := append([]byte(nil), b...)
	select {
	case w.p.answerback <- data:
	default: // a stuffed PTY loses query responses, never the daemon
	}
	return len(b), nil
}

func (p *pane) answerbackLoop() {
	for {
		select {
		case <-p.quit:
			return
		case data := <-p.answerback:
			if f := p.ptmx.Load(); f != nil {
				_, _ = f.Write(data)
			}
		}
	}
}

// inputLoop is the only writer of keyboard bytes to the PTY. A foreground
// app that stops reading blocks this goroutine only — never a serve loop.
func (p *pane) inputLoop() {
	for {
		select {
		case <-p.quit:
			return
		case data := <-p.inputQ:
			if f := p.ptmx.Load(); f != nil {
				_, _ = f.Write(data)
			}
		}
	}
}

// paneEnv builds the shell environment: the daemon's own environment minus
// stale terminal context (the daemon inherits its first client's env), with
// TERM pinned to what the pane VT actually emulates and TIDE_SESSION
// injected (spec: capability model).
func paneEnv(uuid, socket string) []string {
	drop := []string{"TIDE_SESSION=", "TMUX=", "STY=", "TERM=", "TERM_PROGRAM=", "TERM_PROGRAM_VERSION="}
	env := make([]string, 0, len(os.Environ())+2)
	for _, kv := range os.Environ() {
		skip := false
		for _, d := range drop {
			if strings.HasPrefix(kv, d) {
				skip = true
				break
			}
		}
		if !skip {
			env = append(env, kv)
		}
	}
	return append(env, "TERM=xterm-256color", "TIDE_SESSION="+uuid+":"+socket)
}

// spawnLocked starts the shell. Callers hold p.mu or have not yet published
// the pane.
func (p *pane) spawnLocked(socket string) error {
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	cmd := exec.Command(shell)
	cmd.Dir = p.root
	cmd.Env = paneEnv(p.uuid, socket)
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: uint16(p.rows), Cols: uint16(p.cols)})
	if err != nil {
		return fmt.Errorf("starting %s in %s: %w", shell, p.root, err)
	}
	if old := p.ptmx.Load(); old != nil {
		old.Close() // stop the previous readLoop before its replacement starts
	}
	p.cmd = cmd
	p.dead = false
	p.ptmx.Store(ptmx)
	go p.readLoop(ptmx)
	go p.waitShell(cmd)
	return nil
}

// readLoop pumps PTY output into the VT and to every attached client. The
// term.Write and the broadcast happen under one lock so an attach snapshot
// can never miss or double-apply bytes. Shell death is the waiter's job:
// EIO here only means every slave fd is gone (a background job can hold the
// PTY long after the shell exits, and its output still belongs on screen).
func (p *pane) readLoop(ptmx *os.File) {
	buf := make([]byte, 32*1024)
	for {
		n, err := ptmx.Read(buf)
		if n > 0 {
			data := append([]byte(nil), buf[:n]...)
			p.mu.Lock()
			p.term.Write(data)
			p.broadcastLocked(protocol.Message{Type: protocol.TypeOutput, Root: p.root, Data: data})
			p.mu.Unlock()
			p.scheduleCheckpoint()
		}
		if err != nil {
			ptmx.Close()
			return
		}
	}
}

// waitShell reaps the shell — the single Wait owner for one spawn — and
// reports its death. The dead flag is what makes later SIGHUPs safe: it is
// only ever set after Wait returns, and shutdown signals through
// os.Process, which refuses post-Wait signals, so a recycled pid can never
// be signaled.
func (p *pane) waitShell(cmd *exec.Cmd) {
	status := 0
	if err := cmd.Wait(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			status = ee.ExitCode()
		}
	}
	p.mu.Lock()
	if p.cmd != cmd || p.closing {
		p.mu.Unlock()
		return // respawned already, or shutdown owns the teardown
	}
	p.dead = true
	notice := fmt.Sprintf("\x1b[0m\r\n[tide] shell exited (status %d)\r\n", status)
	p.term.Write([]byte(notice))
	p.broadcastLocked(protocol.Message{Type: protocol.TypeOutput, Root: p.root, Data: []byte(notice)})
	p.broadcastLocked(protocol.Message{Type: protocol.TypeExit, Root: p.root, ExitStatus: status})
	p.mu.Unlock()
	p.scheduleCheckpoint()
}

// attachClient registers a client, enqueues the attach reply as the first
// frame of its outbox, and returns the client count. Routing the reply
// through the outbox — not the serve loop — is what guarantees no stream
// frame can ever precede it on the wire. Registration, snapshot, and reply
// are atomic with respect to readLoop: output after the snapshot reaches
// the client as stream frames, output before it is inside the snapshot,
// never both.
func (p *pane) attachClient(conn *protocol.Conn, cols, rows int, socket string,
	reply func(snapshot []byte, clients int) protocol.Message) (clients int, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.dead && !p.closing {
		if err := p.spawnLocked(socket); err != nil {
			return 0, err
		}
	}
	p.resizeLocked(cols, rows)
	out := make(chan protocol.Message, outboxSize)
	p.clients[conn] = out
	out <- reply(p.term.Snapshot(true, historyReplay), len(p.clients))
	go clientWriter(conn, out)
	return len(p.clients), nil
}

// clientWriter drains one client's outbox so a slow client can never stall
// the pane. The pane drops the client (closes the conn) instead of waiting.
func clientWriter(conn *protocol.Conn, out <-chan protocol.Message) {
	for m := range out {
		if conn.Send(m) != nil {
			conn.Close()
			for range out { // drain until the pane closes the channel
			}
			return
		}
	}
}

func (p *pane) broadcastLocked(m protocol.Message) {
	for conn, out := range p.clients {
		select {
		case out <- m:
		default:
			// The outbox is full: this client cannot keep up with the pane.
			// Tell it why (best effort, bounded) and drop it; silence here
			// used to masquerade as a daemon crash.
			delete(p.clients, conn)
			close(out)
			p.logf.Printf("drop slow client root=%s (outbox full)", p.root)
			go func(conn *protocol.Conn) {
				_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
				_ = conn.Send(protocol.Message{Type: protocol.TypeDropped,
					Err: "client too slow consuming pane output"})
				_ = conn.Close()
			}(conn)
		}
	}
}

func (p *pane) removeClient(conn *protocol.Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if out, ok := p.clients[conn]; ok {
		delete(p.clients, conn)
		close(out)
	}
}

// takeClients hands every attached conn to the caller (for the kill sweep)
// and stops their writers.
func (p *pane) takeClients() []*protocol.Conn {
	p.mu.Lock()
	defer p.mu.Unlock()
	conns := make([]*protocol.Conn, 0, len(p.clients))
	for conn, out := range p.clients {
		delete(p.clients, conn)
		close(out)
		conns = append(conns, conn)
	}
	return conns
}

func (p *pane) clientCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.clients)
}

// input queues keyboard bytes for the PTY. When the queue is full — the
// foreground app stopped reading — frames are dropped: dropping input beats
// wedging the client's whole connection behind a blocked PTY write.
func (p *pane) input(data []byte) {
	select {
	case p.inputQ <- data:
	default:
		p.logf.Printf("drop input root=%s (pty not consuming)", p.root)
	}
}

func (p *pane) resize(cols, rows int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.resizeLocked(cols, rows)
}

// resizeLocked applies latest-wins sizing (v0 multi-client semantics).
// Rows discarded by a shrink land in the history ring (vt extension), so a
// reattach from a smaller terminal narrows the view without losing content.
func (p *pane) resizeLocked(cols, rows int) {
	cols, rows = clampDim(cols, p.cols), clampDim(rows, p.rows)
	if cols == p.cols && rows == p.rows {
		return
	}
	p.cols, p.rows = cols, rows
	if f := p.ptmx.Load(); f != nil && !p.dead {
		_ = pty.Setsize(f, &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)})
	}
	p.term.Resize(cols, rows)
}

// shutdown checkpoints the pane and hangs up the shell; safe to call from
// both the kill path and daemon shutdown (it runs once). Closing the PTY
// master delivers the kernel's own SIGHUP to the pane's foreground process
// group; the explicit Signal covers a shell that ignores hangups, and
// cannot hit a recycled pid (see waitShell).
func (p *pane) shutdown() {
	p.shutdownOnce.Do(func() {
		p.checkpointNow()
		p.mu.Lock()
		p.closing = true
		cmd, dead := p.cmd, p.dead
		p.mu.Unlock()
		p.stopCheckpoints()
		close(p.quit)
		if cmd != nil && cmd.Process != nil && !dead {
			_ = cmd.Process.Signal(syscall.SIGHUP)
		}
		if f := p.ptmx.Load(); f != nil {
			f.Close()
		}
	})
}

// scheduleCheckpoint debounces content checkpoints: at most one snapshot
// per checkpointDelay regardless of output volume.
func (p *pane) scheduleCheckpoint() {
	p.cpMu.Lock()
	defer p.cpMu.Unlock()
	if p.cpClosing || p.cpTimer != nil {
		return
	}
	p.cpTimer = time.AfterFunc(checkpointDelay, func() {
		p.cpMu.Lock()
		p.cpTimer = nil
		closing := p.cpClosing
		p.cpMu.Unlock()
		if !closing {
			p.checkpointNow()
		}
	})
}

// checkpointNow captures and persists atomically with respect to other
// checkpoints of this pane: cpSaveMu spans capture AND save, so an older
// snapshot can never overwrite a newer one (the debounce timer racing the
// shutdown checkpoint used to allow exactly that).
func (p *pane) checkpointNow() {
	p.cpSaveMu.Lock()
	defer p.cpSaveMu.Unlock()
	p.mu.Lock()
	snap := p.term.Snapshot(false, 0)
	hist := p.term.HistoryANSI(historyPersist)
	cols, rows := p.cols, p.rows
	p.mu.Unlock()
	p.save(p.root, p.uuid, cols, rows, snap, hist)
}

func (p *pane) stopCheckpoints() {
	p.cpMu.Lock()
	defer p.cpMu.Unlock()
	p.cpClosing = true
	if p.cpTimer != nil {
		p.cpTimer.Stop()
		p.cpTimer = nil
	}
}
