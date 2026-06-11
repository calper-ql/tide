package daemon

import (
	"bytes"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/calper-ql/tide/internal/client"
	"github.com/calper-ql/tide/internal/paths"
	"github.com/calper-ql/tide/internal/protocol"
	"github.com/calper-ql/tide/internal/version"
)

// logBuf is a concurrency-safe log sink dumped when a test fails.
type logBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// start runs a daemon over private dirs and returns its exit channel. Tests
// must shut it down themselves (a cleanup makes that best-effort).
func start(t *testing.T, runtimeDir, statePath string) chan error {
	t.Helper()
	// Panes spawn $SHELL; pin it so tests exercise tide, not the
	// developer's shell rc (an interactive zsh that stalls in ~/.zshrc
	// never echoes the markers tests wait for). The pinned container has
	// no SHELL and already falls back to /bin/sh — this matches it.
	t.Setenv("SHELL", "/bin/sh")
	lb := &logBuf{}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("daemon log:\n%s", lb.String())
		}
	})
	done := make(chan error, 1)
	go func() {
		done <- Run(Options{RuntimeDir: runtimeDir, StatePath: statePath, Log: lb})
	}()
	waitUp(t, runtimeDir)
	t.Cleanup(func() {
		if c, err := client.Dial(runtimeDir); err == nil {
			_ = client.Shutdown(c)
			c.Close()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
			}
		}
	})
	return done
}

func waitUp(t *testing.T, runtimeDir string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if c, err := client.Dial(runtimeDir); err == nil {
			c.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("daemon did not come up")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// runtimeDir returns a fresh dir for the daemon socket. Not t.TempDir():
// that path embeds the test name, which can push the socket past the
// 104-byte sun_path limit on macOS (bind fails with EINVAL).
func runtimeDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "tide-rt-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func sessionList(t *testing.T, runtimeDir string) []protocol.SessionInfo {
	t.Helper()
	c, err := client.Dial(runtimeDir)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	sessions, err := client.Ls(c)
	if err != nil {
		t.Fatal(err)
	}
	return sessions
}

func TestSessionSurvivesClientDetachAndEndsOnlyByKill(t *testing.T) {
	rt := runtimeDir(t)
	statePath := filepath.Join(t.TempDir(), "sessions.json")
	start(t, rt, statePath)
	root := t.TempDir()
	anchor := t.TempDir() // second session so killing root doesn't end the daemon

	x, err := client.Dial(rt)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := client.Attach(x, anchor, 80, 24); err != nil {
		t.Fatal(err)
	}
	defer x.Close()

	a, err := client.Dial(rt)
	if err != nil {
		t.Fatal(err)
	}
	info, _, err := client.Attach(a, root, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	if info.Root != root || info.Clients != 1 || info.Panes != 1 {
		t.Fatalf("attach info = %+v", info)
	}

	// Multi-client attach is a v1 ruling: a second client joins the same
	// session.
	b, err := client.Dial(rt)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	info, _, err = client.Attach(b, root, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	if info.Clients != 2 {
		t.Fatalf("second attach saw %d clients, want 2", info.Clients)
	}

	// Client death is a detach; the session must survive it untouched.
	a.Close()
	eventually(t, "detach to register", func() bool {
		for _, s := range sessionList(t, rt) {
			if s.Root == root {
				return s.Clients == 1
			}
		}
		return false
	})

	// Explicit kill: remaining attached clients are told, session is gone.
	k, err := client.Dial(rt)
	if err != nil {
		t.Fatal(err)
	}
	defer k.Close()
	if err := client.Kill(k, root); err != nil {
		t.Fatal(err)
	}
	_ = b.SetDeadline(time.Now().Add(10 * time.Second))
	for {
		m, err := b.Recv()
		if err != nil {
			t.Fatalf("attached client should be notified before hangup, got %v", err)
		}
		if m.Type == protocol.TypeRender {
			continue // frames queued before the kill are fine
		}
		if m.Type != protocol.TypeKilled || m.Root != root {
			t.Fatalf("notification = %+v", m)
		}
		break
	}
	if s := sessionList(t, rt); len(s) != 1 || s[0].Root != anchor {
		t.Fatalf("sessions after kill = %+v", s)
	}

	// Killing again must fail loudly, not invent state.
	if err := client.Kill(k, root); err == nil || !strings.Contains(err.Error(), "no session") {
		t.Fatalf("second kill err = %v", err)
	}
}

func TestCheckpointSurvivesDaemonRestart(t *testing.T) {
	rt := runtimeDir(t)
	statePath := filepath.Join(t.TempDir(), "sessions.json")
	done := start(t, rt, statePath)
	root := t.TempDir()

	c, err := client.Dial(rt)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := client.Attach(c, root, 80, 24); err != nil {
		t.Fatal(err)
	}
	if err := client.Shutdown(c); err != nil {
		t.Fatal(err)
	}
	c.Close()
	if err := <-done; err != nil {
		t.Fatalf("daemon exit: %v", err)
	}

	// A new daemon over the same state file recovers the session — daemon
	// death never loses sessions (spec guarantee tier 2).
	start(t, rt, statePath)
	s := sessionList(t, rt)
	if len(s) != 1 || s[0].Root != root || s[0].Clients != 0 {
		t.Fatalf("recovered sessions = %+v", s)
	}
}

func TestSpawnRaceLoserYieldsWithoutDisturbingWinner(t *testing.T) {
	rt := runtimeDir(t)
	statePath := filepath.Join(t.TempDir(), "sessions.json")
	start(t, rt, statePath)

	c, err := client.Dial(rt)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := client.Attach(c, t.TempDir(), 80, 24); err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// A second daemon on the same runtime dir must lose the flock, return
	// promptly without error, and leave the live socket alone.
	errc := make(chan error, 1)
	go func() {
		errc <- Run(Options{RuntimeDir: rt, StatePath: statePath, Log: io.Discard})
	}()
	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("losing daemon returned %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("losing daemon did not yield")
	}

	// The winner is still serving and still has the session.
	s := sessionList(t, rt)
	if len(s) != 1 || s[0].Clients != 1 {
		t.Fatalf("sessions after race = %+v", s)
	}
}

func TestStaleSocketFileIsCleared(t *testing.T) {
	// Daemon death (e.g. SIGKILL) leaves the socket file behind; the next
	// daemon must remove it and bind (spec: stale socket — remove, spawn,
	// retry).
	rt := runtimeDir(t)
	statePath := filepath.Join(t.TempDir(), "sessions.json")
	if err := os.WriteFile(paths.SocketPath(rt), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	start(t, rt, statePath) // waitUp inside fails the test if it can't bind
}

func TestSIGTERMShutsDownCleanly(t *testing.T) {
	// SIGTERM is the version-independent shutdown path `tide restart` uses
	// against a protocol-mismatched daemon.
	rt := runtimeDir(t)
	statePath := filepath.Join(t.TempDir(), "sessions.json")
	done := start(t, rt, statePath)

	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("daemon exit: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not exit on SIGTERM")
	}
}

func TestSecondAttachOnOneConnRefused(t *testing.T) {
	rt := runtimeDir(t)
	statePath := filepath.Join(t.TempDir(), "sessions.json")
	start(t, rt, statePath)

	c, err := client.Dial(rt)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, _, err := client.Attach(c, t.TempDir(), 80, 24); err != nil {
		t.Fatal(err)
	}
	if _, _, err := client.Attach(c, t.TempDir(), 80, 24); err == nil || !strings.Contains(err.Error(), "already attached") {
		t.Fatalf("second attach err = %v", err)
	}
}

func TestCorruptStateQuarantinedAndLoggedDaemonStillServes(t *testing.T) {
	rt := runtimeDir(t)
	stateDir := t.TempDir()
	statePath := filepath.Join(stateDir, "sessions.json")
	if err := os.WriteFile(statePath, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	start(t, rt, statePath) // a corrupt checkpoint must not brick the daemon
	if s := sessionList(t, rt); len(s) != 0 {
		t.Fatalf("sessions = %+v, want empty after quarantine", s)
	}
	matches, err := filepath.Glob(statePath + ".corrupt-*")
	if err != nil || len(matches) != 1 {
		t.Fatalf("quarantine file: %v %v", matches, err)
	}
}

func TestProtocolMismatchRefusedWithoutKillingAnything(t *testing.T) {
	rt := runtimeDir(t)
	statePath := filepath.Join(t.TempDir(), "sessions.json")
	start(t, rt, statePath)

	a, err := client.Dial(rt)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if _, _, err := client.Attach(a, t.TempDir(), 80, 24); err != nil {
		t.Fatal(err)
	}

	// A client from the future: hello parses, version differs.
	nc, err := net.Dial("unix", paths.SocketPath(rt))
	if err != nil {
		t.Fatal(err)
	}
	raw := protocol.NewConn(nc)
	defer raw.Close()
	if m, err := raw.Recv(); err != nil || m.Type != protocol.TypeHello {
		t.Fatalf("server hello = %+v, %v", m, err)
	}
	if err := raw.Send(protocol.Message{
		Type: protocol.TypeHello, BinaryVersion: "9.9.9", ProtocolVersion: version.Protocol + 1,
	}); err != nil {
		t.Fatal(err)
	}
	m, err := raw.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if m.Type != protocol.TypeError || !strings.Contains(m.Err, "tide restart") {
		t.Fatalf("mismatch reply = %+v, want error pointing at 'tide restart'", m)
	}

	// Nothing was killed: the session and its client are untouched.
	s := sessionList(t, rt)
	if len(s) != 1 || s[0].Clients != 1 {
		t.Fatalf("sessions after mismatch = %+v", s)
	}
}
