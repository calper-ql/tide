// Command tide attaches to (or creates) the session for the current
// project; the daemon spawns on demand (spec: invocation).
package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/calper-ql/tide/internal/client"
	"github.com/calper-ql/tide/internal/daemon"
	"github.com/calper-ql/tide/internal/paths"
	"github.com/calper-ql/tide/internal/project"
	"github.com/calper-ql/tide/internal/protocol"
	"github.com/calper-ql/tide/internal/version"
)

const usage = `tide ` + version.Binary + ` — terminal IDE

usage:
  tide [path]      attach to the project's session, creating it on demand
  tide -r user@host [path]
                   attach a session on a remote host over ssh; the client
                   runs here, so copy lands on this machine's clipboard
  tide --here      use the current directory as the project root verbatim
  tide ls          list live sessions
  tide kill [path] [--here]
                   end the project's session (the only way a session ends)
  tide restart     shut the daemon down and start fresh (version upgrades)
  tide install [dir]
                   symlink this binary onto PATH (default ~/.local/bin) so a
                   non-interactive ssh shell can find it for 'tide -r'
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "tide:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	rt, err := paths.RuntimeDir()
	if err != nil {
		return err
	}

	if len(args) == 0 {
		return attach(rt, "", false)
	}
	switch args[0] {
	case "--daemon":
		return runDaemon(rt)
	case "--serve":
		// Remote bridge: invoked over ssh by `tide -r` on another machine.
		// Runs on THIS host, bridging the host daemon to stdio; the real
		// interactive client is the remote caller's, so copy lands there.
		return serve(rt, args[1:])
	case "-r":
		// Attach a remote machine's session from here; the client runs
		// locally so copy lands on THIS machine's clipboard.
		return remoteAttach(args[1:])
	case "install":
		// Symlink this binary onto PATH so a non-interactive ssh shell (which
		// `tide -r` uses) can find it — aliases don't work there.
		return install(args[1:])
	case "--here":
		return attach(rt, "", true)
	case "ls":
		return ls(rt)
	case "kill":
		target, here := "", false
		for _, a := range args[1:] {
			if a == "--here" {
				here = true
			} else {
				target = a
			}
		}
		return kill(rt, target, here)
	case "restart":
		return restart(rt)
	case "help", "-h", "--help":
		fmt.Print(usage)
		return nil
	default:
		if strings.HasPrefix(args[0], "-") {
			fmt.Fprint(os.Stderr, usage)
			return fmt.Errorf("unknown flag %q", args[0])
		}
		return attach(rt, args[0], false)
	}
}

func runDaemon(rt string) error {
	statePath, err := paths.StatePath()
	if err != nil {
		return err
	}
	logFile, err := os.OpenFile(paths.LogPath(rt), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer logFile.Close()
	err = daemon.Run(daemon.Options{RuntimeDir: rt, StatePath: statePath, Log: logFile})
	if err != nil {
		// The detached daemon's stderr is /dev/null; the log file is the
		// only place a startup failure can surface.
		log.New(logFile, "", log.LstdFlags).Printf("daemon failed: %v", err)
	}
	return err
}

// resolveRoot maps an invocation to a session identity. --here skips the
// .git walk (spec: root resolution override).
func resolveRoot(target string, here bool) (root string, foundRepo bool, err error) {
	dir := target
	if dir == "" {
		dir, err = os.Getwd()
		if err != nil {
			return "", false, err
		}
	}
	if here {
		root, err = project.Canonical(dir)
		return root, true, err
	}
	return project.Resolve(dir)
}

// enterSequences put the client terminal into tide mode: alt screen (the
// user's shell screen is restored untouched on detach), SGR mouse with
// button-drag tracking, bracketed paste, focus reporting, and the kitty
// keyboard protocol pushed for chord disambiguation (Ctrl+Shift+E) —
// terminals without it ignore the push and the daemon's decoder handles
// the legacy encoding.
// 1002 (drag tracking) then 1003 (any-motion, for hover highlights):
// terminals without 1003 keep 1002 and simply send no hover events —
// the chrome degrades to no highlight, never to broken clicks.
const enterSequences = "\x1b[?1049h\x1b[?1002h\x1b[?1003h\x1b[?1006h\x1b[?2004h\x1b[?1004h\x1b[>1u\x1b[2J"

// resetSequences undo everything enterSequences set (in reverse: kitty
// pop first, alt-screen leave last so the user's screen comes back clean)
// plus anything the composed render stream may have left (SGR, cursor).
const resetSequences = "\x1b[<u\x1b[?1004l\x1b[?2004l\x1b[?1006l\x1b[?1003l\x1b[?1002l" +
	"\x1b[0m\x1b[?25h\x1b[?1049l"

// errNested is returned when tide is launched from inside one of its own
// panes ($TIDE_SESSION is set there). Attaching would stack a second
// alt-screen + mouse/keyboard regime inside a pane — the tmux-in-tmux trap
// — so attach refuses it. ls/kill/restart from within a pane are fine; they
// never attach.
var errNested = errors.New("already inside a tide session — refusing to nest tide in itself\n" +
	"  • detach first: Ctrl+Shift+E (or the bar's '-')\n" +
	"  • nest anyway: unset TIDE_SESSION")

func attach(rt, target string, here bool) error {
	if os.Getenv("TIDE_SESSION") != "" {
		return errNested
	}
	root, foundRepo, err := resolveRoot(target, here)
	if err != nil {
		return err
	}
	stdinFd := int(os.Stdin.Fd())
	if !term.IsTerminal(stdinFd) {
		return errors.New("attach requires a terminal (stdin is not a tty)")
	}
	if !term.IsTerminal(int(os.Stdout.Fd())) {
		// Otherwise the pane stream lands in a file/pipe while the real
		// terminal sits raw and blank, blindly executing keystrokes.
		return errors.New("attach requires a terminal (stdout is not a tty)")
	}

	// Register for resizes before the first size read: daemon spawn can
	// take a moment, and a resize during it must not be lost.
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	cols, rows, err := term.GetSize(stdinFd)
	if err != nil {
		return err
	}

	c, err := client.EnsureDaemon(rt)
	if err != nil {
		return err
	}
	defer c.Close()
	info, snap, err := client.Attach(c, root, cols, rows)
	if err != nil {
		return err
	}
	// Self-heal a resize that landed between the size read and the attach.
	if w, h, gerr := term.GetSize(stdinFd); gerr == nil && (w != cols || h != rows) {
		_ = client.SendResize(c, w, h)
	}
	if !foundRepo {
		fmt.Printf("[tide] no git repository found — project root is %s\n", root)
	}
	fmt.Printf("[tide] attached to %s (%s) — '-' in the bar or Ctrl+Shift+E detaches\n",
		info.Root, plural(info.Clients, "client"))

	reason, serr := streamSession(c, stdinFd, snap, winch,
		"connection to the daemon closed — run 'tide ls' to check the session", nil)
	if serr != nil {
		return serr
	}
	if reason != "" {
		fmt.Printf("[tide] %s\n", reason)
	}
	return nil
}

// streamSession runs the raw-terminal render/input loop against an attached
// connection until detach, kill, or an unexpected close. snap (may be nil) is
// the initial paint written before the loop; for a remote attach it is nil
// because the first render frame paints. connLostMsg is the error surfaced on
// an unexpected recv failure; extraTeardown (may be nil) runs during teardown,
// e.g. to reap an ssh child. Shared by local attach() and remote
// remoteAttach() — the only difference between the two is how the connection
// is obtained, never how it is driven.
func streamSession(c *protocol.Conn, stdinFd int, snap []byte, winch <-chan os.Signal, connLostMsg string, extraTeardown func()) (string, error) {
	oldState, err := term.MakeRaw(stdinFd)
	if err != nil {
		return "", err
	}
	var restoreOnce sync.Once
	restore := func() {
		restoreOnce.Do(func() {
			os.Stdout.WriteString(resetSequences)
			_ = term.Restore(stdinFd, oldState)
			fmt.Println()
		})
	}
	defer restore()
	os.Stdout.WriteString(enterSequences)
	if len(snap) > 0 {
		if _, err := os.Stdout.Write(snap); err != nil {
			return "", err
		}
	}

	type result struct {
		reason string
		err    error
	}
	done := make(chan result, 2)

	// Keyboard/mouse → daemon, raw. The daemon's router owns the keymap;
	// the client intercepts nothing (spec: one control scheme).
	go func() {
		buf := make([]byte, 4096)
		for {
			n, rerr := os.Stdin.Read(buf)
			if n > 0 {
				if serr := client.SendInput(c, append([]byte(nil), buf[:n]...)); serr != nil {
					done <- result{err: serr}
					return
				}
			}
			if rerr != nil {
				done <- result{reason: "stdin closed — detached; session keeps running"}
				return
			}
		}
	}()

	// Pane → screen. outputDone lets teardown wait until this goroutine can
	// no longer write: a frame landing on the terminal after the reset
	// sequences would re-corrupt it.
	outputDone := make(chan struct{})
	go func() {
		defer close(outputDone)
		for {
			m, rerr := c.Recv()
			if rerr != nil {
				done <- result{err: errors.New(connLostMsg)}
				return
			}
			switch m.Type {
			case protocol.TypeRender:
				_, _ = os.Stdout.Write(m.Data)
			case protocol.TypeCopy:
				// Off the render goroutine: a wedged clipboard tool must not
				// stall frame delivery. On a remote attach this runs on the
				// LAPTOP, so copy lands on the laptop's clipboard.
				go writeNativeClipboard(m.Target, m.Data)
			case protocol.TypeDetached:
				done <- result{reason: "detached — session keeps running; reattach to resume"}
				return
			case protocol.TypeKilled:
				done <- result{reason: "session ended"}
				return
			case protocol.TypeDropped:
				done <- result{reason: "detached by the daemon: " + m.Err}
				return
			}
		}
	}()

	hangup := make(chan os.Signal, 1)
	signal.Notify(hangup, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(hangup)

	// teardown stops the output stream before touching the terminal.
	teardown := func() {
		c.Close()
		<-outputDone
		if extraTeardown != nil {
			extraTeardown()
		}
		restore()
	}
	for {
		select {
		case <-winch:
			if w, h, gerr := term.GetSize(stdinFd); gerr == nil {
				_ = client.SendResize(c, w, h)
			}
		case <-hangup:
			// Killing the terminal is a valid detach (spec: invocation).
			teardown()
			return "detached — session keeps running; reattach to resume", nil
		case r := <-done:
			teardown()
			return r.reason, r.err
		}
	}
}

func ls(rt string) error {
	c, err := dialNoSpawn(rt)
	if err != nil {
		return err
	}
	if c == nil {
		fmt.Println("[tide] no live sessions (daemon not running)")
		return nil
	}
	defer c.Close()
	sessions, err := client.Ls(c)
	if err != nil {
		return err
	}
	if len(sessions) == 0 {
		fmt.Println("[tide] no live sessions")
		return nil
	}
	for _, s := range sessions {
		fmt.Printf("%s\t%s\t%s\tsince %s\n",
			s.Root, plural(s.Panes, "pane"), plural(s.Clients, "client"),
			s.CreatedAt.Local().Format(time.DateTime))
	}
	return nil
}

// killCandidates lists the session roots a kill invocation may mean, most
// specific first: the exact path (so sessions created with --here are
// reachable), then the .git-walk root. The path itself may name a session
// for a since-deleted directory, so it is computed without stat.
func killCandidates(dir string, here bool) []string {
	var out []string
	add := func(p string) {
		for _, q := range out {
			if q == p {
				return
			}
		}
		out = append(out, p)
	}
	if abs, err := filepath.Abs(dir); err == nil {
		add(filepath.Clean(abs))
	}
	if canon, err := project.Canonical(dir); err == nil {
		add(canon)
	}
	if !here {
		if root, _, err := project.Resolve(dir); err == nil {
			add(root)
		}
	}
	return out
}

// pickKillTarget returns the first candidate with a live session.
func pickKillTarget(sessions []protocol.SessionInfo, candidates []string) string {
	for _, cand := range candidates {
		for _, s := range sessions {
			if s.Root == cand {
				return cand
			}
		}
	}
	return ""
}

func kill(rt, target string, here bool) error {
	dir := target
	if dir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return err
		}
		dir = wd
	}
	candidates := killCandidates(dir, here)
	if len(candidates) == 0 {
		return fmt.Errorf("cannot resolve %s", dir)
	}
	c, err := dialNoSpawn(rt)
	if err != nil {
		return err
	}
	if c == nil {
		return fmt.Errorf("no session for %s (daemon not running)", candidates[0])
	}
	defer c.Close()
	sessions, err := client.Ls(c)
	if err != nil {
		return err
	}
	root := pickKillTarget(sessions, candidates)
	if root == "" {
		return fmt.Errorf("no session for %s", strings.Join(candidates, " or "))
	}
	if err := client.Kill(c, root); err != nil {
		return err
	}
	fmt.Printf("[tide] session %s ended\n", root)
	return nil
}

func restart(rt string) error {
	c, err := client.Dial(rt)
	switch {
	case err == nil:
		defer c.Close()
		sessions, err := client.Ls(c)
		if err != nil {
			return err
		}
		warnRestart(sessions)
		ok, err := confirm("Proceed?")
		if err != nil || !ok {
			return err
		}
		if err := client.Shutdown(c); err != nil {
			return err
		}

	case errors.As(err, new(*protocol.MismatchError)):
		// The running daemon speaks another protocol, so we can't even ask
		// it for a session list — SIGTERM via the pid in the lock file is
		// the version-independent shutdown path.
		fmt.Println("[tide] the running daemon speaks a different protocol version")
		fmt.Println("[tide] restarting will shut down all its sessions")
		ok, err := confirm("Proceed?")
		if err != nil || !ok {
			return err
		}
		if err := signalDaemon(rt); err != nil {
			return err
		}

	default:
		fmt.Println("[tide] daemon not running — starting fresh")
	}

	if err := waitDown(rt); err != nil {
		return err
	}
	nc, err := client.EnsureDaemon(rt)
	if err != nil {
		return err
	}
	nc.Close()
	fmt.Printf("[tide] daemon running (%s, protocol %d)\n", version.Binary, version.Protocol)
	return nil
}

func warnRestart(sessions []protocol.SessionInfo) {
	if len(sessions) == 0 {
		fmt.Println("[tide] no live sessions; the daemon will restart")
		return
	}
	fmt.Printf("[tide] restarting will shut down %s:\n", plural(len(sessions), "session"))
	for _, s := range sessions {
		fmt.Printf("  %s (%s)\n", s.Root, plural(s.Clients, "client"))
	}
}

func confirm(prompt string) (bool, error) {
	fmt.Printf("%s [y/N] ", prompt)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	if answer != "y" && answer != "yes" {
		fmt.Println("[tide] aborted")
		return false, nil
	}
	return true, nil
}

// signalDaemon sends SIGTERM to the pid recorded in the lock file. The
// flock probe immediately before the kill is the staleness guard: the
// confirmation prompt leaves unbounded human time during which the daemon
// can exit and the OS recycle its pid onto an innocent process. A free
// lock means there is no daemon left to signal.
func signalDaemon(rt string) error {
	f, err := os.OpenFile(paths.LockPath(rt), os.O_RDWR, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return nil // no lock file, no daemon
	}
	if err != nil {
		return err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
		return nil // lock is free: the daemon is already gone
	} else if !errors.Is(err, syscall.EWOULDBLOCK) {
		return err
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return fmt.Errorf("cannot read the daemon's pid: %w", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return fmt.Errorf("lock file %s holds no valid pid", paths.LockPath(rt))
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

// waitDown waits for the old daemon to stop accepting connections.
func waitDown(rt string) error {
	deadline := time.Now().Add(5 * time.Second)
	for {
		c, err := client.Dial(rt)
		if err != nil {
			var mm *protocol.MismatchError
			if !errors.As(err, &mm) {
				return nil // nobody listening anymore
			}
		} else {
			c.Close()
		}
		if time.Now().After(deadline) {
			return errors.New("old daemon did not shut down")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// dialNoSpawn returns (nil, nil) when no daemon is running; a protocol
// mismatch is still surfaced so the user learns to run 'tide restart'.
func dialNoSpawn(rt string) (*protocol.Conn, error) {
	c, err := client.Dial(rt)
	if err == nil {
		return c, nil
	}
	if errors.As(err, new(*protocol.MismatchError)) {
		return nil, err
	}
	return nil, nil
}

func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
