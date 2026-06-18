// tide extension to the vt10x port: the view API the daemon's compositor
// renders panes through. Content space addresses every line the pane still
// holds: history lines 0..H-1, then the live screen rows H..H+rows-1.

package vt

import (
	"bytes"
	"strings"
)

// ContentSize returns the pane's content dimensions: history line count,
// live rows, and columns.
func (t *Term) ContentSize() (history, rows, cols int) {
	t.State.lock()
	defer t.State.unlock()
	return t.historyCount, t.rows, t.cols
}

// View copies the window of content visible when scrolled back by scroll
// lines (0 = live screen): exactly rows lines, each exactly cols glyphs.
// Rows beyond content come back blank. The copy is what makes it safe to
// render outside the terminal lock while PTY output keeps flowing, and the
// returned start (the content index of the first view row) is captured
// under the same lock — a separate ContentSize call could race history
// growth and shift selection mapping by a line.
func (t *Term) View(scroll, rows int) (view [][]Glyph, start int) {
	t.State.lock()
	defer t.State.unlock()
	if scroll < 0 {
		scroll = 0
	}
	if scroll > t.historyCount {
		scroll = t.historyCount
	}
	out := make([][]Glyph, rows)
	start = t.historyCount - scroll
	for i := 0; i < rows; i++ {
		idx := start + i
		l := t.contentLineLocked(idx)
		cp := make([]Glyph, t.cols)
		for x := range cp {
			if l != nil && x < len(l) {
				cp[x] = l[x]
			} else {
				cp[x] = Glyph{Char: ' ', FG: DefaultFG, BG: DefaultBG}
			}
		}
		out[i] = cp
	}
	return out, start
}

// contentLineLocked returns the line at a content-space index, or nil when
// out of range.
func (t *State) contentLineLocked(idx int) line {
	if idx < 0 {
		return nil
	}
	if idx < t.historyCount {
		return t.historyLine(idx)
	}
	row := idx - t.historyCount
	if row >= t.rows {
		return nil
	}
	return t.lines[row]
}

// CursorState returns the cursor position on the live screen and whether
// it should be drawn.
func (t *Term) CursorState() (x, y int, visible bool) {
	t.State.lock()
	defer t.State.unlock()
	return t.cur.X, t.cur.Y, t.mode&ModeHide == 0
}

// CursorShape returns the DECSCUSR cursor style an inner program set (0 = none
// set; the client keeps its own default).
func (t *Term) CursorShape() int {
	t.State.lock()
	defer t.State.unlock()
	return t.cursorShape
}

// ModeSnapshot returns the current mode flags under the lock.
func (t *Term) ModeSnapshot() ModeFlag {
	t.State.lock()
	defer t.State.unlock()
	return t.mode
}

// KeyboardProtoSnapshot returns the inner app's requested keyboard
// enhancements under the lock: the active Kitty keyboard protocol flags
// (0 = off) and the xterm modifyOtherKeys level (0/1/2). The router uses
// them to re-encode modified keys the legacy form would have to drop.
func (t *Term) KeyboardProtoSnapshot() (kittyFlags, modifyOtherKeys int) {
	t.State.lock()
	defer t.State.unlock()
	return t.kittyFlags, t.modifyOtherKeys
}

// TitleSnapshot returns the OSC title under the lock.
func (t *Term) TitleSnapshot() string {
	t.State.lock()
	defer t.State.unlock()
	return t.title
}

// ContentText extracts the text between two content-space positions
// (inclusive start, exclusive end column semantics like a text editor's
// linear selection). Trailing blanks are trimmed per line; soft-wrapped
// lines (attrWrap on their last cell) join without a newline; wide dummy
// cells contribute nothing.
func (t *Term) ContentText(startLine, startX, endLine, endX int) string {
	t.State.lock()
	defer t.State.unlock()
	if startLine > endLine || (startLine == endLine && startX >= endX) {
		return ""
	}
	var sb strings.Builder
	for idx := startLine; idx <= endLine; idx++ {
		l := t.contentLineLocked(idx)
		if l == nil {
			continue
		}
		x0, x1 := 0, len(l)
		if idx == startLine {
			x0 = clamp(startX, 0, len(l))
		}
		if idx == endLine {
			x1 = clamp(endX, 0, len(l))
		}
		var lineText strings.Builder
		for x := x0; x < x1; x++ {
			g := l[x]
			if g.Mode&attrWideDummy != 0 {
				continue
			}
			if g.Char == 0 {
				lineText.WriteByte(' ')
			} else {
				lineText.WriteRune(g.Char)
			}
		}
		sb.WriteString(strings.TrimRight(lineText.String(), " "))
		if idx != endLine {
			softWrap := len(l) > 0 && l[len(l)-1].Mode&attrWrap != 0
			if !softWrap {
				sb.WriteByte('\n')
			}
		}
	}
	return sb.String()
}

// RenderSegment emits glyphs l[x0:x0+w] with minimal SGR changes, padding
// with blanks to exactly w columns, inverting cells in [invertFrom,
// invertTo) (cell coords relative to the full line; pass -1,-1 for none).
// Wide pairs cut at either edge degrade to spaces so column accounting
// stays exact. The caller owns cursor positioning and any trailing reset.
func RenderSegment(b *bytes.Buffer, l []Glyph, x0, w int, invertFrom, invertTo int) {
	var cur Glyph
	first := true
	for x := x0; x < x0+w; x++ {
		var g Glyph
		if x >= 0 && x < len(l) {
			g = l[x]
		} else {
			g = Glyph{Char: ' ', FG: DefaultFG, BG: DefaultBG}
		}
		// Degrade pairs cut by the clip window.
		if g.Mode&attrWide != 0 && x+1 >= x0+w {
			g = Glyph{Char: ' ', FG: g.FG, BG: g.BG, Mode: g.Mode &^ attrWide}
		}
		if g.Mode&attrWideDummy != 0 {
			if x == x0 { // lead is outside the window
				g = Glyph{Char: ' ', FG: g.FG, BG: g.BG, Mode: g.Mode &^ attrWideDummy}
			} else {
				continue // lead already rendered and covers this column
			}
		}
		if x >= invertFrom && x < invertTo {
			g.Mode ^= attrReverse
			g.FG, g.BG = g.BG, g.FG // appendSGR re-swaps; net effect is ;7
		}
		if first || sgrKey(g) != sgrKey(cur) {
			appendSGR(b, g)
			cur, first = g, false
		}
		b.WriteRune(glyphRune(g))
	}
}

// AppendSGR exposes the renderer's SGR emitter for chrome drawing.
func AppendSGR(b *bytes.Buffer, g Glyph) { appendSGR(b, g) }

// MouseReporting summarizes a pane's mouse-reporting request for the
// router: which event classes the inner application asked for, and whether
// it wants SGR encoding.
type MouseReporting struct {
	X10        bool // presses only
	Normal     bool // press + release + wheel
	ButtonDrag bool // + motion with a button held
	AnyMotion  bool // + all motion
	SGR        bool
}

func (t *Term) MouseSnapshot() MouseReporting {
	t.State.lock()
	defer t.State.unlock()
	return MouseReporting{
		X10:        t.mode&ModeMouseX10 != 0,
		Normal:     t.mode&ModeMouseButton != 0,
		ButtonDrag: t.mode&ModeMouseMotion != 0,
		AnyMotion:  t.mode&ModeMouseMany != 0,
		SGR:        t.mode&ModeMouseSgr != 0,
	}
}
