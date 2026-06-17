package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInsertAndSerialize(t *testing.T) {
	d := newDoc("", nil)
	d.insertString("ab")
	d.insertNewline()
	d.insertString("cd")
	if got := string(d.bytes()); got != "ab\ncd" {
		t.Errorf("bytes = %q, want \"ab\\ncd\"", got)
	}
	if d.cy != 1 || d.cx != 2 {
		t.Errorf("cursor = (%d,%d), want (1,2)", d.cy, d.cx)
	}
}

func TestBackspaceJoinsLines(t *testing.T) {
	d := newDoc("", []byte("ab\ncd"))
	d.cy, d.cx = 1, 0
	d.backspace()
	if got := string(d.bytes()); got != "abcd" {
		t.Errorf("bytes = %q, want \"abcd\"", got)
	}
	if d.cy != 0 || d.cx != 2 {
		t.Errorf("cursor = (%d,%d), want (0,2)", d.cy, d.cx)
	}
}

func TestSaveRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f.txt")
	d := newDoc(p, []byte("x\ny\n")) // trailing newline -> 3 lines
	d.insertString("Z")              // at (0,0)
	if err := d.save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != "Zx\ny\n" {
		t.Errorf("on disk = %q, want \"Zx\\ny\\n\"", string(got))
	}
	if d.dirty {
		t.Error("doc still dirty after save")
	}
}

func TestUndoRedoCoalescesTyping(t *testing.T) {
	d := newDoc("", []byte("abc"))
	d.cx = 3
	d.insertString("XY") // one coalesced typing group
	if got := string(d.bytes()); got != "abcXY" {
		t.Fatalf("after typing = %q", got)
	}
	d.Undo()
	if got := string(d.bytes()); got != "abc" {
		t.Errorf("after undo = %q, want \"abc\"", got)
	}
	d.Redo()
	if got := string(d.bytes()); got != "abcXY" {
		t.Errorf("after redo = %q, want \"abcXY\"", got)
	}
}

func TestDisplayColAndInverse(t *testing.T) {
	line := []rune("a\tb") // 'a', tab (to col 4), 'b'
	if got := displayCol(line, 2); got != 4 {
		t.Errorf("displayCol after tab = %d, want 4", got)
	}
	if got := displayCol(line, 3); got != 5 {
		t.Errorf("displayCol after 'b' = %d, want 5", got)
	}
	if got := colFromDisplay(line, 4); got != 2 {
		t.Errorf("colFromDisplay(4) = %d, want 2 (the 'b')", got)
	}
}

func TestExpandLine(t *testing.T) {
	if cells := expandLine([]rune("你")); len(cells) != 2 || cells[0] != '你' || cells[1] != 0 {
		t.Errorf("wide rune expansion = %v, want ['你', 0]", cells)
	}
	if cells := expandLine([]rune("\t")); len(cells) != tabWidth {
		t.Errorf("tab expansion length = %d, want %d", len(cells), tabWidth)
	}
	if cells := expandLine([]rune{0x01}); len(cells) != 1 || cells[0] != '·' {
		t.Errorf("control-char expansion = %v, want ['·']", cells)
	}
}
