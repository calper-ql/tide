package main

import (
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"
)

// chunkReader hands back one predefined chunk per Read (a real tty in raw
// mode dribbles keystrokes the same way), then io.EOF. It records how many
// Reads happened so a test can prove the guard stopped at Enter and did not
// keep consuming — the bytes AFTER Enter must be left for the shell.
type chunkReader struct {
	chunks [][]byte
	i      int
	reads  int
}

func (c *chunkReader) Read(p []byte) (int, error) {
	c.reads++
	if c.i >= len(c.chunks) {
		return 0, io.EOF
	}
	n := copy(p, c.chunks[c.i])
	c.i++
	return n, nil
}

// TestDrainUntilEnterStopsAtFirstEnter is the security invariant: everything
// up to and including the Enter is swallowed, and reading stops there — the
// next line (what the user types after realizing they were disconnected) is
// NOT consumed by the guard.
func TestDrainUntilEnterStopsAtFirstEnter(t *testing.T) {
	r := &chunkReader{chunks: [][]byte{
		[]byte("hunter"), // the password, typed blind, chunk 1
		[]byte("2"),      // chunk 2
		[]byte("\r"),     // Enter — the guard must stop HERE
		[]byte("whoami"), // must never be read by the guard
	}}
	drainUntilEnter(r)
	// Reads: "hunter", "2", "\r" (returns) → 3 reads. Never reaches "whoami".
	if r.reads != 3 {
		t.Fatalf("drainUntilEnter did %d reads, want 3 (stop at the Enter chunk)", r.reads)
	}
	if r.i != 3 {
		t.Fatalf("guard consumed %d chunks, want 3 — it read past the Enter", r.i)
	}
}

// A newline (LF) counts as Enter too, and Enter arriving in the same chunk as
// the secret still terminates the drain.
func TestDrainUntilEnterHandlesLFAndInlineEnter(t *testing.T) {
	lf := &chunkReader{chunks: [][]byte{[]byte("secret\n"), []byte("rm -rf x")}}
	drainUntilEnter(lf)
	if lf.reads != 1 || lf.i != 1 {
		t.Fatalf("inline LF: reads=%d consumed=%d, want 1/1", lf.reads, lf.i)
	}
}

// With no Enter at all, the guard drains to EOF and returns rather than
// hanging — a closed terminal (SIGHUP) must still release cleanly.
func TestDrainUntilEnterReturnsOnEOF(t *testing.T) {
	done := make(chan struct{})
	go func() {
		drainUntilEnter(&chunkReader{chunks: [][]byte{[]byte("no newline here")}})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("drainUntilEnter did not return on EOF")
	}
}

// TestStdinPumpForwardsThenGuardsOnSendError is the mid-session drop: input
// flows to the daemon until a send fails, after which NOTHING more is
// forwarded and the trailing keystrokes (the rest of a password + Enter) are
// swallowed. The pump reports firstLoss so the caller wakes the main loop.
func TestStdinPumpForwardsThenGuardsOnSendError(t *testing.T) {
	var sent [][]byte
	live := true // the "connection": drops after the first successful send
	send := func(b []byte) error {
		if !live {
			return errors.New("broken pipe")
		}
		live = false // this send succeeds, the next fails
		sent = append(sent, b)
		return nil
	}
	r := &chunkReader{chunks: [][]byte{
		[]byte("ls\r"),     // forwarded (connection still up)
		[]byte("pass"),     // send fails here → loss detected, guarding begins
		[]byte("word"),     // discarded
		[]byte("\r"),       // Enter → stop
		[]byte("rm -rf /"), // MUST NOT be read by the guard
	}}
	var lost atomic.Bool
	outcome, firstLoss := stdinPump(r, send, &lost)

	if outcome != pumpLost || !firstLoss {
		t.Fatalf("outcome=%v firstLoss=%v, want pumpLost/true", outcome, firstLoss)
	}
	if !lost.Load() {
		t.Fatal("pump did not latch lost after the send error")
	}
	if len(sent) != 1 || string(sent[0]) != "ls\r" {
		t.Fatalf("forwarded %q, want exactly [\"ls\\r\"] — a secret was forwarded after the drop", sent)
	}
	if r.i != 4 { // ls\r, pass, word, \r — stops at the Enter, never reads "rm -rf /"
		t.Fatalf("guard consumed %d chunks, want 4 (stop at Enter, leave the next line)", r.i)
	}
}

// TestStdinPumpGuardsWhenPeerAlreadyLost covers the common case: the OUTPUT
// goroutine saw the recv error first and latched lost. The pump must then
// forward nothing and guard immediately — firstLoss is false because it was
// not the discoverer.
func TestStdinPumpGuardsWhenPeerAlreadyLost(t *testing.T) {
	var lost atomic.Bool
	lost.Store(true)
	sendCalled := false
	send := func(b []byte) error { sendCalled = true; return nil }
	r := &chunkReader{chunks: [][]byte{[]byte("secret"), []byte("\r"), []byte("after")}}

	outcome, firstLoss := stdinPump(r, send, &lost)
	if outcome != pumpLost || firstLoss {
		t.Fatalf("outcome=%v firstLoss=%v, want pumpLost/false", outcome, firstLoss)
	}
	if sendCalled {
		t.Fatal("pump forwarded a keystroke to the daemon after the connection was already lost")
	}
	if r.i != 2 { // "secret", "\r" → stop; never reads "after"
		t.Fatalf("guard consumed %d chunks, want 2", r.i)
	}
}

// TestStdinPumpCleanStdinEOF is the ordinary detach: stdin closes while the
// connection is healthy. No guard, no loss — the caller reports a clean detach.
func TestStdinPumpCleanStdinEOF(t *testing.T) {
	var sent [][]byte
	send := func(b []byte) error { sent = append(sent, b); return nil }
	r := &chunkReader{chunks: [][]byte{[]byte("whoami\r")}}
	var lost atomic.Bool

	outcome, firstLoss := stdinPump(r, send, &lost)
	if outcome != pumpStdinEOF || firstLoss || lost.Load() {
		t.Fatalf("outcome=%v firstLoss=%v lost=%v, want pumpStdinEOF/false/false", outcome, firstLoss, lost.Load())
	}
	if len(sent) != 1 || string(sent[0]) != "whoami\r" {
		t.Fatalf("forwarded %q, want [\"whoami\\r\"]", sent)
	}
}
