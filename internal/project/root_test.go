package project

import (
	"os"
	"path/filepath"
	"testing"
)

// canon resolves symlinks so expectations match Resolve's canonical output
// (macOS /tmp is a symlink to /private/tmp).
func canon(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func mkdirAll(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
}

func writeFile(t *testing.T, p, content string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestResolveFindsGitDir(t *testing.T) {
	tmp := canon(t, t.TempDir())
	repo := filepath.Join(tmp, "repo")
	deep := filepath.Join(repo, "a", "b")
	mkdirAll(t, filepath.Join(repo, ".git"))
	mkdirAll(t, deep)

	root, found, err := Resolve(deep)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("expected repository to be found")
	}
	if root != repo {
		t.Fatalf("root = %q, want %q", root, repo)
	}
}

func TestResolveWorktreeGitFile(t *testing.T) {
	// Linked worktrees have a .git *file* pointing at the real gitdir.
	tmp := canon(t, t.TempDir())
	wt := filepath.Join(tmp, "wt")
	mkdirAll(t, filepath.Join(wt, "sub"))
	writeFile(t, filepath.Join(wt, ".git"), "gitdir: /elsewhere/.git/worktrees/wt\n")

	root, found, err := Resolve(filepath.Join(wt, "sub"))
	if err != nil {
		t.Fatal(err)
	}
	if !found || root != wt {
		t.Fatalf("root = %q found = %v, want %q true", root, found, wt)
	}
}

func TestResolveNearestGitWins(t *testing.T) {
	tmp := canon(t, t.TempDir())
	outer := filepath.Join(tmp, "outer")
	inner := filepath.Join(outer, "inner")
	deep := filepath.Join(inner, "x")
	mkdirAll(t, filepath.Join(outer, ".git"))
	mkdirAll(t, filepath.Join(inner, ".git"))
	mkdirAll(t, deep)

	root, found, err := Resolve(deep)
	if err != nil {
		t.Fatal(err)
	}
	if !found || root != inner {
		t.Fatalf("root = %q found = %v, want %q true", root, found, inner)
	}
}

func TestResolveNoRepositoryFallsBackToDir(t *testing.T) {
	tmp := canon(t, t.TempDir())
	d := filepath.Join(tmp, "plain")
	mkdirAll(t, d)

	root, found, err := Resolve(d)
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Skip("a .git exists above the temp dir on this machine; cannot test the no-repo path hermetically")
	}
	if root != d {
		t.Fatalf("root = %q, want %q", root, d)
	}
}

func TestResolveRejectsNonexistentAndFilePaths(t *testing.T) {
	tmp := canon(t, t.TempDir())

	if _, _, err := Resolve(filepath.Join(tmp, "no/such/dir")); err == nil {
		t.Fatal("a nonexistent path must not become a session identity")
	}

	f := filepath.Join(tmp, "file.txt")
	writeFile(t, f, "x")
	if _, _, err := Resolve(f); err == nil {
		t.Fatal("a file path must not become a session identity")
	}
}

func TestResolveThroughSymlink(t *testing.T) {
	tmp := canon(t, t.TempDir())
	repo := filepath.Join(tmp, "repo")
	mkdirAll(t, filepath.Join(repo, ".git"))
	link := filepath.Join(tmp, "link")
	if err := os.Symlink(repo, link); err != nil {
		t.Fatal(err)
	}

	root, found, err := Resolve(link)
	if err != nil {
		t.Fatal(err)
	}
	if !found || root != repo {
		t.Fatalf("root = %q found = %v, want %q true (symlinks must not split session identity)", root, found, repo)
	}
}
