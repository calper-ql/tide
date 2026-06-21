package main

import (
	"bytes"
	"net"
	"os/exec"
	"testing"
	"time"

	"github.com/calper-ql/tide/internal/protocol"
	"github.com/calper-ql/tide/internal/version"
)

// TestPipeConnFramesProtocol proves the stdio shim carries protocol frames:
// a server and client handshake over crossed os-pipe-backed pipeConns and
// exchange a frame. This is the adapter `tide -r` wraps ssh's stdio in.
func TestPipeConnFramesProtocol(t *testing.T) {
	// Two net.Pipe ends stand in for the ssh stdio (full duplex, in-process).
	a, b := net.Pipe()
	server := protocol.NewConn(&pipeConn{r: a, w: a, closeFn: a.Close})
	client := protocol.NewConn(&pipeConn{r: b, w: b, closeFn: b.Close})
	defer server.Close()
	defer client.Close()

	done := make(chan error, 1)
	go func() {
		if _, err := server.ServerHandshake(); err != nil {
			done <- err
			return
		}
		m, err := server.Recv()
		if err != nil {
			done <- err
			return
		}
		// Echo the input back as a render frame.
		done <- server.Send(protocol.Message{Type: protocol.TypeRender, Data: m.Data})
	}()

	if _, err := client.ClientHandshake(); err != nil {
		t.Fatalf("client handshake: %v", err)
	}
	if err := client.Send(protocol.Message{Type: protocol.TypeInput, Data: []byte("ping")}); err != nil {
		t.Fatalf("send: %v", err)
	}
	m, err := client.Recv()
	if err != nil {
		t.Fatalf("recv: %v", err)
	}
	if m.Type != protocol.TypeRender || string(m.Data) != "ping" {
		t.Fatalf("got %q %q, want render \"ping\"", m.Type, m.Data)
	}
	if err := <-done; err != nil {
		t.Fatalf("server side: %v", err)
	}
}

// TestRelayPumpsBothWays exercises the transport pump: the caller's input
// reaches the daemon, and the daemon's frames (crucially TypeCopy — the
// clipboard) reach the caller. This pins the invariant that makes remote copy
// work: the copy frame flows back to the caller's client, which runs the
// clipboard tool locally.
func TestRelayPumpsBothWays(t *testing.T) {
	laptopA, laptopB := net.Pipe() // the "ssh pipe"
	daemonA, daemonB := net.Pipe() // serve <-> daemon

	relayCaller := protocol.NewConn(&pipeConn{r: laptopA, w: laptopA, closeFn: laptopA.Close})
	relayDaemon := protocol.NewConn(&pipeConn{r: daemonA, w: daemonA, closeFn: daemonA.Close})
	laptop := protocol.NewConn(&pipeConn{r: laptopB, w: laptopB, closeFn: laptopB.Close})
	daemon := protocol.NewConn(&pipeConn{r: daemonB, w: daemonB, closeFn: daemonB.Close})

	relayDone := make(chan error, 1)
	go func() { relayDone <- relay(relayCaller, relayDaemon) }()

	// Fake daemon: read the relayed input, then push a clipboard frame back.
	daemonGot := make(chan protocol.Message, 1)
	go func() {
		m, err := daemon.Recv()
		if err != nil {
			t.Errorf("daemon recv: %v", err)
			return
		}
		daemonGot <- m
		_ = daemon.Send(protocol.Message{Type: protocol.TypeCopy, Target: protocol.CopyClipboard, Data: []byte("from-remote")})
	}()

	// Input flows caller -> daemon.
	if err := laptop.Send(protocol.Message{Type: protocol.TypeInput, Data: []byte("xy")}); err != nil {
		t.Fatalf("input: %v", err)
	}
	select {
	case m := <-daemonGot:
		if m.Type != protocol.TypeInput || string(m.Data) != "xy" {
			t.Fatalf("daemon got %q %q, want input \"xy\"", m.Type, m.Data)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("input never reached the daemon")
	}
	// Clipboard flows daemon -> caller (the whole point of remote copy).
	cp, err := laptop.Recv()
	if err != nil {
		t.Fatalf("recv copy: %v", err)
	}
	if cp.Type != protocol.TypeCopy || cp.Target != protocol.CopyClipboard || string(cp.Data) != "from-remote" {
		t.Fatalf("copy frame = %q/%q/%q, want clipboard from-remote", cp.Type, cp.Target, cp.Data)
	}

	// Tearing down a side ends the relay cleanly.
	laptop.Close()
	daemon.Close()
	select {
	case <-relayDone:
	case <-time.After(3 * time.Second):
		t.Fatal("relay did not exit after the pipes closed")
	}
}

func TestBuildRemoteCmdFindsTideWithoutPATH(t *testing.T) {
	// Default: prefer PATH, fall back to the `tide install` location, so
	// neither a missing PATH entry nor a shell alias matters.
	cmd := buildRemoteCmd("", nil)
	if !contains(cmd, "$HOME/.local/bin/tide") || !contains(cmd, "command -v tide") || !contains(cmd, "--serve") {
		t.Fatalf("default remote cmd = %q", cmd)
	}
	// A path arg is shell-quoted (survives spaces).
	if c := buildRemoteCmd("", []string{"/srv/my app"}); !contains(c, "'/srv/my app'") {
		t.Fatalf("path not shell-quoted: %q", c)
	}
	// --remote-bin execs that binary directly.
	if c := buildRemoteCmd("/opt/tide", []string{"--here"}); !contains(c, "exec '/opt/tide' --serve") || !contains(c, "'--here'") {
		t.Fatalf("remote-bin cmd = %q", c)
	}
}

func TestParseRemoteAttach(t *testing.T) {
	dest, bin, serve := parseRemoteAttach([]string{"user@host", "/proj"})
	if dest != "user@host" || bin != "" || len(serve) != 1 || serve[0] != "/proj" {
		t.Fatalf("plain: dest=%q bin=%q serve=%v", dest, bin, serve)
	}
	dest, bin, serve = parseRemoteAttach([]string{"--remote-bin", "/opt/tide", "h", "--here"})
	if dest != "h" || bin != "/opt/tide" || len(serve) != 1 || serve[0] != "--here" {
		t.Fatalf("flag: dest=%q bin=%q serve=%v", dest, bin, serve)
	}
	if dest, bin, _ := parseRemoteAttach([]string{"--remote-bin=/x/tide", "host"}); dest != "host" || bin != "/x/tide" {
		t.Fatalf("eq-flag: dest=%q bin=%q", dest, bin)
	}
}

func TestRemoteDialErrorClassifies(t *testing.T) {
	// Protocol mismatch → actionable "update tide on host" message.
	mm := &protocol.MismatchError{PeerBinary: "0.0.9", PeerProtocol: 2}
	if got := remoteDialError("zeus", mm, &bytes.Buffer{}, nil).Error(); !contains(got, "protocol 2") || !contains(got, "update tide on zeus") {
		t.Fatalf("mismatch error = %q", got)
	}

	// tide missing on PATH → real exit-127 ProcessState from a child.
	c := exec.Command("sh", "-c", "exit 127")
	_ = c.Run()
	notFound := remoteDialError("zeus", errEOF{}, bytes.NewBufferString("bash: tide: command not found\n"), c.ProcessState).Error()
	if !contains(notFound, "non-interactive PATH") || !contains(notFound, "tide install") {
		t.Fatalf("not-found error = %q", notFound)
	}

	// Some other ssh failure → surfaced verbatim, not misreported as missing.
	other := remoteDialError("zeus", errEOF{}, bytes.NewBufferString("Permission denied (publickey).\n"), nil).Error()
	if contains(other, "PATH") || !contains(other, "Permission denied") {
		t.Fatalf("other error = %q", other)
	}

	_ = version.Protocol // referenced in the mismatch message
}

type errEOF struct{}

func (errEOF) Error() string { return "EOF" }

func contains(s, sub string) bool { return bytes.Contains([]byte(s), []byte(sub)) }
