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
	_, frame, err := client.Attach(c, root, 100, 30)
	if err != nil {
		c.Close()
		t.Fatal(err)
	}
	return c, frame
}

// collectRender accumulates render frames (on top of seed, normally the
// attach frame) until the composed output contains want.
func collectRender(t *testing.T, c *protocol.Conn, seed []byte, want string) string {
	t.Helper()
	_ = c.SetDeadline(time.Now().Add(15 * time.Second))
	defer c.SetDeadline(time.Time{})
	var sb strings.Builder
	sb.Write(seed)
	for !strings.Contains(sb.String(), want) {
		m, err := c.Recv()
		if err != nil {
			tail := sb.String()
			if len(tail) > 300 {
				tail = tail[len(tail)-300:]
			}
			t.Fatalf("waiting for %q, got error %v; tail %q", want, err, tail)
		}
		if m.Type == protocol.TypeRender {
			sb.Write(m.Data)
		}
	}
	return sb.String()
}

func TestPaneEchoRoundtrip(t *testing.T) {
	rt := t.TempDir()
	start(t, rt, filepath.Join(t.TempDir(), "sessions.json"))
	root := t.TempDir()

	c, frame := dialAttach(t, rt, root)
	defer c.Close()
	if err := client.SendInput(c, []byte("echo tide-marker-$((40+2))\r")); err != nil {
		t.Fatal(err)
	}
	collectRender(t, c, frame, "tide-marker-42")
}

// TestAcceptanceCrashSurvival is the Phase-1 acceptance test from the spec:
// start a build in a pane, kill the terminal outright (abrupt connection
// death, no goodbye), reattach from a new terminal — build output intact
// (the head via wheel-scrollback, the daemon owns it now), mid-keystroke.
func TestAcceptanceCrashSurvival(t *testing.T) {
	rt := t.TempDir()
	start(t, rt, filepath.Join(t.TempDir(), "sessions.json"))
	root := t.TempDir()

	a, aframe := dialAttach(t, rt, root)
	build := "i=1; while [ $i -le 60 ]; do echo build-line-$i; i=$((i+1)); done\r"
	if err := client.SendInput(a, []byte(build)); err != nil {
		t.Fatal(err)
	}
	collectRender(t, a, aframe, "build-line-60")

	// Mid-keystroke: half a command typed, never submitted.
	if err := client.SendInput(a, []byte("echo par")); err != nil {
		t.Fatal(err)
	}
	collectRender(t, a, nil, "echo par")

	// Kill the terminal: the connection just dies.
	a.Close()

	// Reattach: the screen tail and the half-typed command are in the
	// attach frame.
	b, bframe := dialAttach(t, rt, root)
	defer b.Close()
	for _, want := range []string{"build-line-60", "echo par"} {
		if !bytes.Contains(bframe, []byte(want)) {
			t.Fatalf("reattach frame missing %q", want)
		}
	}

	// The head of the build scrolled off-screen; the daemon-side scrollback
	// serves it to the mouse wheel (clients run in the alt screen, native
	// scrollback is tide's job now).
	var got string
	for i := 0; i < 20 && !strings.Contains(got, "build-line-1\r"); i++ {
		if err := client.SendInput(b, []byte("\x1b[<64;10;10M")); err != nil {
			t.Fatal(err)
		}
		got += collectRender(t, b, nil, "SCROLL")
	}
	if !strings.Contains(got, "build-line-1") {
		t.Fatal("scrollback did not reach the head of the build output")
	}

	// Mid-keystroke continuation: any key snaps live; finishing the command
	// must execute "echo partial-zzz" — the first half typed pre-crash.
	if err := client.SendInput(b, []byte("tial-zzz\r")); err != nil {
		t.Fatal(err)
	}
	collectRender(t, b, nil, "partial-zzz")
}

func TestShellExitNoticeAndClickRestarts(t *testing.T) {
	rt := t.TempDir()
	start(t, rt, filepath.Join(t.TempDir(), "sessions.json"))
	root := t.TempDir()

	c, frame := dialAttach(t, rt, root)
	defer c.Close()
	if err := client.SendInput(c, []byte("exit 7\r")); err != nil {
		t.Fatal(err)
	}
	collectRender(t, c, frame, "shell exited (status 7)")

	// The session survives the shell's death (prime rule); clicking the
	// dead pane restarts it.
	if s := sessionList(t, rt); len(s) != 1 {
		t.Fatalf("sessions after shell exit = %+v", s)
	}
	if err := client.SendInput(c, []byte("\x1b[<0;10;10M\x1b[<0;10;10m")); err != nil {
		t.Fatal(err)
	}
	if err := client.SendInput(c, []byte("echo respawned-ok\r")); err != nil {
		t.Fatal(err)
	}
	collectRender(t, c, nil, "respawned-ok")
}

func TestPaneContentSurvivesDaemonRestart(t *testing.T) {
	rt := t.TempDir()
	statePath := filepath.Join(t.TempDir(), "sessions.json")
	done := start(t, rt, statePath)
	root := t.TempDir()

	a, aframe := dialAttach(t, rt, root)
	if err := client.SendInput(a, []byte("echo marker-persists\r")); err != nil {
		t.Fatal(err)
	}
	collectRender(t, a, aframe, "marker-persists")
	a.Close()

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
	b, bframe := dialAttach(t, rt, root)
	defer b.Close()
	if !bytes.Contains(bframe, []byte("restored from checkpoint")) {
		t.Fatal("restored frame missing the restore notice")
	}
	// The marker line scrolled up with the restore notice; it is either on
	// screen or one wheel-step away.
	if !bytes.Contains(bframe, []byte("marker-persists")) {
		if err := client.SendInput(b, []byte("\x1b[<64;10;10M")); err != nil {
			t.Fatal(err)
		}
		collectRender(t, b, nil, "marker-persists")
	}
}

func TestResizePropagatesToShell(t *testing.T) {
	rt := t.TempDir()
	start(t, rt, filepath.Join(t.TempDir(), "sessions.json"))
	root := t.TempDir()

	c, _ := dialAttach(t, rt, root)
	defer c.Close()
	// 73x19 client minus session bar, pane bar, bottom ring = 16 rows;
	// minus the side ring columns = 71 cols.
	if err := client.SendResize(c, 73, 19); err != nil {
		t.Fatal(err)
	}
	if err := client.SendInput(c, []byte("sleep 0.2; echo size-$(stty size | tr ' ' 'x')\r")); err != nil {
		t.Fatal(err)
	}
	collectRender(t, c, nil, "size-16x71")
}

func TestMultiClientSeesSameComposition(t *testing.T) {
	rt := t.TempDir()
	start(t, rt, filepath.Join(t.TempDir(), "sessions.json"))
	root := t.TempDir()

	a, aframe := dialAttach(t, rt, root)
	defer a.Close()
	b, bframe := dialAttach(t, rt, root)
	defer b.Close()

	marker := fmt.Sprintf("both-see-%d", time.Now().UnixNano())
	if err := client.SendInput(a, []byte("echo "+marker+"\r")); err != nil {
		t.Fatal(err)
	}
	collectRender(t, a, aframe, marker)
	collectRender(t, b, bframe, marker)
}

func TestSecondCtrlCInterruptsAfterCopy(t *testing.T) {
	rt := t.TempDir()
	start(t, rt, filepath.Join(t.TempDir(), "sessions.json"))
	root := t.TempDir()

	c, frame := dialAttach(t, rt, root)
	defer c.Close()
	if err := client.SendInput(c, []byte("echo select-source-text; sleep 30\r")); err != nil {
		t.Fatal(err)
	}
	collectRender(t, c, frame, "select-source-text")

	// Drag-select across most of the pane (the exact prompt geometry varies
	// by shell, so the drag spans enough rows to be sure it covers text),
	// then Ctrl+C: the ruling says copy, not SIGINT — sleep must survive.
	if err := client.SendInput(c, []byte("\x1b[<0;3;4M\x1b[<32;60;18M\x1b[<0;60;18m")); err != nil {
		t.Fatal(err)
	}
	collectRender(t, c, nil, "\x1b]52;p;") // selection fed PRIMARY
	if err := client.SendInput(c, []byte{0x03}); err != nil {
		t.Fatal(err)
	}
	collectRender(t, c, nil, "\x1b]52;c;") // copy hit the clipboard
	collectRender(t, c, nil, "copied")     // and the bar said so

	// Second Ctrl+C: no selection anymore — the byte reaches the shell and
	// kills the sleep; the next command proves the shell is responsive.
	if err := client.SendInput(c, []byte{0x03}); err != nil {
		t.Fatal(err)
	}
	if err := client.SendInput(c, []byte("echo after-interrupt\r")); err != nil {
		t.Fatal(err)
	}
	collectRender(t, c, nil, "after-interrupt")
}

func TestKillingLastSessionExitsDaemon(t *testing.T) {
	rt := t.TempDir()
	statePath := filepath.Join(t.TempDir(), "sessions.json")
	done := start(t, rt, statePath)
	rootA, rootB := t.TempDir(), t.TempDir()

	a, _ := dialAttach(t, rt, rootA)
	defer a.Close()
	b, _ := dialAttach(t, rt, rootB)
	b.Close() // detach; session B stays

	// Killing one of two sessions leaves the daemon up.
	k, err := client.Dial(rt)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Kill(k, rootB); err != nil {
		t.Fatal(err)
	}
	if s := sessionList(t, rt); len(s) != 1 {
		t.Fatalf("sessions = %+v, want only A", s)
	}

	// Killing the last session ends the daemon (ruled 2026-06-10).
	if err := client.Kill(k, rootA); err != nil {
		t.Fatal(err)
	}
	k.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("daemon exit: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("daemon did not exit after its last session was killed")
	}
}
