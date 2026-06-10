package daemon

import (
	"bytes"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/calper-ql/tide/internal/client"
	"github.com/calper-ql/tide/internal/protocol"
)

func dialAttach(t *testing.T, rt, root string) (*protocol.Conn, []byte) {
	t.Helper()
	c, err := client.Dial(rt)
	if err != nil {
		t.Fatal(err)
	}
	_, snap, err := client.Attach(c, root, 100, 30)
	if err != nil {
		c.Close()
		t.Fatal(err)
	}
	return c, snap
}

// collectUntil reads stream frames until the accumulated pane output
// contains want, returning everything read.
func collectUntil(t *testing.T, c *protocol.Conn, want string) string {
	t.Helper()
	_ = c.SetDeadline(time.Now().Add(15 * time.Second))
	defer c.SetDeadline(time.Time{})
	var sb strings.Builder
	for !strings.Contains(sb.String(), want) {
		m, err := c.Recv()
		if err != nil {
			t.Fatalf("waiting for %q, got error %v after %q", want, err, sb.String())
		}
		if m.Type == protocol.TypeOutput {
			sb.Write(m.Data)
		}
	}
	return sb.String()
}

func TestPaneEchoRoundtrip(t *testing.T) {
	rt := t.TempDir()
	start(t, rt, filepath.Join(t.TempDir(), "sessions.json"))
	root := t.TempDir()

	c, _ := dialAttach(t, rt, root)
	defer c.Close()
	if err := client.SendInput(c, []byte("echo tide-marker-$((40+2))\r")); err != nil {
		t.Fatal(err)
	}
	out := collectUntil(t, c, "tide-marker-42")
	if !strings.Contains(out, "tide-marker-42") {
		t.Fatalf("output = %q", out)
	}
}

// TestAcceptanceCrashSurvival is the Phase-1 acceptance test from the spec:
// start a build in a pane, kill the terminal outright (abrupt connection
// death, no goodbye), reattach from a new terminal — build output intact,
// mid-keystroke.
func TestAcceptanceCrashSurvival(t *testing.T) {
	rt := t.TempDir()
	start(t, rt, filepath.Join(t.TempDir(), "sessions.json"))
	root := t.TempDir()

	a, _ := dialAttach(t, rt, root)
	// A "build": 60 lines, more than the 30-row screen, so the head must
	// survive via daemon-side scrollback.
	build := "i=1; while [ $i -le 60 ]; do echo build-line-$i; i=$((i+1)); done\r"
	if err := client.SendInput(a, []byte(build)); err != nil {
		t.Fatal(err)
	}
	collectUntil(t, a, "build-line-60")

	// Mid-keystroke: half a command typed, never submitted.
	if err := client.SendInput(a, []byte("echo par")); err != nil {
		t.Fatal(err)
	}
	collectUntil(t, a, "echo par") // the echo reached the pane grid

	// Kill the terminal. No detach message, no cleanup — the connection
	// just dies.
	a.Close()

	// Reattach from a "new terminal": the snapshot must contain the whole
	// build (head from scrollback, tail on screen) and the half-typed
	// command.
	b, snap := dialAttach(t, rt, root)
	defer b.Close()
	for _, want := range []string{"build-line-1", "build-line-35", "build-line-60", "echo par"} {
		if !bytes.Contains(snap, []byte(want)) {
			t.Fatalf("reattach snapshot missing %q", want)
		}
	}

	// Mid-keystroke continuation: finish the half-typed command. The shell
	// must see "echo partial-zzz" — the first half typed before the crash.
	if err := client.SendInput(b, []byte("tial-zzz\r")); err != nil {
		t.Fatal(err)
	}
	out := collectUntil(t, b, "partial-zzz")
	if !strings.Contains(out, "partial-zzz") {
		t.Fatalf("continuation output = %q", out)
	}
}

func TestShellExitNotifiesAndReattachRespawns(t *testing.T) {
	rt := t.TempDir()
	start(t, rt, filepath.Join(t.TempDir(), "sessions.json"))
	root := t.TempDir()

	a, _ := dialAttach(t, rt, root)
	if err := client.SendInput(a, []byte("exit 7\r")); err != nil {
		t.Fatal(err)
	}
	_ = a.SetDeadline(time.Now().Add(15 * time.Second))
	for {
		m, err := a.Recv()
		if err != nil {
			t.Fatalf("waiting for exit notice: %v", err)
		}
		if m.Type == protocol.TypeExit {
			if m.ExitStatus != 7 {
				t.Fatalf("exit status = %d, want 7", m.ExitStatus)
			}
			break
		}
	}
	a.Close()

	// The session survived the shell's death (prime rule); reattach starts
	// a fresh shell that works.
	if s := sessionList(t, rt); len(s) != 1 {
		t.Fatalf("sessions after shell exit = %+v", s)
	}
	b, snap := dialAttach(t, rt, root)
	defer b.Close()
	if !bytes.Contains(snap, []byte("shell exited (status 7)")) {
		t.Fatal("snapshot missing the exit notice")
	}
	if err := client.SendInput(b, []byte("echo respawned-ok\r")); err != nil {
		t.Fatal(err)
	}
	collectUntil(t, b, "respawned-ok")
}

func TestPaneContentSurvivesDaemonRestart(t *testing.T) {
	rt := t.TempDir()
	statePath := filepath.Join(t.TempDir(), "sessions.json")
	done := start(t, rt, statePath)
	root := t.TempDir()

	a, _ := dialAttach(t, rt, root)
	if err := client.SendInput(a, []byte("echo marker-persists\r")); err != nil {
		t.Fatal(err)
	}
	collectUntil(t, a, "marker-persists")
	a.Close()

	// Daemon shutdown checkpoints synchronously; a new daemon over the same
	// state restores the content into a fresh pane (tier-2 recovery:
	// processes die, content survives).
	sd, err := client.Dial(rt)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Shutdown(sd); err != nil {
		t.Fatal(err)
	}
	sd.Close()
	if err := <-done; err != nil {
		t.Fatalf("daemon exit: %v", err)
	}

	start(t, rt, statePath)
	b, snap := dialAttach(t, rt, root)
	defer b.Close()
	if !bytes.Contains(snap, []byte("marker-persists")) {
		t.Fatal("restored snapshot missing checkpointed content")
	}
	if !bytes.Contains(snap, []byte("restored from checkpoint")) {
		t.Fatal("restored snapshot missing the restore notice")
	}
	if err := client.SendInput(b, []byte("echo alive-again\r")); err != nil {
		t.Fatal(err)
	}
	collectUntil(t, b, "alive-again")
}

func TestResizePropagatesToShell(t *testing.T) {
	rt := t.TempDir()
	start(t, rt, filepath.Join(t.TempDir(), "sessions.json"))
	root := t.TempDir()

	c, _ := dialAttach(t, rt, root)
	defer c.Close()
	if err := client.SendResize(c, 73, 19); err != nil {
		t.Fatal(err)
	}
	// stty reads the PTY size the daemon set; eventual because resize is
	// fire-and-forget.
	if err := client.SendInput(c, []byte("sleep 0.2; echo size-$(stty size | tr ' ' 'x')\r")); err != nil {
		t.Fatal(err)
	}
	out := collectUntil(t, c, "size-19x73")
	if !strings.Contains(out, "size-19x73") {
		t.Fatalf("output = %q", out)
	}
}

func TestMultiClientSeesSameStream(t *testing.T) {
	rt := t.TempDir()
	start(t, rt, filepath.Join(t.TempDir(), "sessions.json"))
	root := t.TempDir()

	a, _ := dialAttach(t, rt, root)
	defer a.Close()
	b, _ := dialAttach(t, rt, root)
	defer b.Close()

	marker := fmt.Sprintf("both-see-%d", time.Now().UnixNano())
	if err := client.SendInput(a, []byte("echo "+marker+"\r")); err != nil {
		t.Fatal(err)
	}
	collectUntil(t, a, marker)
	collectUntil(t, b, marker)
}
