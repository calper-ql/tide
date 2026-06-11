// A pane is one shell on a PTY, parsed into a VT grid the daemon owns
// (spec: core foundation §3). It knows nothing about clients or layout:
// the workspace composites panes for clients and routes input to them;
// the pane just reports dirtiness and checkpoints its content.
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

	"github.com/calper-ql/tide/internal/session"
	"github.com/calper-ql/tide/internal/vt"
)

const (
	historyKeep     = 5000 // scrollback lines held in memory
	historyPersist  = 1000 // scrollback lines written per checkpoint
	inboxSize       = 256  // input frames buffered before they are dropped
	answerbackSize  = 64   // VT query responses buffered toward the PTY
	checkpointDelay = time.Second
	maxDim          = 4096 // cols/rows bound; beyond this is hostile or absurd
)

// paneHooks are the pane's callbacks into its workspace. dirty fires after
// any visible change; exited fires once per shell death (the exit notice is
// already in the grid by then). save persists a content checkpoint and is
// expected to reject stale panes. clip fires when an inner program writes
// to the clipboard via OSC 52 (target is "c" or "p").
type paneHooks struct {
	dirty  func()
	exited func()
	save   func(paneID string, cols, rows int, snapshot []byte, history [][]byte)
	clip   func(target, text string)
}

type pane struct {
	id   string
	root string
	logf *log.Logger
	hook paneHooks

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
	dead    bool // shell exited; restart is an explicit user action
	closing bool

	cpMu      sync.Mutex
	cpTimer   *time.Timer
	cpClosing bool
	cpSaveMu  sync.Mutex // spans snapshot capture + save; see checkpointNow

	shutdownOnce sync.Once
}

func newPaneID() string {
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
func newPane(id, root string, stored *session.PaneContent, cols, rows int, socket string, logf *log.Logger, hook paneHooks) (*pane, error) {
	cols, rows = clampDim(cols, 80), clampDim(rows, 24)
	p := &pane{
		id:         id,
		root:       root,
		logf:       logf,
		hook:       hook,
		cols:       cols,
		rows:       rows,
		quit:       make(chan struct{}),
		inputQ:     make(chan []byte, inboxSize),
		answerback: make(chan []byte, answerbackSize),
	}
	restoreCols, restoreRows := cols, rows
	if stored != nil {
		// Checkpoint sizes are same-user data but still inputs: fall back
		// to the live size rather than panic on absurd values.
		restoreCols, restoreRows = clampDim(stored.Cols, cols), clampDim(stored.Rows, rows)
	}
	p.term = vt.New(restoreCols, restoreRows, historyKeep, ptyWriter{p})
	if stored != nil && (len(stored.History) > 0 || len(stored.Snapshot) > 0) {
		for _, l := range stored.History {
			p.term.Write(append(l, '\r', '\n'))
		}
		for i := 0; i < restoreRows; i++ {
			p.term.Write([]byte{'\n'})
		}
		if len(stored.Snapshot) > 0 {
			p.term.Write(stored.Snapshot)
		}
		// The snapshot faithfully restores the OLD application's input
		// modes — mouse reporting, bracketed paste, app cursor, alt screen.
		// This pane runs a FRESH shell that asked for none of them; stale
		// modes would make the router forward mouse clicks to a shell that
		// cannot parse them (they echo as ";40M" fragments at the prompt).
		// Visual content stays; input-affecting modes reset.
		p.term.Write([]byte("\x1b[?1049l\x1b[?9l\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1006l" +
			"\x1b[?1004l\x1b[?2004l\x1b[?1l\x1b>\x1b[?7h\x1b[?6l\x1b[r\x1b[2l\x1b[4l\x1b[?25h"))
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
func paneEnv(id, socket string) []string {
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
	return append(env, "TERM=xterm-256color", "TIDE_SESSION="+id+":"+socket)
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
	cmd.Env = paneEnv(p.id, socket)
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

// respawnIfDead restarts the shell of an exited pane (explicit user action:
// clicking the pane or its menu item).
func (p *pane) respawnIfDead(socket string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.dead || p.closing {
		return nil
	}
	return p.spawnLocked(socket)
}

func (p *pane) isDead() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.dead
}

// readLoop pumps PTY output into the VT. Shell death is the waiter's job:
// EIO here only means every slave fd is gone (a background job can hold the
// PTY long after the shell exits, and its output still belongs on screen).
func (p *pane) readLoop(ptmx *os.File) {
	buf := make([]byte, 32*1024)
	for {
		n, err := ptmx.Read(buf)
		if n > 0 {
			p.mu.Lock()
			p.term.Write(buf[:n])
			clips := p.term.DrainClips()
			p.mu.Unlock()
			for _, ev := range clips {
				if p.hook.clip != nil {
					p.hook.clip(ev.Target, ev.Text)
				}
			}
			p.hook.dirty()
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
	notice := fmt.Sprintf("\x1b[0m\r\n[tide] shell exited (status %d) — click to restart\r\n", status)
	p.term.Write([]byte(notice))
	p.mu.Unlock()
	p.hook.dirty()
	p.hook.exited()
	p.scheduleCheckpoint()
}

// input queues keyboard bytes for the PTY. When the queue is full — the
// foreground app stopped reading — frames are dropped: dropping input beats
// wedging the router behind a blocked PTY write.
func (p *pane) input(data []byte) {
	select {
	case p.inputQ <- data:
	default:
		p.logf.Printf("drop input pane=%s (pty not consuming)", p.id)
	}
}

// resize is driven by the layout: the pane is exactly its rect.
func (p *pane) resize(cols, rows int) {
	p.mu.Lock()
	defer p.mu.Unlock()
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

func (p *pane) size() (cols, rows int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cols, p.rows
}

// shutdown checkpoints the pane and hangs up the shell; safe to call from
// pane close, session kill, and daemon shutdown (it runs once). Closing the
// PTY master delivers the kernel's own SIGHUP to the pane's foreground
// process group; the explicit Signal covers a shell that ignores hangups,
// and cannot hit a recycled pid (see waitShell).
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
// snapshot can never overwrite a newer one.
func (p *pane) checkpointNow() {
	p.cpSaveMu.Lock()
	defer p.cpSaveMu.Unlock()
	p.mu.Lock()
	snap := p.term.Snapshot(false, 0)
	hist := p.term.HistoryANSI(historyPersist)
	cols, rows := p.cols, p.rows
	p.mu.Unlock()
	p.hook.save(p.id, cols, rows, snap, hist)
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
