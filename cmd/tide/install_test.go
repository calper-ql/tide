package main

import (
	"os"
	"path/filepath"
	"testing"
)

func fakeBin(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestInstallLinksTideAndSiblingTeddy(t *testing.T) {
	src := t.TempDir()
	exe := filepath.Join(src, "tide")
	fakeBin(t, exe)
	fakeBin(t, filepath.Join(src, "teddy"))
	dir := t.TempDir()

	linked, err := installLinks(exe, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(linked) != 2 {
		t.Fatalf("linked %v, want tide + teddy", linked)
	}
	if got, _ := os.Readlink(filepath.Join(dir, "tide")); got != exe {
		t.Fatalf("tide link -> %q, want %q", got, exe)
	}
	if got, _ := os.Readlink(filepath.Join(dir, "teddy")); got != filepath.Join(src, "teddy") {
		t.Fatalf("teddy link -> %q, want sibling", got)
	}
}

func TestInstallLinksIdempotent(t *testing.T) {
	src := t.TempDir()
	exe := filepath.Join(src, "tide")
	fakeBin(t, exe)
	dir := t.TempDir()

	if _, err := installLinks(exe, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := installLinks(exe, dir); err != nil {
		t.Fatalf("second install should be a no-op, got %v", err)
	}
	if got, _ := os.Readlink(filepath.Join(dir, "tide")); got != exe {
		t.Fatalf("tide link -> %q after re-install, want %q", got, exe)
	}
}

func TestInstallLinksRepointsStaleSymlink(t *testing.T) {
	src := t.TempDir()
	exe := filepath.Join(src, "tide")
	fakeBin(t, exe)
	dir := t.TempDir()
	// A stale symlink to an old location must be repointed, not errored on.
	if err := os.Symlink("/old/tide", filepath.Join(dir, "tide")); err != nil {
		t.Fatal(err)
	}
	if _, err := installLinks(exe, dir); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.Readlink(filepath.Join(dir, "tide")); got != exe {
		t.Fatalf("stale link -> %q, want repointed to %q", got, exe)
	}
}

func TestInstallLinksRefusesToClobberRealFile(t *testing.T) {
	src := t.TempDir()
	exe := filepath.Join(src, "tide")
	fakeBin(t, exe)
	dir := t.TempDir()
	// A real (non-symlink) file the user put there must be preserved.
	real := filepath.Join(dir, "tide")
	if err := os.WriteFile(real, []byte("not ours"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := installLinks(exe, dir); err == nil {
		t.Fatal("installLinks must refuse to clobber a real file")
	}
	if b, _ := os.ReadFile(real); string(b) != "not ours" {
		t.Fatal("the pre-existing file must be left untouched")
	}
}
