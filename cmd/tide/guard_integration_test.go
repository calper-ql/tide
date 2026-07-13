package main

import (
	"bytes"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/term"

	"github.com/calper-ql/tide/internal/protocol"
	"github.com/creack/pty"
)

// TestStreamSessionHoldsTerminalAndGuardsPasswordAfterDrop drives the real
// streamSession over a real PTY and proves the end-to-end security behavior:
// when the connection drops, tide paints the "connection lost" notice, holds
// the terminal, and swallows a password the user keeps typing (up to Enter)
// instead of letting it fall through to the shell. Without the guard,
// streamSession would return the moment the drop was seen and those bytes
// would land in the caller's shell history.
func TestStreamSessionHoldsTerminalAndGuardsPasswordAfterDrop(t *testing.T) {
	ptmx, tty, err := pty.Open() // ptmx = the "user's terminal", tty = the process side
	if err != nil {
		t.Skipf("pty unavailable in this environment: %v", err)
	}
	defer ptmx.Close()
	defer tty.Close()

	// streamSession reads os.Stdin and writes os.Stdout directly; point them at
	// the pty slave for the duration of the test, then put them back.
	oldIn, oldOut := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = tty, tty
	defer func() { os.Stdin, os.Stdout = oldIn, oldOut }()

	// A fake daemon on an in-process pipe. Closing daemonEnd is the "drop".
	daemonEnd, clientEnd := net.Pipe()
	daemon := protocol.NewConn(&pipeConn{r: daemonEnd, w: daemonEnd, closeFn: daemonEnd.Close})
	client := protocol.NewConn(&pipeConn{r: clientEnd, w: clientEnd, closeFn: clientEnd.Close})

	type outcome struct {
		reason string
		err    error
	}
	ret := make(chan outcome, 1)
	winch := make(chan os.Signal, 1)
	go func() {
		reason, e := streamSession(client, int(tty.Fd()), nil, winch,
			"connection lost — the session keeps running there", nil)
		ret <- outcome{reason, e}
	}()

	// Continuously drain the terminal side so the pty buffer never blocks, and
	// record everything painted so we can assert the notice appears.
	var mu = make(chan []byte, 256)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, rerr := ptmx.Read(buf)
			if n > 0 {
				mu <- append([]byte(nil), buf[:n]...)
			}
			if rerr != nil {
				return
			}
		}
	}()

	// Drop the connection. The output goroutine's Recv fails, and streamSession
	// paints the loss notice and starts guarding input.
	daemon.Close()

	// Wait until the notice reaches the terminal — proof tide is holding it.
	if !waitFor(mu, "connection lost", 5*time.Second) {
		t.Fatal("streamSession never painted the connection-lost notice after the drop")
	}

	// The user, unaware, keeps typing a password blind, then presses Enter.
	// The guard must swallow it: streamSession returns because it saw Enter,
	// and it must not have hung.
	if _, err := ptmx.WriteString("sup3r-secret-pw\r"); err != nil {
		t.Fatalf("write to pty: %v", err)
	}

	select {
	case r := <-ret:
		if r.err == nil {
			t.Fatalf("streamSession returned nil error on a drop; want the connection-lost error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("streamSession hung after the drop — the input guard never released on Enter")
	}
}

// TestFlushTTYInputDropsQueuedPartialPassword covers the non-timeout handshake
// path: ssh died on its own while a password sat half-typed (no Enter) in the
// tty's canonical line buffer. flushTTYInput must discard it so it can't be
// prepended to the shell's next command line.
func TestFlushTTYInputDropsQueuedPartialPassword(t *testing.T) {
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Skipf("pty unavailable: %v", err)
	}
	defer ptmx.Close()
	defer tty.Close()

	pristine, err := term.GetState(int(tty.Fd())) // cooked + echo, as ssh's death leaves it
	if err != nil {
		t.Skipf("GetState: %v", err)
	}

	// The user typed a password blind, no Enter yet — it sits queued in the
	// canonical line buffer.
	if _, err := ptmx.WriteString("secr3t-pw"); err != nil {
		t.Fatalf("write: %v", err)
	}
	time.Sleep(100 * time.Millisecond) // let it reach the tty input queue

	flushTTYInput(int(tty.Fd()), pristine)

	// Now the shell would read its next line. Send one and confirm the flushed
	// password is NOT prepended to it.
	if _, err := ptmx.WriteString("whoami\n"); err != nil {
		t.Fatalf("write2: %v", err)
	}
	_ = tty.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 256)
	n, err := tty.Read(buf)
	if err != nil {
		t.Fatalf("read after flush: %v", err)
	}
	got := string(buf[:n])
	if strings.Contains(got, "secr3t-pw") {
		t.Fatalf("flush leaked the queued password into the shell's line: %q", got)
	}
	if !strings.Contains(got, "whoami") {
		t.Fatalf("expected the shell to still receive its own input, got %q", got)
	}
}

// waitFor reads painted chunks until their concatenation contains sub, or the
// deadline passes.
func waitFor(ch <-chan []byte, sub string, d time.Duration) bool {
	deadline := time.After(d)
	var acc []byte
	for {
		select {
		case b := <-ch:
			acc = append(acc, b...)
			if bytes.Contains(acc, []byte(sub)) {
				return true
			}
		case <-deadline:
			return false
		}
	}
}
