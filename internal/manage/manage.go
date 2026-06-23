// Package manage is the interactive session manager behind `tide manage` (and
// `tide -r host manage`): a list of live sessions where killing one requires
// an explicit, per-session confirmation. Like the picker, it renders into the
// daemon's terminal-stream idiom and consumes decoded input events, so the
// same Model drives both a local raw-terminal loop and the remote bridge.
//
// The Model is pure UI state: it never talks to the daemon. A confirmed kill
// is exposed via TakeKill for the driver to execute (client.Kill), after which
// the driver calls SetSessions with the refreshed list. This keeps the
// destructive action out of the rendering code and trivially testable.
package manage

import (
	"bytes"
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/calper-ql/tide/internal/input"
)

const (
	listTop   = 2
	wheelStep = 3

	styReset = "\x1b[0m"
	styBar   = "\x1b[0;7m"      // reverse: bar
	styName  = "\x1b[0;1m"      // bold: session basename row
	styDim   = "\x1b[0;2m"      // dim: hints, flash
	stySel   = "\x1b[0;7;1m"    // reverse bold: selected row
	styWarn  = "\x1b[0;7;1;93m" // reverse bold yellow: the kill confirmation bar
)

// Session is one live session shown in the manager.
type Session struct {
	Root    string
	Panes   int
	Clients int
}

// Model is the manager's UI state.
type Model struct {
	sessions []Session
	hover    int // selected row; -1 = none
	offset   int
	confirm  int // index awaiting kill confirmation; -1 = none
	killReq  string
	quit     bool
	flash    string
	cols     int
	rows     int

	// Confirmation-bar button rects (row rows-1), recorded during render.
	killBtnX, killBtnW     int
	cancelBtnX, cancelBtnW int
}

func New(sessions []Session, cols, rows int) *Model {
	m := &Model{cols: cols, rows: rows, confirm: -1, hover: -1, sessions: sessions}
	if len(sessions) > 0 {
		m.hover = 0
	}
	return m
}

func (m *Model) Sessions() []Session   { return m.sessions }
func (m *Model) Quit() bool            { return m.quit }
func (m *Model) Flash(s string)        { m.flash = s }
func (m *Model) Confirming() bool      { return m.confirm >= 0 }
func (m *Model) Resize(cols, rows int) { m.cols, m.rows = cols, rows; m.clampOffset() }

// SetSessions replaces the list after a kill/refresh, re-clamping selection and
// closing any open confirmation (the confirmed session is gone).
func (m *Model) SetSessions(s []Session) {
	m.sessions = s
	m.confirm = -1
	if m.hover >= len(s) {
		m.hover = len(s) - 1
	}
	if len(s) == 0 {
		m.hover = -1
	}
	m.clampOffset()
}

// TakeKill returns a confirmed-kill root once, for the driver to execute.
func (m *Model) TakeKill() (string, bool) {
	if m.killReq == "" {
		return "", false
	}
	r := m.killReq
	m.killReq = ""
	return r, true
}

func (m *Model) tooSmall() bool { return m.cols < 24 || m.rows < 6 }

func (m *Model) visibleRows() int {
	v := m.rows - listTop - 1 // last row is the confirm/flash line
	if v < 0 {
		return 0
	}
	return v
}

func (m *Model) maxOffset() int {
	if over := len(m.sessions) - m.visibleRows(); over > 0 {
		return over
	}
	return 0
}

func (m *Model) clampOffset() { m.offset = clamp(m.offset, 0, m.maxOffset()) }

// rowAt maps a screen cell to a session index, or -1.
func (m *Model) rowAt(x, y int) int {
	if y < listTop || y >= listTop+m.visibleRows() {
		return -1
	}
	idx := m.offset + (y - listTop)
	if idx < 0 || idx >= len(m.sessions) {
		return -1
	}
	return idx
}

// Handle applies one decoded event and reports whether to repaint.
func (m *Model) Handle(ev input.Event) bool {
	if m.tooSmall() {
		return false
	}
	if m.confirm >= 0 {
		return m.handleConfirm(ev)
	}
	switch ev.Type {
	case input.EvKey:
		switch ev.Key {
		case input.KeyUp:
			return m.move(-1)
		case input.KeyDown:
			return m.move(1)
		case input.KeyEnter, input.KeyDelete:
			return m.askConfirm(m.hover)
		case input.KeyEscape:
			m.quit = true
			return true
		case input.KeyRune:
			switch ev.Rune {
			case 'k', 'x', 'd':
				return m.askConfirm(m.hover)
			case 'q':
				m.quit = true
				return true
			}
		}
	case input.EvMouse:
		switch ev.Mouse {
		case input.MouseWheelUp:
			return m.scroll(-wheelStep)
		case input.MouseWheelDown:
			return m.scroll(wheelStep)
		case input.MousePress:
			if idx := m.rowAt(ev.X, ev.Y); idx >= 0 {
				m.hover = idx
				return m.askConfirm(idx)
			}
		}
	}
	return false
}

// handleConfirm is the modal kill confirmation. Killing requires an explicit
// 'y' or a click on the Kill button; EVERYTHING else (n, Esc, Enter, q, a
// click elsewhere) cancels — the safe default for a destructive action.
func (m *Model) handleConfirm(ev input.Event) bool {
	switch ev.Type {
	case input.EvKey:
		if ev.Key == input.KeyRune && (ev.Rune == 'y' || ev.Rune == 'Y') {
			m.confirmKill()
			return true
		}
		m.confirm = -1 // n / Esc / Enter / q / anything → cancel
		return true
	case input.EvMouse:
		if ev.Mouse == input.MousePress {
			if ev.Y == m.rows-1 && ev.X >= m.killBtnX && ev.X < m.killBtnX+m.killBtnW {
				m.confirmKill()
			} else {
				m.confirm = -1 // click the Cancel button or anywhere else → cancel
			}
			return true
		}
	}
	return false
}

func (m *Model) confirmKill() {
	if m.confirm >= 0 && m.confirm < len(m.sessions) {
		m.killReq = m.sessions[m.confirm].Root
	}
	m.confirm = -1
}

func (m *Model) askConfirm(idx int) bool {
	if idx < 0 || idx >= len(m.sessions) {
		return false
	}
	m.confirm = idx
	m.flash = ""
	return true
}

func (m *Model) move(delta int) bool {
	if len(m.sessions) == 0 {
		return false
	}
	cur := m.hover
	if cur < 0 {
		cur, delta = 0, 0
	}
	n := clamp(cur+delta, 0, len(m.sessions)-1)
	if n == m.hover {
		return false
	}
	m.hover = n
	if n < m.offset {
		m.offset = n
	} else if v := m.visibleRows(); v > 0 && n >= m.offset+v {
		m.offset = n - v + 1
	}
	m.clampOffset()
	return true
}

func (m *Model) scroll(delta int) bool {
	n := clamp(m.offset+delta, 0, m.maxOffset())
	if n == m.offset {
		return false
	}
	m.offset = n
	return true
}

// --- rendering -----------------------------------------------------------

func (m *Model) Render() []byte {
	var b bytes.Buffer
	b.WriteString("\x1b[?25l" + styReset + "\x1b[2J")
	if m.tooSmall() {
		cup(&b, 0, 0)
		b.WriteString("window too small")
		return b.Bytes()
	}

	cup(&b, 0, 0)
	b.WriteString(styBar + padRight(fmt.Sprintf(" tide — manage sessions (%d)", len(m.sessions)), m.cols) + styReset)

	cup(&b, 1, 0)
	b.WriteString(styDim + truncate(" ↑↓ select · Enter/k kill · q quit", m.cols) + styReset)

	if len(m.sessions) == 0 {
		cup(&b, listTop+1, 2)
		b.WriteString(styDim + "no live sessions — press q to quit" + styReset)
	}
	for i := 0; i < m.visibleRows(); i++ {
		idx := m.offset + i
		if idx >= len(m.sessions) {
			break
		}
		cup(&b, listTop+i, 0)
		label := sessionLabel(m.sessions[idx])
		if idx == m.hover {
			b.WriteString(stySel + padRight(label, m.cols) + styReset)
		} else {
			b.WriteString(styName + truncate(label, m.cols) + styReset)
		}
	}

	m.renderBottom(&b)
	return b.Bytes()
}

func (m *Model) renderBottom(b *bytes.Buffer) {
	m.killBtnW, m.cancelBtnW = 0, 0
	cup(b, m.rows-1, 0)
	if m.confirm >= 0 && m.confirm < len(m.sessions) {
		const killBtn, cancelBtn = " [ y · Kill ] ", " [ n · Cancel ] "
		m.cancelBtnX = m.cols - utf8.RuneCountInString(cancelBtn)
		m.cancelBtnW = utf8.RuneCountInString(cancelBtn)
		m.killBtnX = m.cancelBtnX - utf8.RuneCountInString(killBtn)
		m.killBtnW = utf8.RuneCountInString(killBtn)
		q := fmt.Sprintf(" Kill %q? this ends every shell in it.", filepath.Base(m.sessions[m.confirm].Root))
		row := []rune(padRight(q, m.cols))
		for i, r := range []rune(killBtn) {
			if m.killBtnX+i >= 0 && m.killBtnX+i < len(row) {
				row[m.killBtnX+i] = r
			}
		}
		for i, r := range []rune(cancelBtn) {
			if m.cancelBtnX+i >= 0 && m.cancelBtnX+i < len(row) {
				row[m.cancelBtnX+i] = r
			}
		}
		b.WriteString(styWarn + string(row) + styReset)
		return
	}
	if m.flash != "" {
		b.WriteString(styDim + truncate(" "+m.flash, m.cols) + styReset)
	}
}

func sessionLabel(s Session) string {
	meta := fmt.Sprintf("%d pane%s", s.Panes, plural(s.Panes))
	if s.Clients > 0 {
		meta += fmt.Sprintf(" · %d client%s", s.Clients, plural(s.Clients))
	}
	return fmt.Sprintf(" %s  —  %s  ·  %s", filepath.Base(s.Root), s.Root, meta)
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func cup(b *bytes.Buffer, y, x int) { fmt.Fprintf(b, "\x1b[%d;%dH", y+1, x+1) }

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func truncate(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= w {
		return s
	}
	return string([]rune(s)[:w])
}

func padRight(s string, w int) string {
	s = truncate(s, w)
	if pad := w - utf8.RuneCountInString(s); pad > 0 {
		return s + strings.Repeat(" ", pad)
	}
	return s
}
