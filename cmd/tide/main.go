// Command tide attaches to (or creates) the session for the current
// project; the daemon spawns on demand (spec: invocation).
package main

import (
	"bufio"
	"bytes"
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
  tide --here      use the current directory as the project root verbatim
  tide ls          list live sessions
  tide kill [path] [--here]
                   end the project's session (the only way a session ends)
  tide restart     shut the daemon down and start fresh (version upgrades)
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

// resetSequences undoes terminal state the snapshot or the pane stream may
// have set on the user's real terminal: alt screen, mouse reporting,
// bracketed paste, app cursor/keypad, scroll region, origin, reverse video,
// keyboard lock, insert mode, linefeed mode, meta-8bit, cursor shape,
// palette and default-color overrides, charset, hidden cursor. (SRM [12l is
// deliberately absent: its polarity is inverted and resetting it would
// enable local echo.)
const resetSequences = "\x1b[0m\x1b[?1049l\x1b[?1l\x1b>\x1b[?9l\x1b[?1000l" +
	"\x1b[?1002l\x1b[?1003l\x1b[?1006l\x1b[?1004l\x1b[?2004l\x1b[?7h\x1b[r" +
	"\x1b[?5l\x1b[?6l\x1b[?1034l\x1b[2l\x1b[4l\x1b[20l\x1b[ q" +
	"\x1b]104\a\x1b]110\a\x1b]111\a\x1b(B\x1b[?25h"

// detachKey is the v0 placeholder detach chord (Ctrl+\). The session bar
// and Ctrl+Shift+E replace it when the chrome lands; until then this is the
// one key the client steals from the pane.
const detachKey = 0x1c

func attach(rt, target string, here bool) error {
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
	fmt.Printf("[tide] attached to %s (%s) — Ctrl+\\ detaches, session keeps running\n",
		info.Root, plural(info.Clients, "client"))

	oldState, err := term.MakeRaw(stdinFd)
	if err != nil {
		return err
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

	if _, err := os.Stdout.Write(snap); err != nil {
		return err
	}

	type result struct {
		reason string
		err    error
	}
	done := make(chan result, 2)

	// Keyboard → pane. Only the detach key is intercepted.
	go func() {
		buf := make([]byte, 4096)
		for {
			n, rerr := os.Stdin.Read(buf)
			if n > 0 {
				data := buf[:n]
				if i := bytes.IndexByte(data, detachKey); i >= 0 {
					if i > 0 {
						_ = client.SendInput(c, append([]byte(nil), data[:i]...))
					}
					done <- result{reason: "detached — session keeps running; run 'tide' here to reattach"}
					return
				}
				if serr := client.SendInput(c, append([]byte(nil), data...)); serr != nil {
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
				done <- result{err: errors.New("daemon connection closed — state is checkpointed; run 'tide' here to reattach")}
				return
			}
			switch m.Type {
			case protocol.TypeOutput:
				_, _ = os.Stdout.Write(m.Data)
			case protocol.TypeExit:
				done <- result{reason: fmt.Sprintf("shell exited (status %d) — run 'tide' here to start a fresh one", m.ExitStatus)}
				return
			case protocol.TypeKilled:
				done <- result{reason: "session ended by 'tide kill'"}
				return
			case protocol.TypeDropped:
				done <- result{reason: "detached by the daemon: " + m.Err + "; run 'tide' here to reattach"}
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
			fmt.Println("[tide] detached — session keeps running; run 'tide' here to reattach")
			return nil
		case r := <-done:
			teardown()
			if r.err != nil {
				return r.err
			}
			fmt.Printf("[tide] %s\n", r.reason)
			return nil
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
		fmt.Printf("%s\t%s\tsince %s\n",
			s.Root, plural(s.Clients, "client"), s.CreatedAt.Local().Format(time.DateTime))
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
