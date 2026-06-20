package vt

// The VT is where OSC 52 written by a program running INSIDE a pane is
// intercepted (bubbletea's tea.SetClipboard, vim's clipboard=unnamed via a
// supporting plugin, etc.). It is parsed and queued as a ClipEvent rather
// than passed through, so the daemon can re-emit it to every client. These
// tests pin that parse and its boundaries.

import (
	"encoding/base64"
	"strings"
	"testing"
)

// osc52In builds the inner-program OSC 52 sequence (BEL-terminated).
func osc52In(target, text string) []byte {
	return []byte("\x1b]52;" + target + ";" + base64.StdEncoding.EncodeToString([]byte(text)) + "\a")
}

func TestInnerOSC52QueuesClipEvent(t *testing.T) {
	st := base64.StdEncoding.EncodeToString([]byte("st-terminated"))
	cases := []struct {
		name string
		seq  []byte
		want []ClipEvent
	}{
		{"clipboard, BEL", osc52In("c", "hello"), []ClipEvent{{"c", "hello"}}},
		{"primary, BEL", osc52In("p", "selection"), []ClipEvent{{"p", "selection"}}},
		{"ST terminated", []byte("\x1b]52;c;" + st + "\x1b\\"), []ClipEvent{{"c", "st-terminated"}}},
		{"unicode payload", osc52In("c", "café 世界 🚀 ✓"), []ClipEvent{{"c", "café 世界 🚀 ✓"}}},
		{"multiline payload", osc52In("c", "a\nb\nc"), []ClipEvent{{"c", "a\nb\nc"}}},
		{"invalid base64 dropped", []byte("\x1b]52;c;@@@not-base64@@@\a"), nil},
		{"empty payload dropped", []byte("\x1b]52;c;\a"), nil},
		{"unsupported target dropped", osc52In("q", "x"), nil},
		{"cut-buffer target dropped", osc52In("s0", "x"), nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			term := New(80, 24, 0, nil)
			term.Write(tc.seq)
			got := term.DrainClips()
			if len(got) != len(tc.want) {
				t.Fatalf("clips = %+v, want %+v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("clip[%d] = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestInnerOSC52DrainIsOneShot: DrainClips clears the queue, so a clip is
// delivered exactly once (the pane drains after every Write).
func TestInnerOSC52DrainIsOneShot(t *testing.T) {
	term := New(80, 24, 0, nil)
	term.Write(osc52In("c", "once"))
	if got := term.DrainClips(); len(got) != 1 || got[0].Text != "once" {
		t.Fatalf("first drain = %+v, want one 'once'", got)
	}
	if got := term.DrainClips(); len(got) != 0 {
		t.Fatalf("second drain = %+v, want empty", got)
	}
}

// TestInnerOSC52LargeButBoundedRoundTrips: a big-but-bounded clipboard write
// (base64 under the VT's in-flight cap) must round-trip exactly.
func TestInnerOSC52LargeButBoundedRoundTrips(t *testing.T) {
	text := strings.Repeat("Y", 40*1024) // base64 ≈ 54KB, under maxInflight (64KB)
	term := New(80, 24, 0, nil)
	term.Write(osc52In("c", text))
	got := term.DrainClips()
	if len(got) != 1 || got[0].Text != text {
		t.Fatalf("bounded large payload did not round trip (got %d clips, len %d)", len(got), clipLen(got))
	}
}

// TestInnerOSC52OversizePayloadDoesNotCorrupt documents the VT's in-flight
// cap (maxInflight): an inner OSC 52 larger than the cap is truncated at the
// parser. The contract under test is that this never surfaces a SILENTLY
// truncated clipboard — the malformed (cut) base64 fails to decode and the
// clip is dropped rather than delivered as a partial payload.
func TestInnerOSC52OversizePayloadDoesNotCorrupt(t *testing.T) {
	big := strings.Repeat("X", 80*1024) // base64 ≈ 108KB, over maxInflight
	term := New(80, 24, 0, nil)
	term.Write(osc52In("c", big))
	for _, c := range term.DrainClips() {
		if c.Text == big {
			t.Fatal("oversize payload unexpectedly round-tripped intact")
		}
		if c.Text != "" {
			t.Errorf("oversize OSC 52 surfaced a %d-byte partial clipboard; want it dropped", len(c.Text))
		}
	}
}

func clipLen(c []ClipEvent) int {
	if len(c) == 0 {
		return 0
	}
	return len(c[0].Text)
}
