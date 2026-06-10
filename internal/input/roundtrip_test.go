package input

import (
	"fmt"
	"testing"
)

var allModCombos = []Mod{
	0, Shift, Alt, Shift | Alt, Ctrl, Shift | Ctrl, Alt | Ctrl, Shift | Alt | Ctrl,
}

// keyEquiv compares a decoded key event against the event that was
// encoded, allowing the one documented aliasing: bare (or alt-only) space
// decodes as KeyRune ' '.
func keyEquiv(got Event, k Key, r rune, m Mod) bool {
	if got.Type != EvKey {
		return false
	}
	if k == KeySpace && m&Ctrl == 0 {
		k, r = KeyRune, ' '
	}
	return got.Key == k && got.Rune == r && got.Mods == m
}

func roundTrip(t *testing.T, k Key, r rune, m Mod, o EncodeOpts) {
	t.Helper()
	b := EncodeKey(Event{Type: EvKey, Key: k, Rune: r, Mods: m}, o)
	if b == nil {
		t.Errorf("key %d rune %q mods %d opts %+v: not encodable", k, r, m, o)
		return
	}
	got := decodeOne(t, b)
	if !keyEquiv(got, k, r, m) {
		t.Errorf("key %d rune %q mods %d opts %+v: encoded %q, decoded %s",
			k, r, m, o, b, fmtEvent(got))
	}
}

// the named keys whose legacy encodings carry the full modifier bitfield;
// these round-trip across the entire mod matrix.
var fullMatrixKeys = []Key{
	KeyUp, KeyDown, KeyRight, KeyLeft,
	KeyHome, KeyEnd, KeyPageUp, KeyPageDown,
	KeyInsert, KeyDelete,
	KeyF1, KeyF2, KeyF3, KeyF4, KeyF5, KeyF6,
	KeyF7, KeyF8, KeyF9, KeyF10, KeyF11, KeyF12,
}

func TestRoundTripNamedKeys(t *testing.T) {
	for _, app := range []bool{false, true} {
		o := EncodeOpts{AppCursor: app}
		for _, k := range fullMatrixKeys {
			for _, m := range allModCombos {
				roundTrip(t, k, 0, m, o)
			}
		}
	}
}

// the C0-encoded keys round-trip over the modifier sets their legacy
// encodings can carry. Combinations outside these sets are lossy by
// construction (a terminal sends \r for ctrl+enter) and are pinned in
// TestEncodeLossyLegacy instead.
func TestRoundTripC0Keys(t *testing.T) {
	for _, app := range []bool{false, true} {
		o := EncodeOpts{AppCursor: app}
		for _, m := range []Mod{0, Alt} {
			roundTrip(t, KeyEnter, 0, m, o)
			roundTrip(t, KeyTab, 0, m, o)
			roundTrip(t, KeyBackspace, 0, m, o)
			roundTrip(t, KeyEscape, 0, m, o)
			roundTrip(t, KeySpace, 0, m, o) // decodes as KeyRune ' ', per keyEquiv
		}
		for _, m := range []Mod{Shift, Shift | Alt, Shift | Ctrl, Shift | Alt | Ctrl} {
			roundTrip(t, KeyTab, 0, m, o) // backtab carries the full bitfield
		}
		for _, m := range []Mod{Ctrl, Ctrl | Alt} {
			roundTrip(t, KeySpace, 0, m, o)
		}
	}
}

func TestRoundTripRunes(t *testing.T) {
	o := EncodeOpts{}
	for _, r := range []rune{'a', 'z', 'A', 'Z', '0', '9', '/', '-', '[', 'é', '日'} {
		for _, m := range []Mod{0, Alt} {
			roundTrip(t, KeyRune, r, m, o)
		}
	}
	// ctrl round-trips for the runes with a faithful C0 byte. excluded by
	// construction: i (tab), m (enter), '[' (escape), '@' (NUL decodes as
	// ctrl+space), '?' (DEL decodes as backspace)
	ctrlRunes := []rune{
		'a', 'b', 'c', 'd', 'e', 'f', 'g', 'h', 'j', 'k', 'l', 'n', 'o',
		'p', 'q', 'r', 's', 't', 'u', 'v', 'w', 'x', 'y', 'z',
		'\\', ']', '^', '_',
	}
	for _, r := range ctrlRunes {
		for _, m := range []Mod{Ctrl, Ctrl | Alt} {
			roundTrip(t, KeyRune, r, m, o)
		}
	}
}

func TestRoundTripAltIntroducers(t *testing.T) {
	// alt+'[' and friends encode as ESC+char, which is also a sequence
	// introducer; the decoder holds it and the router's idle flush — here
	// decodeOne's trailing Flush — resolves it back to the alt+key
	for _, r := range []rune{'[', 'O', ']', 'P', 'X', '^', '_'} {
		roundTrip(t, KeyRune, r, Alt, EncodeOpts{})
	}
}

func TestRoundTripMouseSGR(t *testing.T) {
	x, y := 7, 11
	var cases []Event
	for _, m := range allModCombos {
		for btn := 1; btn <= 3; btn++ {
			cases = append(cases,
				Event{Type: EvMouse, Mouse: MousePress, Button: btn, Mods: m},
				Event{Type: EvMouse, Mouse: MouseRelease, Button: btn, Mods: m},
				Event{Type: EvMouse, Mouse: MouseMotion, Button: btn, Mods: m},
			)
		}
		cases = append(cases,
			Event{Type: EvMouse, Mouse: MouseMotion, Button: 0, Mods: m},
			Event{Type: EvMouse, Mouse: MouseWheelUp, Button: 0, Mods: m},
			Event{Type: EvMouse, Mouse: MouseWheelDown, Button: 0, Mods: m},
		)
	}
	for _, ev := range cases {
		b := EncodeMouse(ev, MouseAnyMotion, true, x, y)
		if b == nil {
			t.Errorf("%s: not encodable", fmtEvent(ev))
			continue
		}
		got := decodeOne(t, b)
		if got.Type != EvMouse || got.Mouse != ev.Mouse || got.Button != ev.Button ||
			got.Mods != ev.Mods || got.X != x || got.Y != y {
			t.Errorf("%s: encoded %q, decoded %s", fmtEvent(ev), b, fmtEvent(got))
		}
	}
}

func TestRoundTripPaste(t *testing.T) {
	data := []byte("multi\nline\x1bwith escapes\x1b[31m")
	b := EncodePaste(data, EncodeOpts{BracketedPaste: true})
	got := decodeOne(t, b)
	if got.Type != EvPaste || string(got.Paste) != string(data) {
		t.Fatalf("paste round trip: %s", fmtEvent(got))
	}
}

// every encodable key event must decode from a single Feed without
// leftovers, whatever the pane mode flags — a sanity sweep that complements
// the exact matrices above.
func TestEncodeAlwaysDecodes(t *testing.T) {
	opts := []EncodeOpts{
		{}, {AppCursor: true}, {CRLF: true}, {AppCursor: true, AppKeypad: true},
	}
	for _, o := range opts {
		for k := KeyEnter; k <= KeySpace; k++ {
			for _, m := range allModCombos {
				b := EncodeKey(Event{Type: EvKey, Key: k, Mods: m}, o)
				if b == nil {
					t.Errorf("key %d mods %d opts %+v: not encodable", k, m, o)
					continue
				}
				d := NewDecoder()
				evs := d.Feed(b)
				evs = append(evs, d.Flush()...)
				if len(evs) == 0 {
					t.Errorf("key %d mods %d opts %+v: encoded %q decoded to nothing", k, m, o, b)
				}
				if d.Pending() {
					t.Errorf("key %d mods %d opts %+v: %q left the decoder pending", k, m, o, b)
				}
				var raw []byte
				for _, e := range evs {
					raw = append(raw, e.Raw...)
				}
				if string(raw) != string(b) {
					t.Errorf("key %d mods %d: raw %q does not reproduce %q", k, m, raw, b)
				}
			}
		}
	}
}

func ExampleDecoder() {
	d := NewDecoder()
	for _, ev := range d.Feed([]byte("a\x1b[1;5A")) {
		fmt.Printf("key=%d rune=%q mods=%d\n", ev.Key, ev.Rune, ev.Mods)
	}
	// Output:
	// key=0 rune='a' mods=0
	// key=5 rune='\x00' mods=4
}
