package input

import (
	"bytes"
	"fmt"
	"testing"
)

// fmtEvent renders an event compactly for failure messages.
func fmtEvent(e Event) string {
	return fmt.Sprintf(
		"{Type:%d Key:%d Rune:%q Mods:%d Mouse:%d Btn:%d X:%d Y:%d Paste:%q Gained:%v Raw:%q}",
		e.Type, e.Key, e.Rune, e.Mods, e.Mouse, e.Button, e.X, e.Y, e.Paste, e.Gained, e.Raw)
}

func eventsEqual(a, b Event) bool {
	return a.Type == b.Type && a.Key == b.Key && a.Rune == b.Rune &&
		a.Mods == b.Mods && a.Mouse == b.Mouse && a.Button == b.Button &&
		a.X == b.X && a.Y == b.Y && bytes.Equal(a.Paste, b.Paste) &&
		a.Gained == b.Gained && bytes.Equal(a.Raw, b.Raw)
}

// decodeOne feeds b into a fresh decoder (flushing any trailing ambiguity,
// as the router's idle timer would) and requires exactly one event.
func decodeOne(t *testing.T, b []byte) Event {
	t.Helper()
	d := NewDecoder()
	evs := d.Feed(b)
	evs = append(evs, d.Flush()...)
	if len(evs) != 1 {
		t.Fatalf("input %q: got %d events, want 1: %v", b, len(evs), evs)
	}
	if d.Pending() {
		t.Fatalf("input %q: decoder still pending after flush", b)
	}
	return evs[0]
}

func wantKey(k Key, r rune, m Mod, raw string) Event {
	return Event{Type: EvKey, Key: k, Rune: r, Mods: m, Raw: []byte(raw)}
}

func TestC0Controls(t *testing.T) {
	cases := []struct {
		in   string
		want Event
	}{
		{"\x00", wantKey(KeySpace, 0, Ctrl, "\x00")},
		{"\x01", wantKey(KeyRune, 'a', Ctrl, "\x01")},
		{"\x03", wantKey(KeyRune, 'c', Ctrl, "\x03")},
		{"\x08", wantKey(KeyRune, 'h', Ctrl, "\x08")},
		{"\x09", wantKey(KeyTab, 0, 0, "\x09")},
		{"\x0a", wantKey(KeyRune, 'j', Ctrl, "\x0a")},
		{"\x0d", wantKey(KeyEnter, 0, 0, "\x0d")},
		{"\x1a", wantKey(KeyRune, 'z', Ctrl, "\x1a")},
		{"\x1c", wantKey(KeyRune, '\\', Ctrl, "\x1c")},
		{"\x1d", wantKey(KeyRune, ']', Ctrl, "\x1d")},
		{"\x1e", wantKey(KeyRune, '^', Ctrl, "\x1e")},
		{"\x1f", wantKey(KeyRune, '_', Ctrl, "\x1f")},
		{"\x7f", wantKey(KeyBackspace, 0, 0, "\x7f")},
	}
	for _, c := range cases {
		if got := decodeOne(t, []byte(c.in)); !eventsEqual(got, c.want) {
			t.Errorf("%q: got %s, want %s", c.in, fmtEvent(got), fmtEvent(c.want))
		}
	}
}

func TestRunes(t *testing.T) {
	d := NewDecoder()
	evs := d.Feed([]byte("aZ0é日"))
	want := []Event{
		wantKey(KeyRune, 'a', 0, "a"),
		wantKey(KeyRune, 'Z', 0, "Z"),
		wantKey(KeyRune, '0', 0, "0"),
		wantKey(KeyRune, 'é', 0, "é"),
		wantKey(KeyRune, '日', 0, "日"),
	}
	if len(evs) != len(want) {
		t.Fatalf("got %d events, want %d", len(evs), len(want))
	}
	for i := range want {
		if !eventsEqual(evs[i], want[i]) {
			t.Errorf("event %d: got %s, want %s", i, fmtEvent(evs[i]), fmtEvent(want[i]))
		}
	}
}

func TestUTF8AcrossFeeds(t *testing.T) {
	d := NewDecoder()
	var evs []Event
	for _, b := range []byte("日") { // 3-byte rune, fed a byte at a time
		evs = append(evs, d.Feed([]byte{b})...)
	}
	if len(evs) != 1 || !eventsEqual(evs[0], wantKey(KeyRune, '日', 0, "日")) {
		t.Fatalf("got %v", evs)
	}
	if d.Pending() {
		t.Fatal("pending after complete rune")
	}
}

func TestAltPrefixed(t *testing.T) {
	cases := []struct {
		in   string
		want Event
	}{
		{"\x1ba", wantKey(KeyRune, 'a', Alt, "\x1ba")},
		{"\x1bé", wantKey(KeyRune, 'é', Alt, "\x1bé")},
		{"\x1b\x1b", wantKey(KeyEscape, 0, Alt, "\x1b\x1b")},
		{"\x1b\x0d", wantKey(KeyEnter, 0, Alt, "\x1b\x0d")},
		{"\x1b\x09", wantKey(KeyTab, 0, Alt, "\x1b\x09")},
		{"\x1b\x7f", wantKey(KeyBackspace, 0, Alt, "\x1b\x7f")},
		{"\x1b\x00", wantKey(KeySpace, 0, Ctrl|Alt, "\x1b\x00")},
		{"\x1b\x01", wantKey(KeyRune, 'a', Ctrl|Alt, "\x1b\x01")},
		{"\x1b ", wantKey(KeyRune, ' ', Alt, "\x1b ")},
	}
	for _, c := range cases {
		if got := decodeOne(t, []byte(c.in)); !eventsEqual(got, c.want) {
			t.Errorf("%q: got %s, want %s", c.in, fmtEvent(got), fmtEvent(c.want))
		}
	}
}

func TestLegacyKeys(t *testing.T) {
	cases := []struct {
		in   string
		want Event
	}{
		{"\x1b[A", wantKey(KeyUp, 0, 0, "\x1b[A")},
		{"\x1b[B", wantKey(KeyDown, 0, 0, "\x1b[B")},
		{"\x1b[C", wantKey(KeyRight, 0, 0, "\x1b[C")},
		{"\x1b[D", wantKey(KeyLeft, 0, 0, "\x1b[D")},
		{"\x1bOA", wantKey(KeyUp, 0, 0, "\x1bOA")},
		{"\x1bOD", wantKey(KeyLeft, 0, 0, "\x1bOD")},
		{"\x1b[H", wantKey(KeyHome, 0, 0, "\x1b[H")},
		{"\x1b[F", wantKey(KeyEnd, 0, 0, "\x1b[F")},
		{"\x1bOH", wantKey(KeyHome, 0, 0, "\x1bOH")},
		{"\x1bOF", wantKey(KeyEnd, 0, 0, "\x1bOF")},
		{"\x1b[1~", wantKey(KeyHome, 0, 0, "\x1b[1~")},
		{"\x1b[4~", wantKey(KeyEnd, 0, 0, "\x1b[4~")},
		{"\x1b[2~", wantKey(KeyInsert, 0, 0, "\x1b[2~")},
		{"\x1b[3~", wantKey(KeyDelete, 0, 0, "\x1b[3~")},
		{"\x1b[5~", wantKey(KeyPageUp, 0, 0, "\x1b[5~")},
		{"\x1b[6~", wantKey(KeyPageDown, 0, 0, "\x1b[6~")},
		{"\x1bOP", wantKey(KeyF1, 0, 0, "\x1bOP")},
		{"\x1bOS", wantKey(KeyF4, 0, 0, "\x1bOS")},
		{"\x1b[11~", wantKey(KeyF1, 0, 0, "\x1b[11~")},
		{"\x1b[14~", wantKey(KeyF4, 0, 0, "\x1b[14~")},
		{"\x1b[15~", wantKey(KeyF5, 0, 0, "\x1b[15~")},
		{"\x1b[17~", wantKey(KeyF6, 0, 0, "\x1b[17~")},
		{"\x1b[24~", wantKey(KeyF12, 0, 0, "\x1b[24~")},
		{"\x1b[Z", wantKey(KeyTab, 0, Shift, "\x1b[Z")},
		// modifier params: mods-1 is 1=shift 2=alt 4=ctrl
		{"\x1b[1;2A", wantKey(KeyUp, 0, Shift, "\x1b[1;2A")},
		{"\x1b[1;3C", wantKey(KeyRight, 0, Alt, "\x1b[1;3C")},
		{"\x1b[1;5D", wantKey(KeyLeft, 0, Ctrl, "\x1b[1;5D")},
		{"\x1b[1;8B", wantKey(KeyDown, 0, Shift|Alt|Ctrl, "\x1b[1;8B")},
		{"\x1b[1;5H", wantKey(KeyHome, 0, Ctrl, "\x1b[1;5H")},
		{"\x1b[1;2P", wantKey(KeyF1, 0, Shift, "\x1b[1;2P")},
		{"\x1b[3;5~", wantKey(KeyDelete, 0, Ctrl, "\x1b[3;5~")},
		{"\x1b[24;8~", wantKey(KeyF12, 0, Shift|Alt|Ctrl, "\x1b[24;8~")},
		{"\x1b[1;6Z", wantKey(KeyTab, 0, Shift|Ctrl, "\x1b[1;6Z")},
	}
	for _, c := range cases {
		if got := decodeOne(t, []byte(c.in)); !eventsEqual(got, c.want) {
			t.Errorf("%q: got %s, want %s", c.in, fmtEvent(got), fmtEvent(c.want))
		}
	}
}

func TestKittyU(t *testing.T) {
	cases := []struct {
		in   string
		want Event
	}{
		{"\x1b[97u", wantKey(KeyRune, 'a', 0, "\x1b[97u")},
		{"\x1b[97;5u", wantKey(KeyRune, 'a', Ctrl, "\x1b[97;5u")},
		{"\x1b[97;8u", wantKey(KeyRune, 'a', Shift|Alt|Ctrl, "\x1b[97;8u")},
		{"\x1b[97:65;2u", wantKey(KeyRune, 'a', Shift, "\x1b[97:65;2u")}, // shifted alternate
		{"\x1b[13;3u", wantKey(KeyEnter, 0, Alt, "\x1b[13;3u")},
		{"\x1b[9;5u", wantKey(KeyTab, 0, Ctrl, "\x1b[9;5u")},
		{"\x1b[27;1u", wantKey(KeyEscape, 0, 0, "\x1b[27;1u")},
		{"\x1b[127;5u", wantKey(KeyBackspace, 0, Ctrl, "\x1b[127;5u")},
		{"\x1b[32;5u", wantKey(KeySpace, 0, Ctrl, "\x1b[32;5u")},
		{"\x1b[32;1u", wantKey(KeyRune, ' ', 0, "\x1b[32;1u")},
		{"\x1b[97;5:1u", wantKey(KeyRune, 'a', Ctrl, "\x1b[97;5:1u")}, // explicit press
	}
	for _, c := range cases {
		if got := decodeOne(t, []byte(c.in)); !eventsEqual(got, c.want) {
			t.Errorf("%q: got %s, want %s", c.in, fmtEvent(got), fmtEvent(c.want))
		}
	}

	// repeat (:2) and release (:3) are consumed silently, on CSI-u and on
	// the letter-final kitty forms alike
	for _, in := range []string{"\x1b[97;5:3u", "\x1b[97;1:2u", "\x1b[13;1:3u", "\x1b[1;5:3A", "\x1b[3;5:2~"} {
		d := NewDecoder()
		evs := d.Feed([]byte(in))
		evs = append(evs, d.Flush()...)
		if len(evs) != 0 {
			t.Errorf("%q: got %v, want no events", in, evs)
		}
		if d.Pending() {
			t.Errorf("%q: still pending", in)
		}
	}

	// kitty functional code points (numpad and friends) are EvUnknown
	for _, in := range []string{"\x1b[57399;1u", "\x1b[57441u"} {
		got := decodeOne(t, []byte(in))
		if got.Type != EvUnknown || string(got.Raw) != in {
			t.Errorf("%q: got %s, want EvUnknown with exact raw", in, fmtEvent(got))
		}
	}
}

func TestFocus(t *testing.T) {
	got := decodeOne(t, []byte("\x1b[I"))
	if got.Type != EvFocus || !got.Gained || string(got.Raw) != "\x1b[I" {
		t.Errorf("focus in: got %s", fmtEvent(got))
	}
	got = decodeOne(t, []byte("\x1b[O"))
	if got.Type != EvFocus || got.Gained || string(got.Raw) != "\x1b[O" {
		t.Errorf("focus out: got %s", fmtEvent(got))
	}
}

func TestSGRMouseMatrix(t *testing.T) {
	modBits := func(m Mod) int {
		b := 0
		if m&Shift != 0 {
			b += 4
		}
		if m&Alt != 0 {
			b += 8
		}
		if m&Ctrl != 0 {
			b += 16
		}
		return b
	}
	x, y := 10, 5 // 0-based
	for _, m := range allModCombos {
		mb := modBits(m)
		type mc struct {
			b     int
			final byte
			want  Event
		}
		var cases []mc
		for btn := 1; btn <= 3; btn++ {
			cases = append(cases,
				mc{btn - 1 + mb, 'M', Event{Type: EvMouse, Mouse: MousePress, Button: btn, X: x, Y: y, Mods: m}},
				mc{btn - 1 + mb, 'm', Event{Type: EvMouse, Mouse: MouseRelease, Button: btn, X: x, Y: y, Mods: m}},
				mc{32 + btn - 1 + mb, 'M', Event{Type: EvMouse, Mouse: MouseMotion, Button: btn, X: x, Y: y, Mods: m}},
			)
		}
		cases = append(cases,
			mc{32 + 3 + mb, 'M', Event{Type: EvMouse, Mouse: MouseMotion, Button: 0, X: x, Y: y, Mods: m}},
			mc{64 + mb, 'M', Event{Type: EvMouse, Mouse: MouseWheelUp, Button: 0, X: x, Y: y, Mods: m}},
			mc{65 + mb, 'M', Event{Type: EvMouse, Mouse: MouseWheelDown, Button: 0, X: x, Y: y, Mods: m}},
		)
		for _, c := range cases {
			in := fmt.Sprintf("\x1b[<%d;%d;%d%c", c.b, x+1, y+1, c.final)
			c.want.Raw = []byte(in)
			got := decodeOne(t, []byte(in))
			if !eventsEqual(got, c.want) {
				t.Errorf("%q: got %s, want %s", in, fmtEvent(got), fmtEvent(c.want))
			}
		}
	}
}

// x10 builds a legacy mouse report: CSI M with three offset payload bytes.
func x10(b, x, y int) string {
	return string([]byte{0x1b, '[', 'M', byte(32 + b), byte(32 + x), byte(32 + y)})
}

func TestLegacyX10Mouse(t *testing.T) {
	cases := []struct {
		in   string
		want Event
	}{
		{x10(0, 1, 1), Event{Type: EvMouse, Mouse: MousePress, Button: 1, X: 0, Y: 0}},
		{x10(1, 11, 6), Event{Type: EvMouse, Mouse: MousePress, Button: 2, X: 10, Y: 5}},
		{x10(2, 3, 4), Event{Type: EvMouse, Mouse: MousePress, Button: 3, X: 2, Y: 3}},
		// a release cannot name its button in this encoding
		{x10(3, 1, 1), Event{Type: EvMouse, Mouse: MouseRelease, Button: 0, X: 0, Y: 0}},
		// modifier bits: +4 shift, +8 alt, +16 ctrl
		{x10(4, 1, 1), Event{Type: EvMouse, Mouse: MousePress, Button: 1, Mods: Shift}},
		{x10(8, 1, 1), Event{Type: EvMouse, Mouse: MousePress, Button: 1, Mods: Alt}},
		{x10(16, 1, 1), Event{Type: EvMouse, Mouse: MousePress, Button: 1, Mods: Ctrl}},
		{x10(28, 1, 1), Event{Type: EvMouse, Mouse: MousePress, Button: 1, Mods: Shift | Alt | Ctrl}},
		{x10(3+16, 1, 1), Event{Type: EvMouse, Mouse: MouseRelease, Button: 0, Mods: Ctrl}},
		// +32 motion, with a held button or none
		{x10(32, 5, 5), Event{Type: EvMouse, Mouse: MouseMotion, Button: 1, X: 4, Y: 4}},
		{x10(32+2, 5, 5), Event{Type: EvMouse, Mouse: MouseMotion, Button: 3, X: 4, Y: 4}},
		{x10(32+3, 5, 5), Event{Type: EvMouse, Mouse: MouseMotion, Button: 0, X: 4, Y: 4}},
		// wheel
		{x10(64, 2, 2), Event{Type: EvMouse, Mouse: MouseWheelUp, X: 1, Y: 1}},
		{x10(65, 2, 2), Event{Type: EvMouse, Mouse: MouseWheelDown, X: 1, Y: 1}},
		// the maximum legacy coordinate, byte 255
		{x10(0, 223, 223), Event{Type: EvMouse, Mouse: MousePress, Button: 1, X: 222, Y: 222}},
		// coordinate bytes a wrapping terminal pushed below 0x20 clamp at 0
		{string([]byte{0x1b, '[', 'M', 32, 0, 10}), Event{Type: EvMouse, Mouse: MousePress, Button: 1, X: 0, Y: 0}},
	}
	for _, c := range cases {
		c.want.Raw = []byte(c.in)
		got := decodeOne(t, []byte(c.in))
		if !eventsEqual(got, c.want) {
			t.Errorf("%q: got %s, want %s", c.in, fmtEvent(got), fmtEvent(c.want))
		}
	}

	// wheel left/right are not modeled: EvUnknown, still consuming all 6 bytes
	for _, b := range []int{66, 67} {
		in := x10(b, 1, 1)
		got := decodeOne(t, []byte(in))
		if got.Type != EvUnknown || string(got.Raw) != in {
			t.Errorf("%q: got %s, want EvUnknown carrying it verbatim", in, fmtEvent(got))
		}
	}
}

func TestLegacyX10MouseSplitFeeds(t *testing.T) {
	in := []byte(x10(0, 11, 6))
	want := Event{Type: EvMouse, Mouse: MousePress, Button: 1, X: 10, Y: 5, Raw: in}
	for cut := 0; cut <= len(in); cut++ {
		d := NewDecoder()
		evs := d.Feed(in[:cut])
		evs = append(evs, d.Feed(in[cut:])...)
		if d.Pending() {
			t.Fatalf("cut %d: still pending", cut)
		}
		if len(evs) != 1 || !eventsEqual(evs[0], want) {
			t.Fatalf("cut %d: got %v, want %s", cut, evs, fmtEvent(want))
		}
	}
}

func TestLegacyX10DragStream(t *testing.T) {
	// press, drag, drag, release — what a 1002 terminal without SGR
	// support sends for a left-button drag
	in := []byte(x10(0, 2, 2) + x10(32, 3, 2) + x10(32, 4, 3) + x10(3, 4, 3))
	want := []Event{
		{Type: EvMouse, Mouse: MousePress, Button: 1, X: 1, Y: 1},
		{Type: EvMouse, Mouse: MouseMotion, Button: 1, X: 2, Y: 1},
		{Type: EvMouse, Mouse: MouseMotion, Button: 1, X: 3, Y: 2},
		{Type: EvMouse, Mouse: MouseRelease, Button: 0, X: 3, Y: 2},
	}
	d := NewDecoder()
	evs := d.Feed(in)
	if d.Pending() {
		t.Fatal("still pending after drag stream")
	}
	if len(evs) != len(want) {
		t.Fatalf("got %d events, want %d: %v", len(evs), len(want), evs)
	}
	for i := range want {
		want[i].Raw = in[i*6 : i*6+6]
		if !eventsEqual(evs[i], want[i]) {
			t.Errorf("event %d: got %s, want %s", i, fmtEvent(evs[i]), fmtEvent(want[i]))
		}
	}
}

func TestModifyOtherKeys(t *testing.T) {
	cases := []struct {
		in   string
		want Event
	}{
		{"\x1b[27;5;13~", wantKey(KeyEnter, 0, Ctrl, "\x1b[27;5;13~")},
		{"\x1b[27;6;9~", wantKey(KeyTab, 0, Shift|Ctrl, "\x1b[27;6;9~")},
		{"\x1b[27;5;99~", wantKey(KeyRune, 'c', Ctrl, "\x1b[27;5;99~")},
		{"\x1b[27;3;27~", wantKey(KeyEscape, 0, Alt, "\x1b[27;3;27~")},
		{"\x1b[27;5;127~", wantKey(KeyBackspace, 0, Ctrl, "\x1b[27;5;127~")},
		{"\x1b[27;2;32~", wantKey(KeySpace, 0, Shift, "\x1b[27;2;32~")},
		{"\x1b[27;1;13~", wantKey(KeyEnter, 0, 0, "\x1b[27;1;13~")},
	}
	for _, c := range cases {
		if got := decodeOne(t, []byte(c.in)); !eventsEqual(got, c.want) {
			t.Errorf("%q: got %s, want %s", c.in, fmtEvent(got), fmtEvent(c.want))
		}
	}

	// malformed (a fourth field) and bare CSI 27~ forms stay EvUnknown
	for _, in := range []string{"\x1b[27;5;13;1~", "\x1b[27~", "\x1b[27;5~"} {
		got := decodeOne(t, []byte(in))
		if got.Type != EvUnknown || string(got.Raw) != in {
			t.Errorf("%q: got %s, want EvUnknown carrying it verbatim", in, fmtEvent(got))
		}
	}
}

func TestBracketedPaste(t *testing.T) {
	// payload containing a bare ESC, a CSI-like sequence, and a prefix of
	// the terminator — none of which may end the paste early
	payload := "a\x1bb\x1b[5mc\x1b[201x"
	in := "\x1b[200~" + payload + "\x1b[201~"
	got := decodeOne(t, []byte(in))
	if got.Type != EvPaste || string(got.Paste) != payload || string(got.Raw) != in {
		t.Fatalf("got %s", fmtEvent(got))
	}

	// empty paste
	got = decodeOne(t, []byte("\x1b[200~\x1b[201~"))
	if got.Type != EvPaste || len(got.Paste) != 0 {
		t.Fatalf("empty paste: got %s", fmtEvent(got))
	}
}

func TestPasteSpanningFeeds(t *testing.T) {
	payload := "line1\nline2\x1b[31mred\x1b"
	in := []byte("\x1b[200~" + payload + "\x1b[201~")
	d := NewDecoder()
	var evs []Event
	for _, b := range in { // one byte per Feed
		evs = append(evs, d.Feed([]byte{b})...)
	}
	if len(evs) != 1 || evs[0].Type != EvPaste || string(evs[0].Paste) != payload {
		t.Fatalf("got %v", evs)
	}
	if !bytes.Equal(evs[0].Raw, in) {
		t.Fatalf("raw: got %q", evs[0].Raw)
	}
	if d.Pending() {
		t.Fatal("pending after paste end")
	}
}

func TestPasteCap(t *testing.T) {
	d := NewDecoder()
	var evs []Event
	evs = append(evs, d.Feed([]byte("\x1b[200~"))...)
	chunk := bytes.Repeat([]byte{'x'}, 1<<20)
	total := 0
	for total < maxPaste+3*len(chunk) { // 3 MiB over the cap
		evs = append(evs, d.Feed(chunk)...)
		total += len(chunk)
	}
	if !d.Pending() {
		t.Fatal("paste should still be open")
	}
	evs = append(evs, d.Feed([]byte("\x1b[201~done"))...)
	if len(evs) != 5 {
		t.Fatalf("got %d events, want paste + 'done'", len(evs))
	}
	if evs[0].Type != EvPaste || len(evs[0].Paste) != maxPaste {
		t.Fatalf("paste len %d, want cap %d", len(evs[0].Paste), maxPaste)
	}
	// the stream stays in sync after the drop
	for i, r := range "done" {
		if evs[1+i].Type != EvKey || evs[1+i].Rune != r {
			t.Fatalf("post-paste event %d: %s", i, fmtEvent(evs[1+i]))
		}
	}
}

func TestUnknownSequences(t *testing.T) {
	cases := []string{
		"\x1b[?2004h",             // private-mode set echoed back
		"\x1b[?1;2c",              // DA1 reply
		"\x1b[12;40R",             // CPR reply
		"\x1b]52;c;Zm9v\x07",      // OSC, BEL-terminated
		"\x1b]11;rgb:aa/bb\x1b\\", // OSC, ST-terminated
		"\x1bP1+r544e\x1b\\",      // DCS (XTGETTCAP reply)
		"\x1b_Gi=1;OK\x1b\\",      // APC (kitty graphics reply)
		"\x1bOz",                  // well-formed SS3 we do not recognize
		"\x1b[201~",               // stray paste terminator
		"\x1b[25~",                // unmapped tilde key
		"\x1b[E",                  // unmapped CSI final
	}
	for _, in := range cases {
		got := decodeOne(t, []byte(in))
		if got.Type != EvUnknown || string(got.Raw) != in {
			t.Errorf("%q: got %s, want EvUnknown carrying it verbatim", in, fmtEvent(got))
		}
	}
}

func TestMalformedCSIDoesNotEatControls(t *testing.T) {
	// a C0 byte inside a CSI aborts it: the prefix surfaces as EvUnknown
	// and the control (here ctrl+c, which must never be lost) still decodes
	d := NewDecoder()
	evs := d.Feed([]byte("\x1b[1;\x03"))
	if len(evs) != 2 {
		t.Fatalf("got %d events: %v", len(evs), evs)
	}
	if evs[0].Type != EvUnknown || string(evs[0].Raw) != "\x1b[1;" {
		t.Errorf("prefix: got %s", fmtEvent(evs[0]))
	}
	if !eventsEqual(evs[1], wantKey(KeyRune, 'c', Ctrl, "\x03")) {
		t.Errorf("ctrl+c: got %s", fmtEvent(evs[1]))
	}
}

func TestSplitFeeds(t *testing.T) {
	seqs := [][]byte{
		[]byte("\x1b[1;5A"),
		[]byte("\x1bOP"),
		[]byte("\x1b[<0;11;6M"),
		[]byte("\x1b[<35;2;3m"),
		[]byte(x10(0, 11, 6)),
		[]byte("\x1b[97;5u"),
		[]byte("é日"),
		[]byte("\x1b[200~a\x1bb\x1b[201~"),
		[]byte("\x1b]52;c;Zm9v\x07"),
		[]byte("\x1b]11;rgb:aa/bb\x1b\\"),
		[]byte("\x1b[I"),
		[]byte("\x1b[3;7~"),
		[]byte("\x1b\x1b"),
		[]byte("\x1bx"),
	}
	var stream []byte
	for _, s := range seqs {
		stream = append(stream, s...)
	}
	want := NewDecoder().Feed(stream)
	if len(want) == 0 {
		t.Fatal("no events from whole stream")
	}
	var wantRaw []byte
	for _, e := range want {
		wantRaw = append(wantRaw, e.Raw...)
	}
	if !bytes.Equal(wantRaw, stream) {
		t.Fatalf("raw concatenation does not reproduce the stream:\n got %q\nwant %q", wantRaw, stream)
	}

	for cut := 0; cut <= len(stream); cut++ {
		d := NewDecoder()
		got := d.Feed(stream[:cut])
		got = append(got, d.Feed(stream[cut:])...)
		if d.Pending() {
			t.Fatalf("cut %d: still pending", cut)
		}
		if len(got) != len(want) {
			t.Fatalf("cut %d: got %d events, want %d", cut, len(got), len(want))
		}
		for i := range want {
			if !eventsEqual(got[i], want[i]) {
				t.Fatalf("cut %d event %d: got %s, want %s", cut, i, fmtEvent(got[i]), fmtEvent(want[i]))
			}
		}
	}
}

func TestFlush(t *testing.T) {
	// lone ESC: Feed holds it, Flush resolves it as the escape key
	d := NewDecoder()
	if evs := d.Feed([]byte{0x1b}); len(evs) != 0 {
		t.Fatalf("lone ESC produced %v", evs)
	}
	if !d.Pending() {
		t.Fatal("lone ESC: not pending")
	}
	evs := d.Flush()
	if len(evs) != 1 || !eventsEqual(evs[0], wantKey(KeyEscape, 0, 0, "\x1b")) {
		t.Fatalf("flush: got %v", evs)
	}
	if d.Pending() {
		t.Fatal("pending after flush")
	}

	// unfinished introducers resolve to the alt+key that produces them
	cases := []struct {
		in   string
		want []Event
	}{
		{"\x1b[", []Event{wantKey(KeyRune, '[', Alt, "\x1b[")}},
		{"\x1bO", []Event{wantKey(KeyRune, 'O', Alt, "\x1bO")}},
		{"\x1b]", []Event{wantKey(KeyRune, ']', Alt, "\x1b]")}},
		{"\x1b[1;5", []Event{
			wantKey(KeyRune, '[', Alt, "\x1b["),
			wantKey(KeyRune, '1', 0, "1"),
			wantKey(KeyRune, ';', 0, ";"),
			wantKey(KeyRune, '5', 0, "5"),
		}},
	}
	for _, c := range cases {
		d := NewDecoder()
		if evs := d.Feed([]byte(c.in)); len(evs) != 0 {
			t.Fatalf("%q: Feed produced %v", c.in, evs)
		}
		got := d.Flush()
		if len(got) != len(c.want) {
			t.Fatalf("%q: flush got %v, want %v", c.in, got, c.want)
		}
		for i := range c.want {
			if !eventsEqual(got[i], c.want[i]) {
				t.Errorf("%q event %d: got %s, want %s", c.in, i, fmtEvent(got[i]), fmtEvent(c.want[i]))
			}
		}
		if d.Pending() {
			t.Errorf("%q: pending after flush", c.in)
		}
	}

	// flush on an empty decoder is a no-op
	d = NewDecoder()
	if evs := d.Flush(); evs != nil {
		t.Fatalf("empty flush: got %v", evs)
	}

	// flush never forces an open paste; only the terminator ends it
	d = NewDecoder()
	d.Feed([]byte("\x1b[200~partial"))
	if evs := d.Flush(); len(evs) != 0 {
		t.Fatalf("flush during paste: got %v", evs)
	}
	if !d.Pending() {
		t.Fatal("paste no longer pending after flush")
	}
	evs = d.Feed([]byte("\x1b[201~"))
	if len(evs) != 1 || string(evs[0].Paste) != "partial" {
		t.Fatalf("paste after flush: got %v", evs)
	}
}

func TestEscapeThenSequenceAfterFlush(t *testing.T) {
	// the idle timer separates a real escape keypress from a following
	// arrow; this is exactly the router's flush discipline
	d := NewDecoder()
	if evs := d.Feed([]byte{0x1b}); len(evs) != 0 {
		t.Fatal("expected no events")
	}
	evs := d.Flush()
	evs = append(evs, d.Feed([]byte("\x1b[A"))...)
	if len(evs) != 2 || evs[0].Key != KeyEscape || evs[1].Key != KeyUp {
		t.Fatalf("got %v", evs)
	}
}
