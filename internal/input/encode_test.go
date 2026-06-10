package input

import (
	"bytes"
	"testing"
)

func ke(k Key, r rune, m Mod) Event { return Event{Type: EvKey, Key: k, Rune: r, Mods: m} }

func TestEncodeKeyBytes(t *testing.T) {
	app := EncodeOpts{AppCursor: true}
	cases := []struct {
		ev   Event
		o    EncodeOpts
		want string
	}{
		{ke(KeyUp, 0, 0), EncodeOpts{}, "\x1b[A"},
		{ke(KeyUp, 0, 0), app, "\x1bOA"},
		{ke(KeyUp, 0, Ctrl), EncodeOpts{}, "\x1b[1;5A"},
		{ke(KeyUp, 0, Ctrl), app, "\x1b[1;5A"}, // mods force CSI even in app mode
		{ke(KeyHome, 0, 0), app, "\x1bOH"},
		{ke(KeyEnd, 0, Shift|Alt), EncodeOpts{}, "\x1b[1;4F"},
		{ke(KeyF1, 0, 0), EncodeOpts{}, "\x1bOP"},
		{ke(KeyF1, 0, Shift), EncodeOpts{}, "\x1b[1;2P"},
		{ke(KeyF5, 0, 0), EncodeOpts{}, "\x1b[15~"},
		{ke(KeyF5, 0, Alt), EncodeOpts{}, "\x1b[15;3~"},
		{ke(KeyF12, 0, Shift|Alt|Ctrl), EncodeOpts{}, "\x1b[24;8~"},
		{ke(KeyDelete, 0, 0), EncodeOpts{}, "\x1b[3~"},
		{ke(KeyDelete, 0, Ctrl), EncodeOpts{}, "\x1b[3;5~"},
		{ke(KeyEnter, 0, 0), EncodeOpts{}, "\r"},
		{ke(KeyEnter, 0, 0), EncodeOpts{CRLF: true}, "\r\n"},
		{ke(KeyEnter, 0, Alt), EncodeOpts{}, "\x1b\r"},
		{ke(KeyTab, 0, 0), EncodeOpts{}, "\t"},
		{ke(KeyTab, 0, Alt), EncodeOpts{}, "\x1b\t"},
		{ke(KeyTab, 0, Shift), EncodeOpts{}, "\x1b[Z"},
		{ke(KeyTab, 0, Shift|Ctrl), EncodeOpts{}, "\x1b[1;6Z"},
		{ke(KeyBackspace, 0, 0), EncodeOpts{}, "\x7f"},
		{ke(KeyBackspace, 0, Alt), EncodeOpts{}, "\x1b\x7f"},
		{ke(KeyEscape, 0, 0), EncodeOpts{}, "\x1b"},
		{ke(KeyEscape, 0, Alt), EncodeOpts{}, "\x1b\x1b"},
		{ke(KeySpace, 0, 0), EncodeOpts{}, " "},
		{ke(KeySpace, 0, Ctrl), EncodeOpts{}, "\x00"},
		{ke(KeySpace, 0, Ctrl|Alt), EncodeOpts{}, "\x1b\x00"},
		{ke(KeyRune, 'a', 0), EncodeOpts{}, "a"},
		{ke(KeyRune, 'a', Alt), EncodeOpts{}, "\x1ba"},
		{ke(KeyRune, 'a', Ctrl), EncodeOpts{}, "\x01"},
		{ke(KeyRune, 'z', Ctrl|Alt), EncodeOpts{}, "\x1b\x1a"},
		{ke(KeyRune, '\\', Ctrl), EncodeOpts{}, "\x1c"},
		{ke(KeyRune, 'é', Alt), EncodeOpts{}, "\x1bé"},
		{ke(KeyRune, '日', 0), EncodeOpts{}, "日"},
	}
	for _, c := range cases {
		got := EncodeKey(c.ev, c.o)
		if string(got) != c.want {
			t.Errorf("%s opts %+v: got %q, want %q", fmtEvent(c.ev), c.o, got, c.want)
		}
	}
}

// legacy encodings cannot carry every modifier; pin the documented
// behavior of a directly-attached terminal for the lossy combinations.
func TestEncodeLossyLegacy(t *testing.T) {
	cases := []struct {
		ev   Event
		want string
	}{
		{ke(KeyEnter, 0, Ctrl), "\r"},
		{ke(KeyEnter, 0, Shift), "\r"},
		{ke(KeyTab, 0, Ctrl), "\t"},
		// ctrl+backspace sends BS where plain backspace sends DEL; the
		// encoding is pinned here rather than round-tripped because BS
		// collides with ctrl+h on decode (the legacy collision)
		{ke(KeyBackspace, 0, Ctrl), "\x08"},
		{ke(KeyBackspace, 0, Ctrl|Alt), "\x1b\x08"},
		{ke(KeySpace, 0, Shift), " "},
		{ke(KeyRune, 'A', Ctrl), "\x01"}, // case is lost under ctrl
		{ke(KeyRune, '1', Ctrl), "1"},    // no control byte exists: ctrl dropped
		{ke(KeyRune, 'a', Shift), "a"},   // shift lives in the rune itself
		{ke(KeyRune, '[', Ctrl), "\x1b"}, // ctrl+[ is escape
		{ke(KeyRune, '?', Ctrl), "\x7f"}, // ctrl+? is del
		{ke(KeyRune, '@', Ctrl), "\x00"},
	}
	for _, c := range cases {
		got := EncodeKey(c.ev, EncodeOpts{})
		if string(got) != c.want {
			t.Errorf("%s: got %q, want %q", fmtEvent(c.ev), got, c.want)
		}
	}
}

func TestEncodeKeyNil(t *testing.T) {
	cases := []Event{
		{Type: EvMouse, Mouse: MousePress, Button: 1},
		{Type: EvFocus, Gained: true},
		{Type: EvPaste, Paste: []byte("x")},
		{Type: EvUnknown, Raw: []byte("\x1b[?1;2c")},
		ke(Key(999), 0, 0),       // out-of-range key
		ke(KeyRune, 0, 0),        // no rune
		ke(KeyRune, -1, 0),       // invalid rune
		ke(KeyRune, 0xd800, Alt), // surrogate
	}
	for _, ev := range cases {
		if got := EncodeKey(ev, EncodeOpts{}); got != nil {
			t.Errorf("%s: got %q, want nil", fmtEvent(ev), got)
		}
	}
}

func TestEncodePaste(t *testing.T) {
	data := []byte("line1\nline2\x1bx")
	got := EncodePaste(data, EncodeOpts{BracketedPaste: true})
	want := "\x1b[200~line1\nline2\x1bx\x1b[201~"
	if string(got) != want {
		t.Errorf("bracketed: got %q, want %q", got, want)
	}
	got = EncodePaste(data, EncodeOpts{})
	if !bytes.Equal(got, data) {
		t.Errorf("bare: got %q, want %q", got, data)
	}
}

func mev(mt MouseType, btn int, m Mod) Event {
	return Event{Type: EvMouse, Mouse: mt, Button: btn, Mods: m}
}

func TestEncodeMouseGating(t *testing.T) {
	press := mev(MousePress, 1, 0)
	release := mev(MouseRelease, 1, 0)
	motionBtn := mev(MouseMotion, 1, 0)
	motionNone := mev(MouseMotion, 0, 0)
	wheel := mev(MouseWheelUp, 0, 0)

	cases := []struct {
		ev     Event
		proto  MouseProto
		report bool
	}{
		{press, MouseOff, false},
		{wheel, MouseOff, false},
		{press, MouseX10, true},
		{release, MouseX10, false},
		{motionBtn, MouseX10, false},
		{motionNone, MouseX10, false},
		{wheel, MouseX10, true},
		{press, MouseNormal, true},
		{release, MouseNormal, true},
		{motionBtn, MouseNormal, false},
		{motionNone, MouseNormal, false},
		{wheel, MouseNormal, true},
		{press, MouseButtonMotion, true},
		{release, MouseButtonMotion, true},
		{motionBtn, MouseButtonMotion, true},
		{motionNone, MouseButtonMotion, false},
		{wheel, MouseButtonMotion, true},
		{press, MouseAnyMotion, true},
		{release, MouseAnyMotion, true},
		{motionBtn, MouseAnyMotion, true},
		{motionNone, MouseAnyMotion, true},
		{wheel, MouseAnyMotion, true},
	}
	for _, c := range cases {
		for _, sgr := range []bool{false, true} {
			got := EncodeMouse(c.ev, c.proto, sgr, 0, 0)
			if (got != nil) != c.report {
				t.Errorf("%s proto %d sgr %v: got %q, want reported=%v",
					fmtEvent(c.ev), c.proto, sgr, got, c.report)
			}
		}
	}

	// a press without a valid button cannot be encoded in any mode
	if got := EncodeMouse(mev(MousePress, 0, 0), MouseAnyMotion, true, 0, 0); got != nil {
		t.Errorf("buttonless press: got %q, want nil", got)
	}
}

func TestEncodeMouseBytes(t *testing.T) {
	cases := []struct {
		ev    Event
		proto MouseProto
		sgr   bool
		x, y  int
		want  string
	}{
		{mev(MousePress, 1, 0), MouseNormal, true, 0, 0, "\x1b[<0;1;1M"},
		{mev(MouseRelease, 1, 0), MouseNormal, true, 0, 0, "\x1b[<0;1;1m"},
		{mev(MousePress, 3, Ctrl), MouseNormal, true, 10, 5, "\x1b[<18;11;6M"},
		{mev(MouseWheelUp, 0, Ctrl), MouseNormal, true, 0, 0, "\x1b[<80;1;1M"},
		{mev(MouseMotion, 1, 0), MouseButtonMotion, true, 2, 3, "\x1b[<32;3;4M"},
		{mev(MouseMotion, 0, 0), MouseAnyMotion, true, 2, 3, "\x1b[<35;3;4M"},
		{mev(MousePress, 1, 0), MouseNormal, false, 0, 0, "\x1b[M\x20\x21\x21"},
		{mev(MousePress, 2, 0), MouseNormal, false, 0, 0, "\x1b[M\x21\x21\x21"},
		{mev(MouseRelease, 1, 0), MouseNormal, false, 0, 0, "\x1b[M\x23\x21\x21"}, // button lost: 3
		{mev(MouseWheelDown, 0, 0), MouseNormal, false, 0, 0, "\x1b[M\x61\x21\x21"},
		// X10 omits modifiers
		{mev(MousePress, 1, Ctrl|Shift), MouseX10, false, 0, 0, "\x1b[M\x20\x21\x21"},
		{mev(MousePress, 1, Ctrl|Shift), MouseX10, true, 0, 0, "\x1b[<0;1;1M"},
		// legacy byte coords clamp at 222; SGR does not
		{mev(MousePress, 1, 0), MouseNormal, false, 500, 0, "\x1b[M\x20\xff\x21"},
		{mev(MousePress, 1, 0), MouseNormal, true, 500, 600, "\x1b[<0;501;601M"},
	}
	for _, c := range cases {
		got := EncodeMouse(c.ev, c.proto, c.sgr, c.x, c.y)
		if string(got) != c.want {
			t.Errorf("%s proto %d sgr %v at (%d,%d): got %q, want %q",
				fmtEvent(c.ev), c.proto, c.sgr, c.x, c.y, got, c.want)
		}
	}
}
