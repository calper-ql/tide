package tui

import "testing"

func TestDrawTextWidth(t *testing.T) {
	b := NewBuffer(6, 1)
	end := b.DrawText(0, 0, DefaultStyle, "a你b") // 你 is double-width
	if end != 4 {
		t.Errorf("end x = %d, want 4 (1 + 2 + 1)", end)
	}
	if got := b.Cell(0, 0).R; got != 'a' {
		t.Errorf("cell0 = %q, want 'a'", got)
	}
	if got := b.Cell(1, 0).R; got != '你' {
		t.Errorf("cell1 = %q, want '你'", got)
	}
	if got := b.Cell(2, 0).R; got != 0 {
		t.Errorf("cell2 = %q, want continuation (0)", got)
	}
	if got := b.Cell(3, 0).R; got != 'b' {
		t.Errorf("cell3 = %q, want 'b'", got)
	}
}

func TestWideRuneAtRightEdgeDegrades(t *testing.T) {
	b := NewBuffer(2, 1)
	if n := b.Set(1, 0, '你', DefaultStyle); n != 1 {
		t.Errorf("wide rune in last column consumed %d cols, want 1 (degraded to space)", n)
	}
	if got := b.Cell(1, 0).R; got != ' ' {
		t.Errorf("last cell = %q, want space", got)
	}
}

func TestDrawTextClips(t *testing.T) {
	b := NewBuffer(3, 1)
	end := b.DrawText(0, 0, DefaultStyle, "abcdef")
	if end != 3 {
		t.Errorf("end x = %d, want 3 (clipped to width)", end)
	}
	if got := b.Cell(2, 0).R; got != 'c' {
		t.Errorf("cell2 = %q, want 'c'", got)
	}
}

func TestFill(t *testing.T) {
	b := NewBuffer(4, 3)
	st := DefaultStyle.WithBG(Blue)
	b.Fill(Rect{X: 1, Y: 1, W: 2, H: 1}, '#', st)
	if c := b.Cell(1, 1); c.R != '#' || c.St != st {
		t.Errorf("filled cell = %+v, want '#' with blue bg", c)
	}
	if c := b.Cell(0, 0); c.R != ' ' {
		t.Errorf("unfilled cell = %q, want space", c.R)
	}
}
