// Package project resolves the project root that identifies a tide session.
package project

import (
	"fmt"
	"os"
	"path/filepath"
)

// Canonical returns dir as an absolute, symlink-resolved, cleaned path so
// that one project on disk maps to exactly one session identity. The
// directory must exist: silently passing through an unresolvable path would
// let the same project split into two session identities.
func Canonical(dir string) (string, error) {
	d, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(d)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory", d)
	}
	d, err = filepath.EvalSymlinks(d)
	if err != nil {
		return "", err
	}
	return filepath.Clean(d), nil
}

// Resolve walks up from dir to the nearest entry named .git — a directory,
// or a file as in linked worktrees — and returns that entry's parent as the
// project root. Nested repos: the nearest .git wins. If no repository is
// found, dir itself is the root and found is false (the UI states this in
// the status line).
func Resolve(dir string) (root string, found bool, err error) {
	d, err := Canonical(dir)
	if err != nil {
		return "", false, err
	}
	start := d
	for {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			return d, true, nil
		}
		parent := filepath.Dir(d)
		if parent == d {
			return start, false, nil
		}
		d = parent
	}
}
