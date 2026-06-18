package vt

import (
	"bytes"
	"testing"
)

// The Kitty keyboard protocol set/query/disable round-trip: an app turns on
// disambiguation (CSI = 1 ; 1 u, as bubbletea/ultraviolet emit), the VT
// records it and answers a query (CSI ? u) with the active flags, and a
// disable (CSI = 0 ; 1 u) clears it again.
func TestKittyKeyboardSetQueryDisable(t *testing.T) {
	var ans bytes.Buffer
	term := New(20, 4, 0, &ans)

	term.Write([]byte("\x1b[=1;1u"))
	if kf, _ := term.KeyboardProtoSnapshot(); kf != 1 {
		t.Fatalf("after set: kittyFlags = %d, want 1", kf)
	}

	term.Write([]byte("\x1b[?u"))
	if got := ans.String(); got != "\x1b[?1u" {
		t.Fatalf("query response = %q, want %q", got, "\x1b[?1u")
	}

	term.Write([]byte("\x1b[=0;1u"))
	if kf, _ := term.KeyboardProtoSnapshot(); kf != 0 {
		t.Fatalf("after disable: kittyFlags = %d, want 0", kf)
	}
}

// CSI = flags ; mode u honours the mode: 2 adds bits, 3 removes them, 1
// replaces the set.
func TestKittyKeyboardSetModes(t *testing.T) {
	term := New(20, 4, 0, nil)
	term.Write([]byte("\x1b[=1;1u")) // set -> 1
	term.Write([]byte("\x1b[=4;2u")) // add -> 5
	if kf, _ := term.KeyboardProtoSnapshot(); kf != 5 {
		t.Fatalf("after add: kittyFlags = %d, want 5", kf)
	}
	term.Write([]byte("\x1b[=1;3u")) // remove bit 1 -> 4
	if kf, _ := term.KeyboardProtoSnapshot(); kf != 4 {
		t.Fatalf("after remove: kittyFlags = %d, want 4", kf)
	}
	term.Write([]byte("\x1b[=2;1u")) // replace -> 2
	if kf, _ := term.KeyboardProtoSnapshot(); kf != 2 {
		t.Fatalf("after replace: kittyFlags = %d, want 2", kf)
	}
}

// Push/pop nest, restoring the previous flag set on pop (CSI > flags u /
// CSI < n u).
func TestKittyKeyboardPushPop(t *testing.T) {
	term := New(20, 4, 0, nil)
	term.Write([]byte("\x1b[>1u")) // push 1
	term.Write([]byte("\x1b[>5u")) // push 5
	if kf, _ := term.KeyboardProtoSnapshot(); kf != 5 {
		t.Fatalf("after pushes: kittyFlags = %d, want 5", kf)
	}
	term.Write([]byte("\x1b[<u")) // pop 1 -> back to 1
	if kf, _ := term.KeyboardProtoSnapshot(); kf != 1 {
		t.Fatalf("after first pop: kittyFlags = %d, want 1", kf)
	}
	term.Write([]byte("\x1b[<u")) // pop 1 -> back to 0
	if kf, _ := term.KeyboardProtoSnapshot(); kf != 0 {
		t.Fatalf("after second pop: kittyFlags = %d, want 0", kf)
	}
}

// CSI > 4 ; Pv m is xterm modifyOtherKeys, not an SGR: it must set the level
// and leave the pane's text attributes untouched. (Before the fix it routed
// to setAttr, leaking underline+faint into every following cell.)
func TestModifyOtherKeysNotSGR(t *testing.T) {
	term := New(20, 4, 0, nil)
	term.Write([]byte("\x1b[>4;2m"))
	if _, mok := term.KeyboardProtoSnapshot(); mok != 2 {
		t.Fatalf("modifyOtherKeys = %d, want 2", mok)
	}
	var mode int16
	term.WithLock(func(s *State) { mode = s.cur.Attr.Mode })
	if mode != 0 {
		t.Fatalf("cursor attribute mode = %d, want 0 (SGR must not have run)", mode)
	}

	term.Write([]byte("\x1b[>4m")) // reset
	if _, mok := term.KeyboardProtoSnapshot(); mok != 0 {
		t.Fatalf("modifyOtherKeys after reset = %d, want 0", mok)
	}
}

// A bare CSI u is still DECRC (restore cursor) — the marker check must not
// swallow it as a keyboard request.
func TestBareCSIuStillDECRC(t *testing.T) {
	term := New(20, 10, 0, nil)
	term.Write([]byte("\x1b[5;3H")) // move to row 5, col 3 (1-based)
	term.Write([]byte("\x1b[s"))    // DECSC save
	term.Write([]byte("\x1b[1;1H")) // move away
	term.Write([]byte("\x1b[u"))    // DECRC restore
	var x, y int
	term.WithLock(func(s *State) { x, y = s.cur.X, s.cur.Y })
	if x != 2 || y != 4 {
		t.Fatalf("after DECRC: cursor = (%d,%d), want (2,4)", x, y)
	}
	if kf, _ := term.KeyboardProtoSnapshot(); kf != 0 {
		t.Fatalf("bare CSI u must not enable kitty: kittyFlags = %d", kf)
	}
}
