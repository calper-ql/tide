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
