package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/calper-ql/tide/internal/protocol"
)

func TestAttachRefusesNestingInsideTideSession(t *testing.T) {
	// A shell inside a tide pane always has TIDE_SESSION set (pane.go), so
	// running `tide` there must refuse rather than stack a session in itself.
	t.Setenv("TIDE_SESSION", "pane-abc123:/run/user/1000/tide/d.sock")
	if err := attach(t.TempDir(), "", false); !errors.Is(err, errNested) {
		t.Fatalf("attach inside a tide pane = %v, want errNested", err)
	}
}

func TestKillCandidatesPrefersExactSessionOverGitWalk(t *testing.T) {
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(tmp, "repo")
	sub := filepath.Join(repo, "sub")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	candidates := killCandidates(sub, false)
	if len(candidates) != 2 || candidates[0] != sub || candidates[1] != repo {
		t.Fatalf("candidates = %v, want [%s %s]", candidates, sub, repo)
	}

	// A session created with --here in repo/sub must be the kill target
	// even though the .git walk says repo.
	sessions := []protocol.SessionInfo{{Root: sub}, {Root: repo}}
	if got := pickKillTarget(sessions, candidates); got != sub {
		t.Fatalf("picked %q, want the exact session %q", got, sub)
	}

	// Without a --here session, the .git-walk root is the target.
	sessions = []protocol.SessionInfo{{Root: repo}}
	if got := pickKillTarget(sessions, candidates); got != repo {
		t.Fatalf("picked %q, want %q", got, repo)
	}

	// --here never falls back to the repo root.
	hereCandidates := killCandidates(sub, true)
	if got := pickKillTarget(sessions, hereCandidates); got != "" {
		t.Fatalf("kill --here picked %q, want no match", got)
	}
}

func TestKillCandidatesSurviveDeletedDirectory(t *testing.T) {
	// A session can outlive its directory; kill must still be able to name
	// it by path even though stat fails.
	gone := filepath.Join(t.TempDir(), "deleted")
	candidates := killCandidates(gone, false)
	if len(candidates) == 0 || candidates[0] != gone {
		t.Fatalf("candidates = %v, want exact path first", candidates)
	}
	sessions := []protocol.SessionInfo{{Root: gone}}
	if got := pickKillTarget(sessions, candidates); got != gone {
		t.Fatalf("picked %q, want %q", got, gone)
	}
}
