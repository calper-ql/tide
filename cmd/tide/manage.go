// `tide manage` is an interactive session manager: list live sessions and kill
// them, each behind an explicit confirmation. It runs locally against this
// machine's daemon; `tide -r host manage` runs the same UI over the ssh bridge
// (serveManage) against the host's daemon. Both drive internal/manage.Model
// and execute confirmed kills via client.Kill.
package main

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"sync"
	"syscall"

	"golang.org/x/term"

	"github.com/calper-ql/tide/internal/client"
	"github.com/calper-ql/tide/internal/input"
	"github.com/calper-ql/tide/internal/manage"
	"github.com/calper-ql/tide/internal/protocol"
)

// manageSessions lists the daemon's sessions for the manager, sorted by root.
func manageSessions(c *protocol.Conn) []manage.Session {
	if c == nil {
		return nil
	}
	infos, err := client.Ls(c)
	if err != nil {
		return nil
	}
	out := make([]manage.Session, 0, len(infos))
	for _, s := range infos {
		out = append(out, manage.Session{Root: s.Root, Panes: s.Panes, Clients: s.Clients})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Root < out[j].Root })
	return out
}

// applyManage feeds decoded events to the model and executes a confirmed kill
// against the daemon, refreshing the list. Shared by the local and remote
// drivers. Reports whether to repaint and whether to quit.
func applyManage(m *manage.Model, c *protocol.Conn, events []input.Event) (dirty, quit bool) {
	for _, ev := range events {
		if m.Handle(ev) {
			dirty = true
		}
	}
	if root, ok := m.TakeKill(); ok {
		switch {
		case c == nil:
			m.Flash("no daemon")
		case client.Kill(c, root) != nil:
			m.Flash("kill failed")
		default:
			m.SetSessions(manageSessions(c))
			m.Flash("killed " + filepath.Base(root))
		}
		dirty = true
	}
	return dirty, m.Quit()
}

// manageCmd is the local driver: a raw-terminal loop against this machine's daemon.
func manageCmd(rt string) error {
	stdinFd := int(os.Stdin.Fd())
	if !term.IsTerminal(stdinFd) || !term.IsTerminal(int(os.Stdout.Fd())) {
		return errors.New("tide manage requires a terminal")
	}
	c, err := client.Dial(rt) // no-spawn: manage only operates on a running daemon
	if err != nil {
		if errors.As(err, new(*protocol.MismatchError)) {
			return err
		}
		fmt.Println("[tide] no live sessions (daemon not running)")
		return nil
	}
	defer c.Close()
	sessions := manageSessions(c)
	if len(sessions) == 0 {
		fmt.Println("[tide] no live sessions")
		return nil
	}
	cols, rows, err := term.GetSize(stdinFd)
	if err != nil {
		return err
	}
	m := manage.New(sessions, cols, rows)

	oldState, err := term.MakeRaw(stdinFd)
	if err != nil {
		return err
	}
	var once sync.Once
	restore := func() {
		once.Do(func() {
			os.Stdout.WriteString(resetSequences)
			_ = term.Restore(stdinFd, oldState)
			fmt.Println()
		})
	}
	defer restore()
	os.Stdout.WriteString(enterSequences)
	os.Stdout.Write(m.Render())

	in := make(chan []byte, 8)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, e := os.Stdin.Read(buf)
			if n > 0 {
				in <- append([]byte(nil), buf[:n]...)
			}
			if e != nil {
				close(in)
				return
			}
		}
	}()
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)

	dec := input.NewDecoder()
	for {
		select {
		case data, ok := <-in:
			if !ok {
				return nil
			}
			dirty, quit := applyManage(m, c, dec.Feed(data))
			if quit {
				return nil
			}
			if dirty {
				os.Stdout.Write(m.Render())
			}
		case <-winch:
			if w, h, e := term.GetSize(stdinFd); e == nil {
				m.Resize(w, h)
				os.Stdout.Write(m.Render())
			}
		}
	}
}

// serveManage is the host side of `tide -r host manage`: it runs the manager
// over the ssh stdio bridge against the host's daemon, while the caller's
// client renders the frames and ships input.
func serveManage(rt string) error {
	conn := protocol.NewConn(&pipeConn{r: os.Stdin, w: os.Stdout})
	if _, err := conn.ServerHandshake(); err != nil {
		return fmt.Errorf("client handshake: %w", err)
	}
	cols, rows, _ := readInitialSize(conn)

	c, err := client.Dial(rt) // no-spawn
	if err != nil {
		if errors.As(err, new(*protocol.MismatchError)) {
			return err
		}
		c = nil // no daemon: an empty manager (shows "no live sessions")
	}
	if c != nil {
		defer c.Close()
	}
	m := manage.New(manageSessions(c), cols, rows)
	if err := conn.Send(protocol.Message{Type: protocol.TypeRender, Data: m.Render()}); err != nil {
		return err
	}
	dec := input.NewDecoder()
	for {
		msg, rerr := conn.Recv()
		if rerr != nil {
			return rerr
		}
		dirty := false
		switch msg.Type {
		case protocol.TypeResize:
			if msg.Cols > 0 && msg.Rows > 0 {
				m.Resize(msg.Cols, msg.Rows)
				dirty = true
			}
		case protocol.TypeInput:
			d, quit := applyManage(m, c, dec.Feed(msg.Data))
			if quit {
				return nil
			}
			dirty = dirty || d
		}
		if dirty {
			if err := conn.Send(protocol.Message{Type: protocol.TypeRender, Data: m.Render()}); err != nil {
				return err
			}
		}
	}
}
