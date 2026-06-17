package tui

import (
	"strings"
	"testing"
)

// newTestScreen builds a Screen with buffers but no terminal, so composeFrame
// (which never touches the writer) can be exercised directly.
func newTestScreen(cols, rows int) *Screen {
	return &Screen{
		cols: cols, rows: rows,
		front: NewBuffer(cols, rows), back: NewBuffer(cols, rows),
		curVis: true, lastVis: true,
	}
}

func TestComposeOnlyPaintsChangedRows(t *testing.T) {
	s := newTestScreen(4, 2)
	s.back.DrawText(0, 0, DefaultStyle, "hi") // change row 0 only

	out := string(s.composeFrame())
	if !strings.Contains(out, "\x1b[1;1H") {
		t.Errorf("expected a move to row 0; frame = %q", out)
	}
	if !strings.Contains(out, "hi") {
		t.Errorf("expected the new text; frame = %q", out)
	}
	if strings.Contains(out, "\x1b[2;1H") {
		t.Errorf("row 1 was unchanged but got repainted; frame = %q", out)
	}
	if !strings.Contains(out, "\x1b[?2026h") || !strings.Contains(out, "\x1b[?2026l") {
		t.Errorf("frame not wrapped in a synchronized update; frame = %q", out)
	}

	// Front now mirrors back, so a second compose with no changes is a no-op.
	if f := s.composeFrame(); f != nil {
		t.Errorf("idempotent compose returned %q, want nil", string(f))
	}
}

func TestComposeCursorOnlyMove(t *testing.T) {
	s := newTestScreen(4, 2)
	s.composeFrame() // settle initial state
	s.SetCursor(2, 1)
	out := string(s.composeFrame())
	if out == "" {
		t.Fatal("cursor move produced no frame")
	}
	if !strings.Contains(out, "\x1b[2;3H") {
		t.Errorf("expected cursor at row 1 col 2; frame = %q", out)
	}
}

func TestComposeDirtyAllClears(t *testing.T) {
	s := newTestScreen(4, 2)
	s.composeFrame()
	s.dirtyAll = true
	out := string(s.composeFrame())
	if !strings.Contains(out, "\x1b[2J") {
		t.Errorf("dirtyAll should clear the screen; frame = %q", out)
	}
	if !strings.Contains(out, "\x1b[1;1H") || !strings.Contains(out, "\x1b[2;1H") {
		t.Errorf("dirtyAll should repaint every row; frame = %q", out)
	}
}

func TestRenderRowNoNulFromWideRune(t *testing.T) {
	s := newTestScreen(4, 1)
	s.back.DrawText(0, 0, DefaultStyle, "你")
	out := string(s.composeFrame())
	if strings.ContainsRune(out, 0) {
		t.Errorf("frame leaked a NUL continuation byte; frame = %q", out)
	}
	if !strings.ContainsRune(out, '你') {
		t.Errorf("frame missing the wide rune; frame = %q", out)
	}
}
