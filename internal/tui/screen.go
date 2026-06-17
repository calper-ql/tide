package tui

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/mattn/go-runewidth"
	"golang.org/x/term"

	"github.com/calper-ql/tide/internal/input"
)

// enterSeq puts the terminal into teddy mode: alt screen, SGR mouse with
// button-drag tracking (1002 — tabs drag, the browser scrolls), bracketed
// paste, and focus reporting; then clear+home. resetSeq undoes it in
// reverse, leaving the user's shell screen as it was. Mirrors tide's
// client; teddy does NOT push the kitty keyboard protocol — raw mode alone
// frees Ctrl+S/Ctrl+C/Ctrl+Q (no XON/XOFF, no ISIG) for editor keybinds.
const (
	enterSeq = "\x1b[?1049h\x1b[?1002h\x1b[?1006h\x1b[?2004h\x1b[?1004h\x1b[2J\x1b[H"
	resetSeq = "\x1b[?1004l\x1b[?2004l\x1b[?1006l\x1b[?1002l\x1b[0m\x1b[?25h\x1b[?1049l"
)

// Screen owns the terminal: raw mode, the alt screen, and a double buffer.
// The app draws into Back() each frame and calls Flush(); only the rows that
// changed since the last frame are written, inside a synchronized-update
// bracket so the repaint never tears.
type Screen struct {
	in  *os.File
	out *os.File
	w   *bufio.Writer

	cols, rows  int
	front, back *Buffer
	raw         *term.State

	curX, curY int
	curVis     bool
	lastX      int
	lastY      int
	lastVis    bool
	dirtyAll   bool
	closed     bool
}

// NewScreen takes over stdin/stdout (both must be terminals), enters raw
// mode and the alt screen, and returns a Screen ready to draw into.
func NewScreen() (*Screen, error) {
	in, out := os.Stdin, os.Stdout
	if !term.IsTerminal(int(in.Fd())) || !term.IsTerminal(int(out.Fd())) {
		return nil, errors.New("teddy requires a terminal (stdin and stdout must be a tty)")
	}
	cols, rows, err := term.GetSize(int(out.Fd()))
	if err != nil {
		return nil, err
	}
	cols, rows = max(cols, 1), max(rows, 1)
	raw, err := term.MakeRaw(int(in.Fd()))
	if err != nil {
		return nil, err
	}
	s := &Screen{
		in: in, out: out, w: bufio.NewWriter(out),
		cols: cols, rows: rows,
		front: NewBuffer(cols, rows), back: NewBuffer(cols, rows),
		raw: raw, curVis: true, lastVis: true,
	}
	s.w.WriteString(enterSeq)
	return s, s.w.Flush()
}

// Close restores the terminal: reset sequences, then cooked mode. Safe to
// call more than once.
func (s *Screen) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	s.w.WriteString(resetSeq)
	_ = s.w.Flush()
	return term.Restore(int(s.in.Fd()), s.raw)
}

func (s *Screen) Size() (int, int) { return s.cols, s.rows }

// Back is the buffer the app draws into. It is sized to the current
// terminal; the app should Clear and redraw it fully each frame.
func (s *Screen) Back() *Buffer { return s.back }

// SetCursor places the text cursor for the next Flush. HideCursor/ShowCursor
// toggle its visibility (chrome-only frames hide it).
func (s *Screen) SetCursor(x, y int) { s.curX, s.curY = x, y }
func (s *Screen) ShowCursor()        { s.curVis = true }
func (s *Screen) HideCursor()        { s.curVis = false }

// Resize adopts a new terminal size (called from the main loop on a resize
// event, so buffer mutation never races the renderer) and forces the next
// frame to repaint fully.
func (s *Screen) Resize(cols, rows int) {
	cols, rows = max(cols, 1), max(rows, 1)
	if cols == s.cols && rows == s.rows {
		return
	}
	s.cols, s.rows = cols, rows
	s.back.Resize(cols, rows)
	s.front.Resize(cols, rows)
	s.dirtyAll = true
}

// Flush writes the frame the back buffer describes, diffed against the last.
func (s *Screen) Flush() error {
	frame := s.composeFrame()
	if len(frame) == 0 {
		return nil
	}
	if _, err := s.w.Write(frame); err != nil {
		return err
	}
	return s.w.Flush()
}

// composeFrame builds the byte stream Flush writes and advances the front
// buffer + cursor bookkeeping. Split out so the diff is unit-testable
// without a real terminal. Returns nil when nothing changed.
func (s *Screen) composeFrame() []byte {
	var changed []int
	for y := 0; y < s.rows; y++ {
		if s.dirtyAll || !rowEqual(s.back, s.front, y) {
			changed = append(changed, y)
		}
	}
	cursorMoved := s.curX != s.lastX || s.curY != s.lastY || s.curVis != s.lastVis
	if len(changed) == 0 && !cursorMoved && !s.dirtyAll {
		return nil
	}

	var b bytes.Buffer
	b.WriteString("\x1b[?2026h") // begin synchronized update (ignored if unsupported)
	if s.dirtyAll {
		b.WriteString("\x1b[2J")
	}
	if len(changed) > 0 {
		b.WriteString("\x1b[?25l") // park the cursor while repainting
		for _, y := range changed {
			cup(&b, 0, y)
			renderRow(&b, s.back, y)
			copyRow(s.front, s.back, y)
		}
	}
	cup(&b, s.curX, s.curY)
	if s.curVis {
		b.WriteString("\x1b[?25h")
	} else {
		b.WriteString("\x1b[?25l")
	}
	b.WriteString("\x1b[?2026l") // end synchronized update

	s.lastX, s.lastY, s.lastVis = s.curX, s.curY, s.curVis
	s.dirtyAll = false
	return b.Bytes()
}

// Event is one item from Events: a decoded input event, a resize, or the
// terminal closing (EOF on stdin).
type Event struct {
	Input  input.Event
	Resize bool
	Closed bool
	Cols   int
	Rows   int
}

// Events delivers input events and resize notifications on one channel. The
// reader goroutine decodes stdin with internal/input (so teddy speaks tide's
// exact input dialect); a SIGWINCH watcher reports new sizes. Resizes are
// only reported here — the main loop applies them via Resize — so the
// buffers are never mutated off the draw goroutine. The channel is never
// closed; EOF arrives as a Closed event.
func (s *Screen) Events() <-chan Event {
	ch := make(chan Event, 128)
	go func() {
		dec := input.NewDecoder()
		buf := make([]byte, 4096)
		for {
			n, err := s.in.Read(buf)
			for _, ev := range dec.Feed(buf[:n]) {
				ch <- Event{Input: ev}
			}
			if err != nil {
				ch <- Event{Closed: true}
				return
			}
		}
	}()
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	go func() {
		for range winch {
			if c, r, err := term.GetSize(int(s.out.Fd())); err == nil {
				ch <- Event{Resize: true, Cols: c, Rows: r}
			}
		}
	}()
	return ch
}

func cup(b *bytes.Buffer, x, y int) { fmt.Fprintf(b, "\x1b[%d;%dH", y+1, x+1) }

func rowEqual(a, b *Buffer, y int) bool {
	if a.W != b.W {
		return false
	}
	ar := a.cells[y*a.W : y*a.W+a.W]
	br := b.cells[y*b.W : y*b.W+b.W]
	for i := range ar {
		if ar[i] != br[i] {
			return false
		}
	}
	return true
}

func copyRow(dst, src *Buffer, y int) {
	copy(dst.cells[y*dst.W:y*dst.W+dst.W], src.cells[y*src.W:y*src.W+src.W])
}

// renderRow emits one row with minimal SGR changes, skipping wide-rune
// continuations (the lead rune already covers both columns). The cells sum
// to exactly W columns, so the row fully overwrites the prior frame's row.
func renderRow(b *bytes.Buffer, buf *Buffer, y int) {
	var cur Style
	first := true
	for x := 0; x < buf.W; {
		c := buf.at(x, y)
		if c.R == 0 { // continuation cell; defensive — the lead already advanced past it
			x++
			continue
		}
		if first || c.St != cur {
			c.St.writeSGR(b)
			cur, first = c.St, false
		}
		b.WriteRune(c.R)
		w := runewidth.RuneWidth(c.R)
		if w < 1 {
			w = 1
		}
		x += w
	}
	b.WriteString("\x1b[0m")
}
