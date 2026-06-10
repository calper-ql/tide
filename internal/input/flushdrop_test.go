package input

import "testing"

// A forced flush of a partial sequence with body bytes must drop it whole:
// literalizing "\x1b[<0;3" would type mouse-report fragments into a pane.
func TestFlushDropsPartialSequenceBody(t *testing.T) {
	d := NewDecoder()
	if evs := d.Feed([]byte("\x1b[<0;3")); len(evs) != 0 {
		t.Fatalf("partial sequence decoded eagerly: %+v", evs)
	}
	evs := d.Flush()
	if len(evs) != 1 || evs[0].Type != EvUnknown {
		t.Fatalf("flush = %+v, want one EvUnknown", evs)
	}
	if string(evs[0].Raw) != "\x1b[<0;3" {
		t.Fatalf("Raw = %q", evs[0].Raw)
	}

	// A bare introducer still resolves to Alt+rune: that is the only
	// keyboard input that produces exactly those bytes.
	d2 := NewDecoder()
	d2.Feed([]byte{0x1b, '['})
	evs = d2.Flush()
	if len(evs) != 1 || evs[0].Type != EvKey || evs[0].Rune != '[' || evs[0].Mods&Alt == 0 {
		t.Fatalf("bare ESC [ flush = %+v, want Alt+'['", evs)
	}
}
