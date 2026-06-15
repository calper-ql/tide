// tide extension to the vt10x port: the snapshot renderer. It serializes
// the live terminal — screen, history, cursor, pen, charset, modes, scroll
// region, wrap-pending — as an ANSI byte stream that recreates the state
// exactly when written to a fresh terminal. This is what makes reattach
// "mid-keystroke exact": the daemon replays a snapshot, then resumes the
// raw PTY stream, and the client terminal cannot tell the difference.

package vt

import (
	"bytes"
	"fmt"
	"sort"
)

// Snapshot renders the current state. The order matters on real terminals:
// the reset prefix comes first (VTE-family terminals wipe scrollback on a
// full RIS, so no RIS is used at all and the reset must not follow the
// history replay); then the history replay (capped at historyMax lines),
// which populates the client terminal's native scrollback; then the screen
// reconstruction; and finally the in-flight tail — a partially received
// escape sequence and/or split UTF-8 rune — so the restored parser waits
// mid-sequence exactly like the source.
func (t *Term) Snapshot(includeHistory bool, historyMax int) []byte {
	t.State.lock()
	defer t.State.unlock()
	var b bytes.Buffer
	t.renderResetLocked(&b)
	if includeHistory {
		t.renderHistoryLocked(&b, historyMax)
	}
	t.renderScreenLocked(&b)
	if !t.seqOverflow {
		b.Write(t.State.seq)
	}
	b.Write(t.pending)
	return b.Bytes()
}

// renderResetLocked brings any terminal to a known clean state using only
// explicit sequences — never RIS, which destroys the host terminal's
// scrollback on VTE/xterm.js. Everything emitted here is parsed by this
// package, keeping snapshots round-trippable.
func (t *State) renderResetLocked(b *bytes.Buffer) {
	b.WriteString("\x1b[0m")     // SGR defaults
	b.WriteString("\x1b[?1049l") // main screen
	b.WriteString("\x1b[r")      // full scroll region
	b.WriteString("\x1b[?6l\x1b[?7h\x1b[?1l\x1b>\x1b[?5l\x1b[?25h")
	b.WriteString("\x1b[?9l\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1004l\x1b[?1006l\x1b[?1034l\x1b[?2004l")
	b.WriteString("\x1b[2l\x1b[4l\x1b[20l") // KAM, IRM, LNM off
	b.WriteString("\x1b]104\a\x1b]110\a\x1b]111\a")
	b.WriteString("\x1b(B")
	// Default tab stops every 8 columns.
	b.WriteString("\x1b[3g")
	for x := tabspaces; x < t.cols; x += tabspaces {
		fmt.Fprintf(b, "\x1b[1;%dH\x1bH", x+1)
	}
	b.WriteString("\x1b[2J\x1b[H")
}

// HistoryANSI returns the newest max scrollback lines (all if max <= 0),
// oldest first, each rendered as a standalone ANSI line without trailing
// newline. Trailing all-blank lines are dropped: they carry no content and
// are usually artifacts of earlier snapshot replays.
func (t *Term) HistoryANSI(max int) [][]byte {
	t.State.lock()
	defer t.State.unlock()
	n := t.historyCount
	last := n
	for last > 0 && blankLine(t.historyLine(last-1)) {
		last--
	}
	first := 0
	if max > 0 && last-first > max {
		first = last - max
	}
	out := make([][]byte, 0, last-first)
	for i := first; i < last; i++ {
		var b bytes.Buffer
		renderLine(&b, t.historyLine(i))
		out = append(out, append(b.Bytes(), "\x1b[0m"...))
	}
	return out
}

func blankLine(l line) bool {
	for _, g := range l {
		if g.Char != ' ' && g.Char != 0 {
			return false
		}
		if g.BG != DefaultBG || g.Mode&(attrReverse|attrUnderline|attrStrike|attrOverline) != 0 {
			return false
		}
	}
	return true
}

func (t *State) renderHistoryLocked(b *bytes.Buffer, max int) {
	n := t.historyCount
	first := 0
	if max > 0 && n > max {
		first = n - max
	}
	if n-first == 0 {
		return
	}
	b.WriteString("\x1b[0m")
	for i := first; i < n; i++ {
		renderLine(b, t.historyLine(i))
		b.WriteString("\x1b[0m\r\n")
	}
	// Push the replayed tail fully into the client's native scrollback;
	// the grid draw below owns the visible screen.
	for i := 0; i < t.rows; i++ {
		b.WriteByte('\n')
	}
}

// renderScreenLocked emits everything needed to reconstruct the state on
// top of renderResetLocked's clean slate. Only sequences the vt package
// itself parses are used, so a snapshot can be round-tripped through a
// fresh Term — the property the tests pin.
func (t *State) renderScreenLocked(b *bytes.Buffer) {
	// Palette overrides (OSC 4), deterministically ordered.
	if len(t.colorOverride) > 0 {
		keys := make([]int, 0, len(t.colorOverride))
		for k := range t.colorOverride {
			keys = append(keys, int(k))
		}
		sort.Ints(keys)
		for _, k := range keys {
			v := t.colorOverride[Color(k)]
			r, g, bb := rgb(int(v))
			switch Color(k) {
			case DefaultFG:
				fmt.Fprintf(b, "\x1b]10;rgb:%02x/%02x/%02x\a", r, g, bb)
			case DefaultBG:
				fmt.Fprintf(b, "\x1b]11;rgb:%02x/%02x/%02x\a", r, g, bb)
			default:
				fmt.Fprintf(b, "\x1b]4;%d;rgb:%02x/%02x/%02x\a", k, r, g, bb)
			}
		}
	}
	if t.title != "" {
		fmt.Fprintf(b, "\x1b]0;%s\a", t.title)
	}

	// Both screens, main first. If the alt screen is active, t.lines IS the
	// alt screen and t.altLines holds the saved main screen. The saved
	// cursor (DECSC slot) is reconstructed differently per branch: ?1049h
	// itself saves the cursor as it switches (in this parser and in real
	// terminals), so on the alt path the cursor is parked at the saved spot
	// just before switching; on the main path an explicit ESC 7 carries the
	// position, pen, and the origin/wrap-pending state bits.
	if t.mode&ModeAltScreen != 0 {
		t.renderGrid(b, t.altLines)
		appendSGR(b, t.curSaved.Attr)
		fmt.Fprintf(b, "\x1b[%d;%dH", t.curSaved.Y+1, t.curSaved.X+1)
		b.WriteString("\x1b[?1049h")
		t.renderGrid(b, t.lines)
	} else {
		t.renderGrid(b, t.lines)
		if t.curSaved.State&cursorOrigin != 0 {
			b.WriteString("\x1b[?6h") // region is not set yet: coordinates stay absolute
		}
		fmt.Fprintf(b, "\x1b[%d;%dH", t.curSaved.Y+1, t.curSaved.X+1)
		appendSGR(b, t.curSaved.Attr)
		if t.curSaved.State&cursorWrapNext != 0 && t.curSaved.X == t.cols-1 {
			// Re-arm the saved wrap-pending state by rewriting the glyph
			// under the saved position (its own SGR, then the saved pen
			// back).
			g := t.lines[t.curSaved.Y][t.curSaved.X]
			appendSGR(b, g)
			b.WriteRune(glyphRune(g))
			appendSGR(b, t.curSaved.Attr)
		}
		b.WriteString("\x1b7")
		if t.curSaved.State&cursorOrigin != 0 {
			b.WriteString("\x1b[?6l")
		}
	}

	// Custom tab stops, only when they differ from the every-8 default the
	// reset prefix installed.
	if !t.defaultTabs() {
		b.WriteString("\x1b[3g")
		for x := 0; x < t.cols; x++ {
			if t.tabs[x] {
				fmt.Fprintf(b, "\x1b[1;%dH\x1bH", x+1)
			}
		}
	}

	// Scroll region and origin mode (both home the cursor; final position
	// is emitted afterwards).
	if t.top != 0 || t.bottom != t.rows-1 {
		fmt.Fprintf(b, "\x1b[%d;%dr", t.top+1, t.bottom+1)
	}
	if t.cur.State&cursorOrigin != 0 {
		b.WriteString("\x1b[?6h")
	}

	// Modes. The reset prefix left everything default, so only deviations
	// are emitted.
	t.renderModes(b)

	// Final cursor position (region-relative under origin mode), pen, and
	// wrap-pending state.
	row, col := t.cur.Y, t.cur.X
	if t.cur.State&cursorOrigin != 0 {
		row -= t.top
	}
	if t.cur.State&cursorWrapNext != 0 {
		// Rewrite the last-column glyph to re-arm the pending wrap, then
		// restore the pen (SGR does not clear wrap-pending).
		g := t.lines[t.cur.Y][t.cur.X]
		fmt.Fprintf(b, "\x1b[%d;%dH", row+1, col+1)
		appendSGR(b, g)
		b.WriteRune(glyphRune(g))
		appendSGR(b, t.cur.Attr)
	} else {
		appendSGR(b, t.cur.Attr)
		fmt.Fprintf(b, "\x1b[%d;%dH", row+1, col+1)
	}

	// Charset last among glyph-affecting state: every glyph above was
	// emitted as its final (already-translated) rune, so the line-drawing
	// pen must only arm after the last re-arm write — an ASCII glyph
	// rewritten under an active gfx charset would be re-translated.
	if t.cur.Attr.Mode&attrGfx != 0 {
		b.WriteString("\x1b(0")
	}

	// tide: cursor shape (DECSCUSR), restored when an app set a non-default one.
	if t.cursorShape > 0 {
		fmt.Fprintf(b, "\x1b[%d q", t.cursorShape)
	}

	// Cursor visibility last.
	if t.mode&ModeHide != 0 {
		b.WriteString("\x1b[?25l")
	}
}

func (t *State) renderGrid(b *bytes.Buffer, grid []line) {
	for y := 0; y < t.rows && y < len(grid); y++ {
		fmt.Fprintf(b, "\x1b[%d;1H", y+1)
		renderLine(b, grid[y])
	}
	b.WriteString("\x1b[0m")
}

// renderLine emits a line's glyphs with minimal SGR changes. The caller is
// responsible for any trailing reset. Double-width dummies are skipped: the
// lead rune occupies both columns when re-rendered.
func renderLine(b *bytes.Buffer, l line) {
	var cur Glyph
	first := true
	for _, g := range l {
		if g.Mode&attrWideDummy != 0 {
			continue
		}
		if first || sgrKey(g) != sgrKey(cur) {
			appendSGR(b, g)
			cur, first = g, false
		}
		b.WriteRune(glyphRune(g))
	}
}

func glyphRune(g Glyph) rune {
	if g.Char == 0 {
		return ' '
	}
	return g.Char
}

// sgrKey collapses a glyph to its display attributes; attrWrap, attrGfx,
// and the wide-pair bits are bookkeeping with no SGR representation.
func sgrKey(g Glyph) Glyph {
	g.Char = 0
	g.Mode &^= attrWrap | attrGfx | attrWide | attrWideDummy
	return g
}

// appendSGR emits the full SGR state for a glyph. Reverse video is emitted
// as ;7 with the stored (pre-swapped) colors swapped back, because setChar
// bakes the swap into the grid; re-parsing therefore reproduces the stored
// glyph exactly.
func appendSGR(b *bytes.Buffer, g Glyph) {
	b.WriteString("\x1b[0")
	fg, bg := g.FG, g.BG
	if g.Mode&attrReverse != 0 {
		b.WriteString(";7")
		fg, bg = bg, fg
	}
	if g.Mode&attrBold != 0 {
		b.WriteString(";1")
	}
	if g.Mode&attrFaint != 0 {
		b.WriteString(";2")
	}
	if g.Mode&attrItalic != 0 {
		b.WriteString(";3")
	}
	if g.Mode&attrUnderline != 0 {
		b.WriteString(";4")
	}
	if g.Mode&attrBlink != 0 {
		b.WriteString(";5")
	}
	if g.Mode&attrConceal != 0 {
		b.WriteString(";8")
	}
	if g.Mode&attrStrike != 0 {
		b.WriteString(";9")
	}
	if g.Mode&attrOverline != 0 {
		b.WriteString(";53")
	}
	appendColor(b, fg, true)
	appendColor(b, bg, false)
	b.WriteByte('m')
}

func appendColor(b *bytes.Buffer, c Color, fg bool) {
	switch {
	case c == DefaultFG || c == DefaultBG || c == DefaultCursor:
		// SGR 0 already selected the defaults.
	case c < 8:
		if fg {
			fmt.Fprintf(b, ";%d", 30+c)
		} else {
			fmt.Fprintf(b, ";%d", 40+c)
		}
	case c < 16:
		if fg {
			fmt.Fprintf(b, ";%d", 90+c-8)
		} else {
			fmt.Fprintf(b, ";%d", 100+c-8)
		}
	case c < 256:
		if fg {
			fmt.Fprintf(b, ";38;5;%d", c)
		} else {
			fmt.Fprintf(b, ";48;5;%d", c)
		}
	default:
		r, g, bl := rgb(int(c))
		if fg {
			fmt.Fprintf(b, ";38;2;%d;%d;%d", r, g, bl)
		} else {
			fmt.Fprintf(b, ";48;2;%d;%d;%d", r, g, bl)
		}
	}
}

func (t *State) renderModes(b *bytes.Buffer) {
	type m struct {
		flag ModeFlag
		on   string
	}
	for _, mm := range []m{
		{ModeAppCursor, "\x1b[?1h"},
		{ModeReverse, "\x1b[?5h"},
		{ModeMouseX10, "\x1b[?9h"},
		{ModeMouseButton, "\x1b[?1000h"},
		{ModeMouseMotion, "\x1b[?1002h"},
		{ModeMouseMany, "\x1b[?1003h"},
		{ModeFocus, "\x1b[?1004h"},
		{ModeMouseSgr, "\x1b[?1006h"},
		{Mode8bit, "\x1b[?1034h"},
		{ModeBracketedPaste, "\x1b[?2004h"},
		{ModeKeyboardLock, "\x1b[2h"},
		{ModeInsert, "\x1b[4h"},
		{ModeEcho, "\x1b[12h"},
		{ModeCRLF, "\x1b[20h"},
	} {
		if t.mode&mm.flag != 0 {
			b.WriteString(mm.on)
		}
	}
	if t.mode&ModeWrap == 0 {
		b.WriteString("\x1b[?7l")
	}
	if t.mode&ModeAppKeypad != 0 {
		b.WriteString("\x1b=")
	}
}

func (t *State) defaultTabs() bool {
	for x := 0; x < t.cols; x++ {
		want := x != 0 && x%tabspaces == 0
		if t.tabs[x] != want {
			return false
		}
	}
	return true
}
