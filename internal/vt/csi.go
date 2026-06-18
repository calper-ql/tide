// Ported from vt10x (github.com/hinshun/vt10x), MIT licensed — see LICENSE-vt10x.
// Local changes are marked with "tide:" comments.

package vt

import (
	"fmt"
	"strconv"
	"strings"
)

// CSI (Control Sequence Introducer)
// ESC+[
type csiEscape struct {
	buf  []byte
	args []int
	// tide: argsSub[i] marks args[i] as a colon SUB-parameter of an earlier
	// arg (ECMA-48 / ITU T.416), so setAttr can tell ESC[4:3m (one styled
	// underline) from ESC[4;3m (underline + italic) and parse the colon
	// direct-color forms ESC[38:2:r:g:bm.
	argsSub []bool
	mode    byte
	// tide: the trailing intermediate byte (0x20-0x2F), e.g. the SP in
	// DECSCUSR (CSI Ps SP q) or the '!' in DECSTR (CSI ! p). Upstream fed it
	// to Atoi, which failed and dropped the whole sequence.
	inter byte
	priv  bool
}

func (c *csiEscape) reset() {
	c.buf = c.buf[:0]
	c.args = c.args[:0]
	c.argsSub = c.argsSub[:0]
	c.mode = 0
	c.inter = 0
	c.priv = false
}

func (c *csiEscape) put(b byte) bool {
	c.buf = append(c.buf, b)
	if b >= 0x40 && b <= 0x7E || len(c.buf) >= 256 {
		c.parse()
		return true
	}
	return false
}

func (c *csiEscape) parse() {
	c.mode = c.buf[len(c.buf)-1]
	c.args = c.args[:0]
	c.argsSub = c.argsSub[:0]
	c.inter = 0
	if len(c.buf) == 1 {
		return
	}
	s := string(c.buf[:len(c.buf)-1]) // drop the final byte
	// Leading private/marker byte. Only '?' sets priv; '>', '=' and '<' are
	// kept out of the parameter text but resolved via c.buf elsewhere (DA,
	// and the Kitty keyboard protocol's CSI >/=/< ... u forms).
	if len(s) > 0 && (s[0] == '?' || s[0] == '>' || s[0] == '=' || s[0] == '<') {
		if s[0] == '?' {
			c.priv = true
		}
		s = s[1:]
	}
	// Trailing intermediate bytes (0x20-0x2F); keep the last (single in
	// practice for DECSCUSR/DECSTR/DECSCL/DECRQM).
	for len(s) > 0 {
		last := s[len(s)-1]
		if last >= 0x20 && last <= 0x2f {
			c.inter = last
			s = s[:len(s)-1]
		} else {
			break
		}
	}
	if s == "" {
		return
	}
	// Parameters are ';'-separated; each may carry ':'-separated
	// sub-parameters. An omitted field is the default (0), matching st's
	// strtol-on-empty and xterm — never break and drop the rest.
	for _, tok := range strings.Split(s, ";") {
		sub := false
		for _, part := range strings.Split(tok, ":") {
			v := 0
			if part != "" {
				if n, err := strconv.Atoi(part); err == nil {
					v = n
				}
			}
			c.args = append(c.args, v)
			c.argsSub = append(c.argsSub, sub)
			sub = true
		}
	}
}

func (c *csiEscape) arg(i, def int) int {
	if i >= len(c.args) || i < 0 {
		return def
	}
	return c.args[i]
}

// maxarg takes the maximum of arg(i, def) and def
func (c *csiEscape) maxarg(i, def int) int {
	return max(c.arg(i, def), def)
}

func (t *State) handleCSI() {
	c := &t.csi
	if c.inter != 0 {
		t.handleCSIInter()
		return
	}
	switch c.mode {
	default:
		goto unknown
	case '@': // ICH - insert <n> blank char
		t.insertBlanks(c.arg(0, 1))
	case 'A': // CUU - cursor <n> up
		t.moveTo(t.cur.X, t.cur.Y-c.maxarg(0, 1))
	case 'B', 'e': // CUD, VPR - cursor <n> down
		t.moveTo(t.cur.X, t.cur.Y+c.maxarg(0, 1))
	case 'c': // DA - device attributes
		// tide: applications synchronously probe DA to fingerprint the
		// terminal, and the query can never reach a real terminal (the
		// compositor renders grids, not raw streams) — so the VT must
		// answer. Primary: VT220-class with ANSI color. Secondary
		// (CSI > c): xterm-style id.
		if len(c.buf) > 0 && c.buf[0] == '>' {
			t.w.Write([]byte("\x1b[>41;1;0c"))
		} else if c.arg(0, 0) == 0 {
			t.w.Write([]byte("\x1b[?62;22c"))
		}
	case 'C', 'a': // CUF, HPR - cursor <n> forward
		t.moveTo(t.cur.X+c.maxarg(0, 1), t.cur.Y)
	case 'D': // CUB - cursor <n> backward
		t.moveTo(t.cur.X-c.maxarg(0, 1), t.cur.Y)
	case 'E': // CNL - cursor <n> down and first col
		t.moveTo(0, t.cur.Y+c.maxarg(0, 1))
	case 'F': // CPL - cursor <n> up and first col
		t.moveTo(0, t.cur.Y-c.maxarg(0, 1))
	case 'g': // TBC - tabulation clear
		switch c.arg(0, 0) {
		// clear current tab stop
		case 0:
			t.tabs[t.cur.X] = false
		// clear all tabs
		case 3:
			for i := range t.tabs {
				t.tabs[i] = false
			}
		default:
			goto unknown
		}
	case 'G', '`': // CHA, HPA - Move to <col>
		t.moveTo(c.arg(0, 1)-1, t.cur.Y)
	case 'H', 'f': // CUP, HVP - move to <row> <col>
		t.moveAbsTo(c.arg(1, 1)-1, c.arg(0, 1)-1)
	case 'I': // CHT - cursor forward tabulation <n> tab stops
		n := c.arg(0, 1)
		for i := 0; i < n; i++ {
			t.putTab(true)
		}
	case 'J': // ED - clear screen
		// TODO: sel.ob.x = -1
		switch c.arg(0, 0) {
		case 0: // below
			t.clear(t.cur.X, t.cur.Y, t.cols-1, t.cur.Y)
			if t.cur.Y < t.rows-1 {
				t.clear(0, t.cur.Y+1, t.cols-1, t.rows-1)
			}
		case 1: // above
			// tide: rows strictly above the cursor are cleared in full, then
			// the cursor row up to the cursor. Upstream guarded with >1, so a
			// cursor on row 1 left row 0 uncleared.
			if t.cur.Y > 0 {
				t.clear(0, 0, t.cols-1, t.cur.Y-1)
			}
			t.clear(0, t.cur.Y, t.cur.X, t.cur.Y)
		case 2: // all
			t.clear(0, 0, t.cols-1, t.rows-1)
		case 3: // tide: clear scrollback (xterm). The screen is untouched.
			t.clearHistory()
		default:
			goto unknown
		}
	case 'K': // EL - clear line
		switch c.arg(0, 0) {
		case 0: // right
			t.clear(t.cur.X, t.cur.Y, t.cols-1, t.cur.Y)
		case 1: // left
			t.clear(0, t.cur.Y, t.cur.X, t.cur.Y)
		case 2: // all
			t.clear(0, t.cur.Y, t.cols-1, t.cur.Y)
		}
	case 'S': // SU - scroll <n> lines up
		t.scrollUp(t.top, c.arg(0, 1))
	case 'T': // SD - scroll <n> lines down
		t.scrollDown(t.top, c.arg(0, 1))
	case 'L': // IL - insert <n> blank lines
		t.insertBlankLines(c.arg(0, 1))
	case 'l': // RM - reset mode
		t.setMode(c.priv, false, c.args)
	case 'M': // DL - delete <n> lines
		t.deleteLines(c.arg(0, 1))
	case 'X': // ECH - erase <n> chars
		t.clear(t.cur.X, t.cur.Y, t.cur.X+c.arg(0, 1)-1, t.cur.Y)
	case 'P': // DCH - delete <n> chars
		t.deleteChars(c.arg(0, 1))
	case 'Z': // CBT - cursor backward tabulation <n> tab stops
		n := c.arg(0, 1)
		for i := 0; i < n; i++ {
			t.putTab(false)
		}
	case 'b': // REP - repeat the last printed graphic char <n> times
		// tide: TERM=xterm-256color advertises the 'rep' capability, so apps
		// (and terminfo) emit CSI b. Mirrors st: no-op with no prior char,
		// count clamped (a huge count must not wedge the daemon).
		if t.lastChar != 0 {
			n := clamp(c.maxarg(0, 1), 1, 65535)
			for i := 0; i < n; i++ {
				t.parse(t.lastChar)
			}
		}
	case 'd': // VPA - move to <row>
		t.moveAbsTo(t.cur.X, c.arg(0, 1)-1)
	case 'h': // SM - set terminal mode
		t.setMode(c.priv, true, c.args)
	case 'm': // SGR - terminal attribute (color)
		if len(c.buf) > 0 && c.buf[0] == '>' {
			// tide: CSI > Pp ; Pv m is xterm XTMODKEYS ("set key modifier
			// options"), NOT an SGR — routing it to setAttr would corrupt the
			// pane's text attributes. Only resource 4 (modifyOtherKeys)
			// changes how the router encodes keys; the rest are ignored.
			t.setModifyOtherKeys(c.arg(0, 0), c.arg(1, 0))
		} else {
			t.setAttr(c.args, c.argsSub)
		}
	case 'n':
		switch c.arg(0, 0) {
		case 5: // DSR - device status report
			t.w.Write([]byte("\033[0n"))
		case 6: // CPR - cursor position report
			t.w.Write([]byte(fmt.Sprintf("\033[%d;%dR", t.cur.Y+1, t.cur.X+1)))
		}
	case 'r': // DECSTBM - set scrolling region
		if c.priv {
			goto unknown
		} else {
			// tide: an inverted/degenerate region (top >= bottom) is ignored,
			// not swapped into a region the app never asked for (xterm).
			top, bot := c.arg(0, 1)-1, c.arg(1, t.rows)-1
			if top < bot {
				t.setScroll(top, bot)
				t.moveAbsTo(0, 0)
			}
		}
	case 's': // DECSC - save cursor position (ANSI.SYS)
		t.saveCursor()
	case 'u':
		// tide: bare CSI u is DECRC (restore cursor, ANSI.SYS). With a
		// private marker it is a Kitty keyboard protocol request — the inner
		// app enabling enhanced key reporting so combinations like shift+enter
		// become distinguishable (the router then re-encodes them in kind).
		switch {
		case c.priv: // CSI ? u - query the active flags
			t.w.Write(fmt.Appendf(nil, "\x1b[?%du", t.kittyFlags))
		case len(c.buf) > 0 && c.buf[0] == '>': // CSI > flags u - push
			t.kittyPush(c.arg(0, 0))
		case len(c.buf) > 0 && c.buf[0] == '<': // CSI < n u - pop n entries
			t.kittyPop(c.arg(0, 1))
		case len(c.buf) > 0 && c.buf[0] == '=': // CSI = flags ; mode u - set
			t.kittySet(c.arg(0, 0), c.arg(1, 1))
		default:
			t.restoreCursor()
		}
	}
	return
unknown: // TODO: get rid of this goto
	t.logf("unknown CSI sequence '%c'\n", c.mode)
	// TODO: c.dump()
}

// handleCSIInter dispatches CSI sequences that carry an intermediate byte.
// Upstream fed the intermediate to Atoi, dropping these entirely.
func (t *State) handleCSIInter() {
	c := &t.csi
	switch {
	case c.inter == ' ' && c.mode == 'q': // DECSCUSR - set cursor style/shape
		// arg omitted -> 0 (no forced shape; the client keeps its default).
		t.cursorShape = c.arg(0, 0)
	case c.inter == '!' && c.mode == 'p': // DECSTR - soft terminal reset
		t.softReset()
	case c.inter == '"' && c.mode == 'p': // DECSCL - conformance level; ignored
	case c.inter == '$' && c.mode == 'p': // DECRQM - request mode; no reply (apps tolerate)
	default:
		t.logf("unknown CSI intermediate %q mode %q\n", string(c.inter), string(c.mode))
	}
}

// tide: the Kitty keyboard protocol flags occupy five bits — disambiguate
// escape codes, report event types, report alternate keys, report all keys
// as escape codes, report associated text.
const kittyFlagsMask = 0x1f

// kittyPush activates flags (CSI > flags u), saving the previous set so a
// later pop restores it.
func (t *State) kittyPush(flags int) {
	t.kittyStack = append(t.kittyStack, t.kittyFlags)
	t.kittyFlags = flags & kittyFlagsMask
}

// kittyPop restores the flags saved by the n most recent pushes (CSI < n u),
// clamped to the stack depth; an exhausted stack means all enhancements off.
func (t *State) kittyPop(n int) {
	if n < 1 {
		n = 1
	}
	for ; n > 0 && len(t.kittyStack) > 0; n-- {
		t.kittyFlags = t.kittyStack[len(t.kittyStack)-1]
		t.kittyStack = t.kittyStack[:len(t.kittyStack)-1]
	}
	if len(t.kittyStack) == 0 {
		// nothing left to restore from; over-popping disables the protocol
		if n > 0 {
			t.kittyFlags = 0
		}
	}
}

// kittySet replaces (mode 1), adds (mode 2), or removes (mode 3) the active
// flags in place, leaving the push/pop stack untouched (CSI = flags ; mode u).
func (t *State) kittySet(flags, mode int) {
	flags &= kittyFlagsMask
	switch mode {
	case 2:
		t.kittyFlags |= flags
	case 3:
		t.kittyFlags &^= flags
	default: // mode 1 (and the unspecified default): set these, clear the rest
		t.kittyFlags = flags
	}
}

// setModifyOtherKeys records the xterm modifyOtherKeys level (resource 4 of
// XTMODKEYS). Other resources do not affect key encoding and are ignored.
func (t *State) setModifyOtherKeys(resource, level int) {
	if resource != 4 {
		return
	}
	t.modifyOtherKeys = level
}
