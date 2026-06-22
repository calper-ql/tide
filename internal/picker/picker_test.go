package picker

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/calper-ql/tide/internal/input"
)

// click synthesizes a left-button press at a screen cell.
func click(x, y int) input.Event {
	return input.Event{Type: input.EvMouse, Mouse: input.MousePress, Button: 1, X: x, Y: y}
}

func mkTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, d := range []string{"alpha", "beta", "zeta"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "readme.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "beta", "inner"), 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

// rowOf returns the screen y for entry index i at the current scroll offset 0.
func rowY(i int) int { return listTop + i }

func TestPickerListsDirsFirstWithUpRow(t *testing.T) {
	root := mkTree(t)
	m := New(root, 80, 30)
	// Expect: "..", alpha/, beta/, zeta/, readme.txt
	want := []string{"..", "alpha", "beta", "zeta", "readme.txt"}
	if len(m.entries) != len(want) {
		t.Fatalf("entries = %d %+v, want %d", len(m.entries), m.entries, len(want))
	}
	for i, w := range want {
		if m.entries[i].name != w {
			t.Errorf("entry[%d] = %q, want %q", i, m.entries[i].name, w)
		}
	}
	if !m.entries[0].up {
		t.Error("first row must be the .. up-entry")
	}
	if m.entries[4].isDir {
		t.Error("readme.txt must sort after dirs and not be a dir")
	}
}

func TestPickerDescendThenOpenChoosesSubdir(t *testing.T) {
	root := mkTree(t)
	m := New(root, 80, 30)

	// Click "beta/" (index 2 → "..", alpha, beta) to descend.
	m.Handle(click(3, rowY(2)))
	if m.dir != filepath.Join(root, "beta") {
		t.Fatalf("after descend, dir = %q, want %q", m.dir, filepath.Join(root, "beta"))
	}
	// Inside beta: "..", inner/. Click the Open button (last row) → choose beta.
	m.Handle(click(5, m.rows-1))
	got, ok := m.Chosen()
	if !ok || got != filepath.Join(root, "beta") {
		t.Fatalf("chosen = %q, %v; want %q, true", got, ok, filepath.Join(root, "beta"))
	}
}

func TestPickerAscendViaUpRow(t *testing.T) {
	root := mkTree(t)
	sub := filepath.Join(root, "beta")
	m := New(sub, 80, 30)
	// Row 0 of the list is ".." here.
	m.Handle(click(2, rowY(0)))
	if m.dir != root {
		t.Fatalf("after .. click, dir = %q, want %q", m.dir, root)
	}
}

func TestPickerCancelButton(t *testing.T) {
	m := New(mkTree(t), 80, 30)
	// Click the bar's cancel button (top row, far right).
	m.Handle(click(m.cols-2, 0))
	if !m.Cancelled() {
		t.Fatal("clicking cancel must set Cancelled")
	}
	if _, ok := m.Chosen(); ok {
		t.Fatal("cancel must not choose a folder")
	}
}

func TestPickerScrollClampsAndStaysConsistent(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 100; i++ {
		if err := os.MkdirAll(filepath.Join(root, string(rune('a'+i%26))+string(rune('0'+i/26))), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	m := New(root, 80, 10) // small viewport forces scrolling
	// Wheel up at the top is a no-op (offset already 0).
	if m.Handle(input.Event{Type: input.EvMouse, Mouse: input.MouseWheelUp}) {
		t.Error("wheel up at top should not change offset")
	}
	// Scroll down past the end clamps to maxOffset.
	for i := 0; i < 100; i++ {
		m.Handle(input.Event{Type: input.EvMouse, Mouse: input.MouseWheelDown})
	}
	if m.offset != m.maxOffset() {
		t.Fatalf("offset = %d, want clamped to maxOffset %d", m.offset, m.maxOffset())
	}
	// A click after scrolling lands on the entry the same arithmetic renders.
	idx := m.entryAt(3, listTop)
	if idx != m.offset {
		t.Fatalf("entryAt(top row) = %d, want offset %d (render/hit must agree)", idx, m.offset)
	}
}

func TestPickerClickingFileDoesNothing(t *testing.T) {
	root := mkTree(t)
	m := New(root, 80, 30)
	// readme.txt is index 4.
	before := m.dir
	dirty := m.Handle(click(3, rowY(4)))
	if dirty {
		t.Error("clicking a file should not repaint/navigate")
	}
	if m.dir != before {
		t.Errorf("clicking a file changed dir to %q", m.dir)
	}
	if _, ok := m.Chosen(); ok {
		t.Error("clicking a file must not choose it")
	}
}

func TestPickerRenderFitsAndMarksDirs(t *testing.T) {
	m := New(mkTree(t), 80, 30)
	frame := string(m.Render())
	// A full repaint and the Open button must be present.
	if !contains(frame, "\x1b[2J") {
		t.Error("render should clear the screen for a clean full repaint")
	}
	if !contains(frame, "Open this folder") {
		t.Error("render should show the Open button")
	}
	if !contains(frame, "alpha/") {
		t.Error("directories should render with a trailing slash")
	}
}

func key(k input.Key) input.Event { return input.Event{Type: input.EvKey, Key: k} }

func TestPickerArrowKeyNavigation(t *testing.T) {
	root := mkTree(t) // "..", alpha/, beta/, zeta/, readme.txt
	m := New(root, 80, 30)

	// Down selects the first row, Down again moves to "alpha".
	m.Handle(key(input.KeyDown)) // -> idx 0 (..)
	m.Handle(key(input.KeyDown)) // -> idx 1 (alpha)
	if m.entries[m.hover].name != "alpha" {
		t.Fatalf("after two Downs, selected %q, want alpha", m.entries[m.hover].name)
	}
	// Move to "beta" and Right to descend into it.
	m.Handle(key(input.KeyDown)) // -> beta
	if m.entries[m.hover].name != "beta" {
		t.Fatalf("selected %q, want beta", m.entries[m.hover].name)
	}
	m.Handle(key(input.KeyRight))
	if m.dir != filepath.Join(root, "beta") {
		t.Fatalf("Right did not descend: dir=%q", m.dir)
	}
	// Left goes back up AND highlights the folder we came from ("beta").
	m.Handle(key(input.KeyLeft))
	if m.dir != root {
		t.Fatalf("Left did not ascend: dir=%q", m.dir)
	}
	if m.hover < 0 || m.entries[m.hover].name != "beta" {
		t.Fatalf("after Left, selection = %v, want came-from beta", m.hover)
	}
}

func TestPickerRightOnFileOrLeftAtRootIsNoop(t *testing.T) {
	root := mkTree(t)
	m := New(root, 80, 30)
	// Select readme.txt (idx 4) and press Right — files aren't enterable.
	m.selectIndex(4)
	if m.Handle(key(input.KeyRight)) {
		t.Error("Right on a file should do nothing")
	}
	if _, ok := m.Chosen(); ok || m.dir != root {
		t.Error("Right on a file must not navigate or choose")
	}
	// Left at the filesystem root is a no-op.
	r := New("/", 80, 30)
	if r.Handle(key(input.KeyLeft)) {
		t.Error("Left at / should do nothing")
	}
}

func TestPickerArrowDownScrollsSelectionIntoView(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 50; i++ {
		if err := os.MkdirAll(filepath.Join(root, "d"+string(rune('A'+i%26))+string(rune('0'+i/26))), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	m := New(root, 80, 10) // small viewport
	for i := 0; i < 40; i++ {
		m.Handle(key(input.KeyDown))
	}
	// The selected row must be within the rendered window.
	if m.hover < m.offset || m.hover >= m.offset+m.visibleRows() {
		t.Fatalf("selection %d not in view [%d,%d)", m.hover, m.offset, m.offset+m.visibleRows())
	}
}

func TestPickerInertWhenTooSmall(t *testing.T) {
	m := New(mkTree(t), 80, 4) // rows < 6: chrome is not drawn
	// A click where the Open button would be must NOT confirm a folder.
	m.Handle(click(5, m.rows-1))
	if _, ok := m.Chosen(); ok {
		t.Fatal("a too-small picker must not confirm a folder from an undrawn row")
	}
	// A top-row click must NOT cancel (cancelStart would be negative).
	m.Handle(click(0, 0))
	if m.Cancelled() {
		t.Fatal("a too-small picker must not cancel from an undrawn bar")
	}
	if !contains(string(m.Render()), "too small") {
		t.Error("a too-small viewport should render the notice, not the chrome")
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
