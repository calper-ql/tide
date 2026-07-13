package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBrowserLoadsSortedHidesGit(t *testing.T) {
	dir := t.TempDir()
	mustMkdir(t, filepath.Join(dir, "zsub"))
	mustMkdir(t, filepath.Join(dir, ".git")) // must be hidden
	mustWrite(t, filepath.Join(dir, "afile.txt"))
	mustWrite(t, filepath.Join(dir, "mfile.go"))

	b := newBrowser(dir)
	if len(b.flat) != 3 {
		t.Fatalf("visible rows = %d, want 3 (.git hidden): %v", len(b.flat), names(b))
	}
	// Directories first, then files, each case-insensitively sorted.
	if !b.flat[0].node.isDir || b.flat[0].node.name != "zsub" {
		t.Errorf("row 0 = %+v, want zsub/ (dir first)", b.flat[0].node)
	}
	if b.flat[1].node.name != "afile.txt" || b.flat[2].node.name != "mfile.go" {
		t.Errorf("file order = %v, want [afile.txt mfile.go]", names(b))
	}
}

func TestBrowserExpandCollapse(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	mustMkdir(t, sub)
	mustWrite(t, filepath.Join(sub, "inner.txt"))

	b := newBrowser(dir)
	if len(b.flat) != 1 {
		t.Fatalf("before expand = %d rows, want 1", len(b.flat))
	}
	noop := func(string) error { return nil }
	b.activate(0, noop) // expand sub/
	if len(b.flat) != 2 || b.flat[1].depth != 1 || b.flat[1].node.name != "inner.txt" {
		t.Fatalf("after expand = %v, want sub/ + inner.txt(depth 1)", names(b))
	}
	b.activate(0, noop) // collapse
	if len(b.flat) != 1 {
		t.Errorf("after collapse = %d rows, want 1", len(b.flat))
	}
}

func TestBrowserReveal(t *testing.T) {
	dir := t.TempDir()
	deep := filepath.Join(dir, "a", "b")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(deep, "c.txt")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	b := newBrowser(dir)
	b.reveal(f)

	if b.revealedPath != f {
		t.Errorf("revealedPath = %q, want %q", b.revealedPath, f)
	}
	if b.sel < 0 || b.sel >= len(b.flat) {
		t.Fatalf("sel %d out of range (flat len %d)", b.sel, len(b.flat))
	}
	if got := b.flat[b.sel].node; got.name != "c.txt" || got.path != f {
		t.Errorf("selected = %q at %s, want c.txt at %s", got.name, got.path, f)
	}
	for _, want := range []string{"a", "b", "c.txt"} {
		found := false
		for _, n := range names(b) {
			if n == want {
				found = true
			}
		}
		if !found {
			t.Errorf("flat %v missing %q (ancestor not expanded to reveal)", names(b), want)
		}
	}
}

func TestBrowserOpensFile(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "f.txt")
	mustWrite(t, fp)
	b := newBrowser(dir)

	var opened string
	b.activate(0, func(p string) error { opened = p; return nil })
	if opened != fp {
		t.Errorf("opened %q, want %q", opened, fp)
	}
}

func TestClampedSideWidth(t *testing.T) {
	a := &App{sideWidth: 28}
	if w := a.clampedSideWidth(120); w != 28 {
		t.Errorf("in-range width = %d, want 28", w)
	}
	a.sideWidth = 2 // too narrow
	if w := a.clampedSideWidth(120); w != minSideWidth {
		t.Errorf("clamp-up width = %d, want %d", w, minSideWidth)
	}
	a.sideWidth = 1000 // too wide: must leave the editor room
	if w, want := a.clampedSideWidth(80), 80-activityW-minEditorWidth; w != want {
		t.Errorf("clamp-down width = %d, want %d", w, want)
	}
}

func names(b *browser) []string {
	out := make([]string, len(b.flat))
	for i, e := range b.flat {
		out[i] = e.node.name
	}
	return out
}

func mustMkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.Mkdir(p, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, p string) {
	t.Helper()
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestBrowserRefreshTracksDisk verifies the poll refresh picks up files created
// and deleted outside teddy, keeps expanded folders expanded, and holds the
// selection on the same file across the reconcile.
func TestBrowserRefreshTracksDisk(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	mustMkdir(t, sub)
	mustWrite(t, filepath.Join(sub, "inner.txt"))
	mustWrite(t, filepath.Join(dir, "b.txt"))

	b := newBrowser(dir)
	noop := func(string) error { return nil }
	// Expand sub/ and select its inner file.
	b.activate(0, noop) // sub/ is row 0 (dirs first)
	for i, e := range b.flat {
		if e.node.name == "inner.txt" {
			b.sel = i
		}
	}
	if len(b.flat) != 3 { // sub/, inner.txt, b.txt
		t.Fatalf("setup rows = %v, want 3", names(b))
	}

	// Create a new file at root and a new one inside the expanded sub/, and
	// delete b.txt — all outside teddy.
	mustWrite(t, filepath.Join(dir, "a.txt"))
	mustWrite(t, filepath.Join(sub, "inner2.txt"))
	if err := os.Remove(filepath.Join(dir, "b.txt")); err != nil {
		t.Fatal(err)
	}

	b.refresh()

	got := names(b)
	// sub/ stays expanded (still shows its children), new files appear, b.txt gone.
	want := map[string]bool{"sub": true, "inner.txt": true, "inner2.txt": true, "a.txt": true}
	for _, n := range got {
		if n == "b.txt" {
			t.Errorf("b.txt still present after deletion: %v", got)
		}
		delete(want, n)
	}
	if len(want) != 0 {
		t.Errorf("refresh missing entries %v; tree = %v", want, got)
	}
	// Selection stayed on inner.txt.
	if b.sel < 0 || b.sel >= len(b.flat) || b.flat[b.sel].node.name != "inner.txt" {
		t.Errorf("selection drifted off inner.txt: sel=%d tree=%v", b.sel, got)
	}
}
