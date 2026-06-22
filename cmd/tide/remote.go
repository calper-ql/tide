// Remote attach: `tide -r user@host [path]` runs the interactive client HERE
// and the daemon on the host, bridged over ssh. The client running locally is
// the whole point — its native clipboard tool writes THIS machine's clipboard,
// so copy works regardless of what the terminal does with OSC 52. No binary is
// pushed: the host must already have tide on its PATH (clear error if not),
// and a protocol mismatch is reported, never forced.
package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/calper-ql/tide/internal/client"
	"github.com/calper-ql/tide/internal/input"
	"github.com/calper-ql/tide/internal/picker"
	"github.com/calper-ql/tide/internal/protocol"
	"github.com/calper-ql/tide/internal/version"
)

// pipeConn adapts a read stream + write stream into a net.Conn so
// protocol.Conn can frame over an ssh subprocess's stdio. Deadlines delegate
// to the underlying *os.File when it supports them (pipes do), so the
// handshake timeout still bounds a wedged peer rather than hanging forever.
type pipeConn struct {
	r       io.Reader
	w       io.Writer
	closeFn func() error
}

func (p *pipeConn) Read(b []byte) (int, error)  { return p.r.Read(b) }
func (p *pipeConn) Write(b []byte) (int, error) { return p.w.Write(b) }

func (p *pipeConn) Close() error {
	if p.closeFn != nil {
		return p.closeFn()
	}
	return nil
}

func (p *pipeConn) LocalAddr() net.Addr  { return pipeAddr{} }
func (p *pipeConn) RemoteAddr() net.Addr { return pipeAddr{} }

func (p *pipeConn) SetDeadline(t time.Time) error {
	_ = p.SetReadDeadline(t)
	return p.SetWriteDeadline(t)
}

func (p *pipeConn) SetReadDeadline(t time.Time) error {
	if d, ok := p.r.(interface{ SetReadDeadline(time.Time) error }); ok {
		return d.SetReadDeadline(t)
	}
	return nil
}

func (p *pipeConn) SetWriteDeadline(t time.Time) error {
	if d, ok := p.w.(interface{ SetWriteDeadline(time.Time) error }); ok {
		return d.SetWriteDeadline(t)
	}
	return nil
}

type pipeAddr struct{}

func (pipeAddr) Network() string { return "pipe" }
func (pipeAddr) String() string  { return "pipe" }

// parseRemoteAttach splits `tide -r` args into the ssh destination, an
// optional --remote-bin override, and the residual args forwarded to
// `--serve` (a path and/or --here).
func parseRemoteAttach(args []string) (dest, remoteBin string, serveArgs []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--remote-bin" && i+1 < len(args):
			remoteBin = args[i+1]
			i++
		case strings.HasPrefix(a, "--remote-bin="):
			remoteBin = strings.TrimPrefix(a, "--remote-bin=")
		case dest == "" && !strings.HasPrefix(a, "-"):
			dest = a
		default:
			serveArgs = append(serveArgs, a)
		}
	}
	return dest, remoteBin, serveArgs
}

// buildRemoteCmd is the single command ssh runs on the host. With an explicit
// --remote-bin it execs that; otherwise it prefers a tide on PATH and falls
// back to the `tide install` default location, so neither a missing PATH entry
// nor a shell alias matters.
func buildRemoteCmd(remoteBin string, serveArgs []string) string {
	var qa string
	for _, a := range serveArgs {
		qa += " " + shquote(a)
	}
	if remoteBin != "" {
		return "exec " + shquote(remoteBin) + " --serve" + qa
	}
	return `t="$HOME/.local/bin/tide"; command -v tide >/dev/null 2>&1 && t=tide; exec "$t" --serve` + qa
}

// shquote single-quotes s for a POSIX shell (the remote $SHELL runs our
// command via -c), so paths with spaces survive.
func shquote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// titlePop restores the terminal title pushed by setTitleSeq (XTWINOPS 23;0t).
const titlePop = "\x1b[23;0t"

// setTitleSeq pushes the current terminal title (XTWINOPS 22;0t) and sets it to
// name the remote, so the tab/window shows where you are; titleSafe strips
// control bytes from the untrusted dest so it can't break out of the OSC.
func setTitleSeq(dest string) string {
	return "\x1b[22;0t\x1b]0;tide: " + titleSafe(dest) + "\a"
}

func titleSafe(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}

// parseRemoteTarget splits the residual args handed to `tide --serve` into the
// project path (a bare arg) and the --here flag, matching attach()'s shape.
func parseRemoteTarget(args []string) (target string, here bool) {
	for _, a := range args {
		switch {
		case a == "--here":
			here = true
		case !strings.HasPrefix(a, "-"):
			target = a
		}
	}
	return target, here
}

// serve runs on the HOST, invoked as `tide --serve [path]` over ssh by a
// remote caller's `tide -r`. It bridges the host's central daemon to the ssh
// stdio pipe: the caller runs the real interactive client. It SHARES the box's
// daemon (no runtime-dir override) and never kills it — an incompatible
// running daemon surfaces as an error rather than a race (the prime rule).
func serve(rt string, args []string) error {
	target, here := parseRemoteTarget(args)
	conn := protocol.NewConn(&pipeConn{r: os.Stdin, w: os.Stdout})
	if _, err := conn.ServerHandshake(); err != nil {
		// The caller's ClientHandshake already surfaced any mismatch; just exit.
		return fmt.Errorf("client handshake: %w", err)
	}

	// The caller sends its terminal size first, so we paint the picker / attach
	// the daemon at the right dimensions. A non-resize first frame is forwarded
	// to the daemon once the stream is up, so no input is lost.
	cols, rows, first := readInitialSize(conn)

	var root string
	if target == "" && !here {
		// No path given → interactive folder picker, rooted at the host $HOME.
		start, herr := os.UserHomeDir()
		if herr != nil || start == "" {
			start = "/"
		}
		chosen, c2, r2, ok, perr := runPicker(conn, start, cols, rows)
		if perr != nil {
			return perr
		}
		if !ok {
			return nil // user cancelled: clean exit, no session created
		}
		// Canonicalize (EvalSymlinks) like the explicit-path branch, so a
		// symlinked $HOME/ancestor doesn't key a second session for one dir.
		cr, _, rerr := resolveRoot(chosen, true)
		if rerr != nil {
			return rerr
		}
		root, cols, rows = cr, c2, r2
		first = protocol.Message{} // any leftover input was consumed by the picker
	} else {
		r, _, err := resolveRoot(target, here)
		if err != nil {
			return err
		}
		root = r
	}

	d, err := client.EnsureDaemon(rt)
	if err != nil {
		// MismatchError here means the host is ALREADY running an incompatible
		// daemon (an older host tide). Surface it; never kill it (prime rule).
		return err
	}
	defer d.Close()
	_, snap, err := client.Attach(d, root, cols, rows)
	if err != nil {
		return err
	}
	if err := conn.Send(protocol.Message{Type: protocol.TypeRender, Data: snap}); err != nil {
		return err
	}
	if first.Type != "" {
		_ = d.Send(first)
	}
	return relay(conn, d)
}

// readInitialSize reads the caller's first frame, expected to be a resize.
// Anything else is returned as leftover to forward to the daemon later.
func readInitialSize(conn *protocol.Conn) (cols, rows int, leftover protocol.Message) {
	cols, rows = 80, 24
	m, err := conn.Recv()
	if err != nil {
		return cols, rows, protocol.Message{}
	}
	if m.Type == protocol.TypeResize && m.Cols > 0 && m.Rows > 0 {
		return m.Cols, m.Rows, protocol.Message{}
	}
	return cols, rows, m
}

// relay pumps frames in both directions between the caller (conn) and the host
// daemon (d) until either side ends. The daemon treats the vanished caller as
// a detach, not a session end. TypeCopy frames ride back to the caller, so the
// caller's client runs the clipboard tool locally — the whole point.
func relay(conn, d *protocol.Conn) error {
	errc := make(chan error, 2)
	go func() {
		for {
			m, err := d.Recv()
			if err != nil {
				errc <- err
				return
			}
			if err := conn.Send(m); err != nil {
				errc <- err
				return
			}
		}
	}()
	go func() {
		for {
			m, err := conn.Recv()
			if err != nil {
				errc <- err
				return
			}
			if err := d.Send(m); err != nil {
				errc <- err
				return
			}
		}
	}()
	return <-errc
}

// runPicker drives the interactive folder picker over the caller connection:
// it paints the picker, decodes the caller's clicks/keys, and repaints on
// change until the user opens a folder (ok=true) or cancels (ok=false). The
// returned size reflects any resizes during the picker.
func runPicker(conn *protocol.Conn, start string, cols, rows int) (root string, fCols, fRows int, ok bool, err error) {
	m := picker.New(start, cols, rows)
	if err := conn.Send(protocol.Message{Type: protocol.TypeRender, Data: m.Render()}); err != nil {
		return "", cols, rows, false, err
	}
	dec := input.NewDecoder()
	for {
		msg, rerr := conn.Recv()
		if rerr != nil {
			return "", cols, rows, false, rerr
		}
		dirty := false
		switch msg.Type {
		case protocol.TypeResize:
			if msg.Cols > 0 && msg.Rows > 0 {
				m.Resize(msg.Cols, msg.Rows)
				dirty = true
			}
		case protocol.TypeInput:
			for _, ev := range dec.Feed(msg.Data) {
				if m.Handle(ev) {
					dirty = true
				}
			}
		}
		if chosen, picked := m.Chosen(); picked {
			c2, r2 := m.Size()
			return chosen, c2, r2, true, nil
		}
		if m.Cancelled() {
			c2, r2 := m.Size()
			return "", c2, r2, false, nil
		}
		if dirty {
			if err := conn.Send(protocol.Message{Type: protocol.TypeRender, Data: m.Render()}); err != nil {
				return "", cols, rows, false, err
			}
		}
	}
}

// remoteAttach runs HERE (the laptop): `tide -r user@host [path]`. It launches
// the host's serve bridge over ssh and drives the standard interactive client
// loop locally, so the clipboard tool runs on this machine.
func remoteAttach(args []string) error {
	dest, remoteBin, serveArgs := parseRemoteAttach(args)
	if dest == "" {
		return errors.New("usage: tide -r [--remote-bin PATH] user@host [path]")
	}

	stdinFd := int(os.Stdin.Fd())
	if !term.IsTerminal(stdinFd) || !term.IsTerminal(int(os.Stdout.Fd())) {
		return errors.New("attach requires a terminal")
	}
	cols, rows, err := term.GetSize(stdinFd)
	if err != nil {
		return err
	}

	// Invoke the remote tide WITHOUT depending on the remote PATH: prefer a
	// tide on PATH, else the `tide install` default (~/.local/bin/tide). A
	// shell alias is invisible to ssh, and ~/.local/bin is often off the
	// non-interactive PATH, so this one command covers both without touching
	// the remote's shell config.
	cmd := exec.Command("ssh", "-T", dest, buildRemoteCmd(remoteBin, serveArgs))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("launching ssh: %w", err)
	}

	conn := protocol.NewConn(&pipeConn{
		r: stdout, w: stdin,
		closeFn: func() error { _ = stdin.Close(); return stdout.Close() },
	})
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, herr := conn.ClientHandshake(); herr != nil {
		// The handshake can time out with ssh still ALIVE (wedged remote, MOTD/
		// 2FA stall). Close the pipes and kill ssh so Wait() can't block forever.
		_ = conn.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return remoteDialError(dest, herr, &stderr, cmd.ProcessState)
	}
	_ = conn.SetDeadline(time.Time{})
	// Safety net for an early return below (e.g. term.MakeRaw fails) before the
	// in-loop teardown can reap ssh; Kill+Wait is idempotent with that teardown.
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()

	fmt.Printf("[tide] connected to %s — copy lands on this machine; Ctrl+Shift+E detaches\n", dest)

	// Name the terminal tab after the remote (the cue `ssh host` used to give),
	// pushing the title so the prior one returns on detach. Terminals without
	// the title stack ignore the push/pop and just keep tide's title until the
	// shell next sets it.
	os.Stdout.WriteString(setTitleSeq(dest))
	defer os.Stdout.WriteString(titlePop)

	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)

	// Tell serve our size, then run the shared client loop. The snapshot
	// arrives as the first render frame, so no initial paint is passed.
	_ = client.SendResize(conn, cols, rows)
	reason, serr := streamSession(conn, stdinFd, nil, winch,
		fmt.Sprintf("connection to %s closed — the session keeps running there; run 'tide -r %s' to reattach", dest, dest),
		func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	if serr != nil {
		return serr
	}
	if reason != "" {
		fmt.Printf("[tide] %s\n", reason)
	}
	return nil
}

// remoteDialError turns a failed remote handshake into an actionable message:
// a protocol mismatch, tide missing on the host's PATH, or a raw ssh failure.
func remoteDialError(dest string, err error, stderr *bytes.Buffer, st *os.ProcessState) error {
	var mm *protocol.MismatchError
	if errors.As(err, &mm) {
		return fmt.Errorf("%s runs tide protocol %d, but this tide speaks %d — update tide on %s to match, then reconnect",
			dest, mm.PeerProtocol, version.Protocol, dest)
	}
	msg := oneLine(strings.TrimSpace(stderr.String()))
	exited127 := st != nil && st.ExitCode() == 127
	if exited127 || strings.Contains(msg, "not found") || strings.Contains(msg, "No such file") {
		return fmt.Errorf("tide is not on %s's non-interactive PATH (a shell alias won't work over ssh) — "+
			"on %s run 'tide install' to symlink it onto PATH, then retry"+stderrSuffix(msg), dest, dest)
	}
	if msg != "" {
		return fmt.Errorf("could not start tide on %s: %s", dest, msg)
	}
	return fmt.Errorf("could not connect to tide on %s: %w", dest, err)
}

func oneLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

func stderrSuffix(msg string) string {
	if msg == "" {
		return ""
	}
	return " (ssh: " + msg + ")"
}
