package input

import (
	"fmt"
	"unicode/utf8"
)

// EncodeOpts mirror the destination pane's terminal modes. The daemon
// tracks them from the pane's VT and re-encodes every routed key
// accordingly, so a pane always receives exactly what a directly-attached
// terminal in those modes would have sent it.
type EncodeOpts struct {
	AppCursor      bool // DECCKM: arrows/Home/End as SS3
	AppKeypad      bool // DECNKM; currently informational
	BracketedPaste bool // mode 2004: wrap pastes in 200~/201~
	CRLF           bool // LNM: Enter sends \r\n instead of \r

	// KittyFlags is the pane's active Kitty keyboard protocol flag set (0
	// when the app has not enabled it); ModifyOtherKeys is its xterm
	// modifyOtherKeys level (0/1/2). When either is on, the modifier
	// combinations the legacy encodings drop — shift+enter most visibly —
	// are sent in the protocol's escape form instead. See EncodeKey.
	KittyFlags      int
	ModifyOtherKeys int
}

// EncodeKey renders a key event as the byte sequence a directly-attached
// terminal would send to an application with those modes. Alt prefixes
// ESC (or joins the CSI mods param on special keys); Ctrl+rune produces
// control bytes; modified arrows/F-keys use CSI 1;mods. Modifier
// combinations the legacy encodings cannot carry are dropped, exactly as
// a real terminal without an extended keyboard protocol drops them
// (Ctrl+Enter sends \r, Ctrl+1 sends '1'). Returns nil for events that
// have no terminal encoding at all (they must not be forwarded).
func EncodeKey(ev Event, o EncodeOpts) []byte {
	if ev.Type != EvKey {
		return nil
	}
	m := ev.Mods & (Shift | Alt | Ctrl)
	// An app that negotiated an enhanced keyboard protocol wants the modifier
	// combinations the legacy encodings cannot carry; render those here and
	// let everything the legacy form handles faithfully fall through.
	if b := encodeEnhanced(ev, m, o); b != nil {
		return b
	}
	switch ev.Key {
	case KeyUp:
		return cursorKey('A', m, o.AppCursor)
	case KeyDown:
		return cursorKey('B', m, o.AppCursor)
	case KeyRight:
		return cursorKey('C', m, o.AppCursor)
	case KeyLeft:
		return cursorKey('D', m, o.AppCursor)
	case KeyHome:
		return cursorKey('H', m, o.AppCursor)
	case KeyEnd:
		return cursorKey('F', m, o.AppCursor)
	case KeyF1:
		return fnKey('P', m)
	case KeyF2:
		return fnKey('Q', m)
	case KeyF3:
		return fnKey('R', m)
	case KeyF4:
		return fnKey('S', m)
	case KeyInsert:
		return tildeKey(2, m)
	case KeyDelete:
		return tildeKey(3, m)
	case KeyPageUp:
		return tildeKey(5, m)
	case KeyPageDown:
		return tildeKey(6, m)
	case KeyF5:
		return tildeKey(15, m)
	case KeyF6:
		return tildeKey(17, m)
	case KeyF7:
		return tildeKey(18, m)
	case KeyF8:
		return tildeKey(19, m)
	case KeyF9:
		return tildeKey(20, m)
	case KeyF10:
		return tildeKey(21, m)
	case KeyF11:
		return tildeKey(23, m)
	case KeyF12:
		return tildeKey(24, m)
	case KeyEnter:
		b := []byte{'\r'}
		if o.CRLF {
			b = append(b, '\n')
		}
		return altPrefix(b, m)
	case KeyTab:
		if m&Shift != 0 {
			if m == Shift {
				return []byte("\x1b[Z")
			}
			return fmt.Appendf(nil, "\x1b[1;%dZ", modParam(m))
		}
		return altPrefix([]byte{'\t'}, m)
	case KeyBackspace:
		if m&Ctrl != 0 {
			return altPrefix([]byte{0x08}, m) // ctrl+backspace is BS, not DEL
		}
		return altPrefix([]byte{0x7f}, m)
	case KeyEscape:
		return altPrefix([]byte{0x1b}, m)
	case KeySpace:
		if m&Ctrl != 0 {
			return altPrefix([]byte{0x00}, m)
		}
		return altPrefix([]byte{' '}, m)
	case KeyRune:
		return encodeRune(ev.Rune, m)
	}
	return nil
}

// encodeEnhanced renders the modified keys whose modifiers the legacy
// terminal encodings cannot carry, using whichever enhanced keyboard
// protocol the pane has enabled (Kitty preferred, else modifyOtherKeys). It
// returns nil — deferring to the legacy encoding — when no protocol is
// active, or when the legacy form already delivers the key faithfully:
// unmodified keys, the alt that ESC-prefixes, ctrl on keys with a C0 byte,
// all printable text, and the arrows/Home/End/function keys/Tab that already
// carry the full modifier set via CSI 1;mods. Only the Kitty protocol's
// disambiguation level is produced; richer flags (event types, all keys as
// escape codes) degrade to it, which is a faithful subset.
func encodeEnhanced(ev Event, m Mod, o EncodeOpts) []byte {
	if o.KittyFlags == 0 && o.ModifyOtherKeys == 0 {
		return nil
	}
	cp, ok := lossyCodepoint(ev, m)
	if !ok {
		return nil
	}
	if o.KittyFlags != 0 {
		// Kitty CSI-u: CSI unicode-key-code ; modifiers u
		return fmt.Appendf(nil, "\x1b[%d;%du", cp, modParam(m))
	}
	// xterm modifyOtherKeys: CSI 27 ; modifiers ; unicode-key-code ~
	return fmt.Appendf(nil, "\x1b[27;%d;%d~", modParam(m), cp)
}

// lossyCodepoint returns the Kitty/modifyOtherKeys unicode-key-code for a key
// event whose modifiers the legacy encoding would silently drop, and
// ok=false for every event the legacy path should keep handling. The code
// points match the decoder's table (see codepointKey): Enter 13, Escape 27,
// Backspace 127, Space 32, and a rune's own value.
func lossyCodepoint(ev Event, m Mod) (cp int, ok bool) {
	switch ev.Key {
	case KeyEnter: // legacy carries only Alt (ESC \r); shift/ctrl are dropped
		if m&(Shift|Ctrl) != 0 {
			return 13, true
		}
	case KeyEscape: // legacy carries only Alt (ESC ESC)
		if m&(Shift|Ctrl) != 0 {
			return 27, true
		}
	case KeyBackspace: // legacy carries Ctrl (BS) and Alt; shift is dropped
		if m&Shift != 0 {
			return 127, true
		}
	case KeySpace: // legacy carries Ctrl (NUL) and Alt; shift is dropped
		if m&Shift != 0 {
			return 32, true
		}
	case KeyRune:
		// A ctrl chord with no C0 byte (ctrl+/, ctrl+1, …) drops ctrl in the
		// legacy form. Runes that do have one (ctrl+a → 0x01) keep using it,
		// as do all unmodified and shifted-text runes.
		if m&Ctrl != 0 && ev.Rune > 0 {
			if _, hasC0 := ctrlByte(ev.Rune); !hasC0 {
				return int(ev.Rune), true
			}
		}
	}
	return 0, false
}

// EncodePaste renders pasted data for the pane: wrapped in 200~/201~
// markers when the pane has bracketed paste on, bare bytes otherwise. The
// payload is forwarded verbatim, escape bytes included — paste guards
// (confirm on control codes, per the spec's clipboard ruling) are the
// router's job, before this call.
func EncodePaste(data []byte, o EncodeOpts) []byte {
	if !o.BracketedPaste {
		return data
	}
	b := make([]byte, 0, len(pasteOpen)+len(data)+len(pasteClose))
	b = append(b, pasteOpen...)
	b = append(b, data...)
	return append(b, pasteClose...)
}

// MouseProto is the pane's mouse reporting mode (DECSET 9/1000/1002/1003).
// SGR framing (DECSET 1006) is tracked separately and layers on top.
type MouseProto int

const (
	MouseOff          MouseProto = iota
	MouseX10                     // DECSET 9: presses (and wheel) only, no modifiers
	MouseNormal                  // DECSET 1000: press, release, wheel
	MouseButtonMotion            // DECSET 1002: + motion while a button is held
	MouseAnyMotion               // DECSET 1003: + all motion
)

// EncodeMouse renders a mouse event (already translated to pane-local
// 0-based coords x, y) per the pane's reporting mode. Returns nil when the
// proto would not report this event: X10 reports presses only, Normal
// omits motion, ButtonMotion omits unbuttoned motion. Without SGR framing
// the legacy single-byte encoding clamps coordinates at 222 and cannot
// name the released button.
func EncodeMouse(ev Event, proto MouseProto, sgr bool, x, y int) []byte {
	if ev.Type != EvMouse || proto == MouseOff {
		return nil
	}
	switch ev.Mouse {
	case MousePress:
		if ev.Button < 1 || ev.Button > 3 {
			return nil
		}
	case MouseRelease:
		if proto == MouseX10 {
			return nil
		}
	case MouseMotion:
		switch proto {
		case MouseX10, MouseNormal:
			return nil
		case MouseButtonMotion:
			if ev.Button == 0 {
				return nil
			}
		}
	case MouseWheelUp, MouseWheelDown:
		// wheel reports in every active mode, as momentary presses
	default:
		return nil
	}

	b := 0
	switch ev.Mouse {
	case MousePress:
		b = ev.Button - 1
	case MouseRelease:
		if sgr && ev.Button >= 1 && ev.Button <= 3 {
			b = ev.Button - 1 // SGR keeps the button; the 'm' final marks release
		} else {
			b = 3
		}
	case MouseMotion:
		if ev.Button >= 1 && ev.Button <= 3 {
			b = 32 + ev.Button - 1
		} else {
			b = 32 + 3
		}
	case MouseWheelUp:
		b = 64
	case MouseWheelDown:
		b = 65
	}
	if proto != MouseX10 {
		if ev.Mods&Shift != 0 {
			b += 4
		}
		if ev.Mods&Alt != 0 {
			b += 8
		}
		if ev.Mods&Ctrl != 0 {
			b += 16
		}
	}
	x, y = max(x, 0), max(y, 0)
	if sgr {
		final := byte('M')
		if ev.Mouse == MouseRelease {
			final = 'm'
		}
		return fmt.Appendf(nil, "\x1b[<%d;%d;%d%c", b, x+1, y+1, final)
	}
	x, y = min(x, 222), min(y, 222)
	return []byte{0x1b, '[', 'M', byte(32 + b), byte(33 + x), byte(33 + y)}
}

// altPrefix renders the legacy alt encoding: ESC before the base bytes.
func altPrefix(b []byte, m Mod) []byte {
	if m&Alt != 0 {
		return append([]byte{0x1b}, b...)
	}
	return b
}

// modParam is the xterm modifier parameter: the Mod bitfield plus one.
func modParam(m Mod) int { return 1 + int(m&(Shift|Alt|Ctrl)) }

// cursorKey renders an arrow or Home/End: SS3 in application-cursor mode,
// CSI otherwise; any modifier forces the CSI 1;mods form regardless of
// DECCKM (xterm behavior).
func cursorKey(f byte, m Mod, app bool) []byte {
	if m == 0 {
		if app {
			return []byte{0x1b, 'O', f}
		}
		return []byte{0x1b, '[', f}
	}
	return fmt.Appendf(nil, "\x1b[1;%d%c", modParam(m), f)
}

// fnKey renders F1-F4, which are SS3 P..S in every mode (PC-style xterm).
func fnKey(f byte, m Mod) []byte {
	if m == 0 {
		return []byte{0x1b, 'O', f}
	}
	return fmt.Appendf(nil, "\x1b[1;%d%c", modParam(m), f)
}

func tildeKey(n int, m Mod) []byte {
	if m == 0 {
		return fmt.Appendf(nil, "\x1b[%d~", n)
	}
	return fmt.Appendf(nil, "\x1b[%d;%d~", n, modParam(m))
}

func encodeRune(r rune, m Mod) []byte {
	if r <= 0 || !utf8.ValidRune(r) {
		return nil
	}
	if m&Ctrl != 0 {
		if c, ok := ctrlByte(r); ok {
			return altPrefix([]byte{c}, m)
		}
		// no control-byte form exists (e.g. ctrl+1): ctrl is dropped, the
		// rune is sent bare — what a legacy keyboard does
	}
	return altPrefix(utf8.AppendRune(nil, r), m)
}

// ctrlByte maps a rune to its C0 control byte under ctrl: the inverse of
// ctrlRune plus the conventional aliases (ctrl+space/@ → NUL, ctrl+[ →
// ESC, ctrl+? → DEL).
func ctrlByte(r rune) (byte, bool) {
	if r >= 'A' && r <= 'Z' {
		r += 'a' - 'A'
	}
	switch {
	case r >= 'a' && r <= 'z':
		return byte(r - 'a' + 1), true
	case r == ' ' || r == '@':
		return 0x00, true
	case r == '[':
		return 0x1b, true
	case r == '\\':
		return 0x1c, true
	case r == ']':
		return 0x1d, true
	case r == '^':
		return 0x1e, true
	case r == '_':
		return 0x1f, true
	case r == '?':
		return 0x7f, true
	}
	return 0, false
}
