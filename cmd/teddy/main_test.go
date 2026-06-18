package main

import (
	"os"
	"path/filepath"
	"testing"
)

// A directory argument inside a git repo must root at that directory, NOT
// walk up to the repo: search/browse stay scoped to the opened folder.
func TestResolveTargetDirNoGitWalk(t *testing.T) {
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(repo, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	root, openPath, err := resolveTarget([]string{sub})
	if err != nil {
		t.Fatal(err)
	}
	if openPath != "" {
		t.Errorf("openPath = %q, want empty for a directory arg", openPath)
	}
	want, _ := filepath.EvalSymlinks(sub)
	if root != want {
		t.Errorf("root = %q, want %q (the opened folder, not the repo root)", root, want)
	}
}

// A file argument roots at the file's parent directory and opens the file.
func TestResolveTargetFile(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	root, openPath, err := resolveTarget([]string{f})
	if err != nil {
		t.Fatal(err)
	}
	if want, _ := filepath.EvalSymlinks(dir); root != want {
		t.Errorf("root = %q, want %q", root, want)
	}
	if want, _ := filepath.Abs(f); openPath != want {
		t.Errorf("openPath = %q, want %q", openPath, want)
	}
}
