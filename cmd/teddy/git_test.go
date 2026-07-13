package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// porcelain -z output: NUL-separated records, branch header first, rename
// entries followed by their source path.
func TestParseStatusGroupsAndBranch(t *testing.T) {
	out := "## main...origin/main [ahead 1, behind 2]\x00" +
		"M  staged.go\x00" + // staged modification
		" M dirty.go\x00" + // unstaged modification
		"MM both.go\x00" + // staged AND unstaged
		"?? new.txt\x00" + // untracked
		"D  gone.go\x00" + // staged delete
		"R  new-name.go\x00old-name.go\x00" // rename: source consumed

	var g gitState
	g.repoTop = "/repo"
	g.parseStatus(out)

	if g.branch != "main" {
		t.Errorf("branch = %q, want main", g.branch)
	}
	if g.ahead != 1 || g.behind != 2 {
		t.Errorf("ahead/behind = %d/%d, want 1/2", g.ahead, g.behind)
	}

	// staged: staged.go(M), both.go(M), gone.go(D), new-name.go(R)
	if len(g.staged) != 4 {
		t.Fatalf("staged = %d, want 4: %+v", len(g.staged), g.staged)
	}
	// unstaged: dirty.go(M), both.go(M) — untracked is its own group now
	if len(g.unstaged) != 2 {
		t.Fatalf("unstaged = %d, want 2: %+v", len(g.unstaged), g.unstaged)
	}
	// untracked: new.txt(?)
	if len(g.untracked) != 1 || g.untracked[0].rel != "new.txt" || g.untracked[0].code != '?' {
		t.Fatalf("untracked = %+v, want one new.txt/?", g.untracked)
	}

	if got := g.staged[3]; got.rel != "new-name.go" || got.code != 'R' {
		t.Errorf("rename entry = %+v, want new-name.go/R", got)
	}
	if got := g.staged[0]; got.abs != "/repo/staged.go" {
		t.Errorf("abs = %q, want /repo/staged.go", got.abs)
	}

	g.rebuildRows()
	// 3 headers + 4 staged + 2 unstaged + 1 untracked = 10 rows
	if len(g.rows) != 10 {
		t.Errorf("rows = %d, want 10", len(g.rows))
	}
	if g.rows[0].kind != grHeader || g.rows[0].label != "Staged Changes (4)" {
		t.Errorf("row 0 = %+v, want Staged Changes (4) header", g.rows[0])
	}
}

func TestParseBranchVariants(t *testing.T) {
	cases := map[string]string{
		"main":                         "main",
		"feature/x...origin/feature/x": "feature/x",
		"No commits yet on trunk":      "trunk",
		"HEAD (no branch)":             "HEAD",
		"dev...origin/dev [ahead 3]":   "dev",
	}
	for in, want := range cases {
		var g gitState
		g.parseBranch(in)
		if g.branch != want {
			t.Errorf("parseBranch(%q) = %q, want %q", in, g.branch, want)
		}
	}
}

// TestGitRefreshStageCommit drives the real git subprocess flow end-to-end:
// an untracked file shows up, staging moves it between groups, and a commit
// clears the working tree.
func TestGitRefreshStageCommit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "t@t")
	run("config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	a := &App{root: dir}

	a.refreshGit()
	if !a.git.available {
		t.Fatal("expected a git repo")
	}
	if len(a.git.untracked) != 1 || a.git.untracked[0].code != '?' {
		t.Fatalf("untracked = %+v, want one untracked file", a.git.untracked)
	}
	if len(a.git.staged) != 0 {
		t.Fatalf("staged = %+v, want none", a.git.staged)
	}

	a.toggleStage(a.git.untracked[0])
	if len(a.git.staged) != 1 || a.git.staged[0].code != 'A' {
		t.Fatalf("after stage, staged = %+v, want one added file", a.git.staged)
	}
	if len(a.git.unstaged) != 0 {
		t.Fatalf("after stage, unstaged = %+v, want none", a.git.unstaged)
	}

	a.git.commitMsg = "first"
	a.commit()
	if a.git.errText != "" {
		t.Fatalf("commit error: %s", a.git.errText)
	}
	if a.git.commitMsg != "" {
		t.Fatalf("commit message should clear, got %q", a.git.commitMsg)
	}
	if len(a.git.staged) != 0 || len(a.git.unstaged) != 0 || len(a.git.untracked) != 0 {
		t.Fatalf("after commit, tree should be clean: staged=%+v unstaged=%+v untracked=%+v",
			a.git.staged, a.git.unstaged, a.git.untracked)
	}

	// The commit landed and the branch is now readable.
	log, err := a.gitCmd(dir, "log", "--oneline")
	if err != nil || !strings.Contains(log, "first") {
		t.Fatalf("git log = %q, err %v; want the 'first' commit", log, err)
	}
}

// TestAutoRefreshGitGating verifies the poll tick only touches git when the
// panel (or a diff tab) is visible, picks up out-of-band changes when it is,
// and never swallows a pending commit error.
func TestAutoRefreshGitGating(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "t@t")
	run("config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "f.txt")
	run("commit", "-qm", "init")

	a := &App{root: dir}

	// Explorer selected, no diff tab: the poll must not even read git.
	a.selected = 0
	a.autoRefreshGit()
	if a.git.loaded {
		t.Fatal("poll ran git status while Source Control was off screen")
	}

	// Open the panel, then change the file outside teddy. The next poll should
	// surface the modification with no user action.
	a.selected = 2
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("one\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a.autoRefreshGit()
	if len(a.git.unstaged) != 1 || a.git.unstaged[0].code != 'M' {
		t.Fatalf("after external edit, unstaged = %+v, want one modified file", a.git.unstaged)
	}

	// A pending error survives a background poll (nothing to act on yet).
	a.git.errText = "boom"
	a.autoRefreshGit()
	if a.git.errText != "boom" {
		t.Fatalf("poll cleared a pending error: errText = %q", a.git.errText)
	}
}
