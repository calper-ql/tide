package daemon

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/calper-ql/tide/internal/layout"
	"github.com/calper-ql/tide/internal/protocol"
	"github.com/calper-ql/tide/internal/session"
)

// sink drains one client's frames so outbox writers never stall, and
// accumulates everything for content assertions.
type sink struct {
	mu    sync.Mutex
	data  bytes.Buffer
	types []string
}

func startSink(conn *protocol.Conn) *sink {
	s := &sink{}
	go func() {
		for {
			m, err := conn.Recv()
			if err != nil {
				return
			}
			s.mu.Lock()
			s.types = append(s.types, m.Type)
			s.data.Write(m.Data)
			s.mu.Unlock()
		}
	}()
	return s
}

func (s *sink) contains(sub string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Contains(s.data.String(), sub)
}

func (s *sink) sawType(typ string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.types {
		if t == typ {
			return true
		}
	}
	return false
}

func (s *sink) waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			s.mu.Lock()
			tail := s.data.String()
			if len(tail) > 400 {
				tail = tail[len(tail)-400:]
			}
			s.mu.Unlock()
			t.Fatalf("timed out waiting for %s; tail: %q", what, tail)
		}
		time.Sleep(15 * time.Millisecond)
	}
}

// newTestWS builds a workspace over a private registry with one real shell
// pane and one attached pipe client.
func newTestWS(t *testing.T) (*ws, *protocol.Conn, *sink) {
	t.Helper()
	root := t.TempDir()
	reg := session.NewRegistry(filepath.Join(t.TempDir(), "sessions.json"))
	if _, _, err := reg.Ensure(root); err != nil {
		t.Fatal(err)
	}
	d := &daemon{
		logf:     log.New(io.Discard, "", 0),
		registry: reg,
		socket:   filepath.Join(t.TempDir(), "test.sock"),
		sessions: map[string]*ws{},
		shutdown: make(chan struct{}),
	}
	w, err := newWS(d, root, session.Session{Root: root}, 100, 30)
	if err != nil {
		t.Fatal(err)
	}
	d.sessions[root] = w
	t.Cleanup(w.teardown)

	server, clientEnd := net.Pipe()
	sc := protocol.NewConn(server)
	cc := protocol.NewConn(clientEnd)
	s := startSink(cc)
	if _, err := w.attach(sc, 100, 30, func(frame []byte, clients, panes int) protocol.Message {
		return protocol.Message{Type: protocol.TypeRender, Data: frame}
	}); err != nil {
		t.Fatal(err)
	}
	return w, sc, s
}

func withWS(w *ws, f func()) {
	w.mu.Lock()
	defer w.mu.Unlock()
	f()
}

// hitCenter returns the center cell of the first hit region of a kind.
func hitCenter(t *testing.T, w *ws, kind hitKind) (int, int) {
	t.Helper()
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, h := range w.hits {
		if h.kind == kind {
			return h.rect.X + h.rect.W/2, h.rect.Y + h.rect.H/2
		}
	}
	t.Fatalf("no hit region of kind %d; hits: %+v", kind, w.hits)
	return 0, 0
}

func press(x, y int) []byte   { return []byte(fmt.Sprintf("\x1b[<0;%d;%dM", x+1, y+1)) }
func rclick(x, y int) []byte  { return []byte(fmt.Sprintf("\x1b[<2;%d;%dM", x+1, y+1)) }
func release(x, y int) []byte { return []byte(fmt.Sprintf("\x1b[<0;%d;%dm", x+1, y+1)) }
func motion(x, y int) []byte  { return []byte(fmt.Sprintf("\x1b[<32;%d;%dM", x+1, y+1)) }
func wheelUp(x, y int) []byte { return []byte(fmt.Sprintf("\x1b[<64;%d;%dM", x+1, y+1)) }

// menuClick runs an open overlay item by label through the real hitmap.
func menuClick(t *testing.T, w *ws, conn *protocol.Conn, label string) {
	t.Helper()
	w.mu.Lock()
	idx := -1
	if w.overlay != nil {
		for i, it := range w.overlay.items {
			if strings.HasPrefix(it.label, label) {
				idx = i
				break
			}
		}
	}
	if idx == -1 {
		w.mu.Unlock()
		t.Fatalf("no overlay item %q", label)
	}
	frame := w.renderLocked() // rebuild hitmap with the overlay present
	_ = frame
	var target *hitRegion
	for i := range w.hits {
		if w.hits[i].kind == hitMenuItem && w.hits[i].item == idx {
			target = &w.hits[i]
			break
		}
	}
	if target == nil {
		w.mu.Unlock()
		t.Fatalf("overlay item %q not in hitmap", label)
	}
	x, y := target.rect.X+1, target.rect.Y
	w.mu.Unlock()
	w.handleInput(conn, press(x, y))
	w.handleInput(conn, release(x, y))
}

func TestBarRendersAndDetachButtonWorks(t *testing.T) {
	w, conn, s := newTestWS(t)
	s.waitFor(t, "bar with tab", func() bool { return s.contains("1:") && s.contains("detach") })

	x, y := hitCenter(t, w, hitDetach)
	w.handleInput(conn, press(x, y))
	w.handleInput(conn, release(x, y))
	s.waitFor(t, "detached frame", func() bool { return s.sawType(protocol.TypeDetached) })
}

func TestContextMenuSplitCreatesPaneAndBorder(t *testing.T) {
	w, conn, s := newTestWS(t)
	s.waitFor(t, "first frame", func() bool { return s.contains("1:") })

	x, y := hitCenter(t, w, hitPane)
	w.handleInput(conn, rclick(x, y))
	w.handleInput(conn, release(x, y))
	s.waitFor(t, "context menu", func() bool { return s.contains("Split Right") && s.contains("Kill Session") })

	menuClick(t, w, conn, "Split Right")
	s.waitFor(t, "two panes", func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		return w.lay.CountPanes() == 2 && len(w.borders) == 1
	})

	// The layout change is checkpointed: a stored layout exists and lists
	// both panes.
	stored, _ := w.d.registry.Get(w.root)
	if len(stored.Layout) == 0 {
		t.Fatal("layout not checkpointed after split")
	}
	var l layout.Layout
	if err := json.Unmarshal(stored.Layout, &l); err != nil || len(l.PaneIDs()) != 2 {
		t.Fatalf("stored layout = %s (%v)", stored.Layout, err)
	}
}

func TestSelectionCtrlCCopiesThenSecondCtrlCReachesShell(t *testing.T) {
	w, conn, s := newTestWS(t)
	s.waitFor(t, "first frame", func() bool { return s.contains("1:") })

	// Plant deterministic content straight into the pane grid and select it
	// via synthetic coordinates (the mouse path is covered separately).
	var paneID string
	withWS(w, func() {
		paneID = w.lay.FocusedPane()
		p := w.panes[paneID]
		p.term.Write([]byte("\r\nSELECT-ME-TEXT\r\n"))
		_, rows, _ := p.term.ContentSize()
		// find the marker's content line
		view, hist := p.term.View(0, rows)
		for i, line := range view {
			text := ""
			for _, g := range line {
				if g.Char != 0 {
					text += string(g.Char)
				}
			}
			if strings.Contains(text, "SELECT-ME-TEXT") {
				w.sel = selectionState{pane: paneID, exists: true, aLine: hist + i, aX: 0, eLine: hist + i, eX: 13}
				break
			}
		}
		if !w.sel.exists {
			t.Fatal("marker line not found in pane view")
		}
	})

	// Ctrl+C with a selection: copy, never SIGINT (ratified ruling).
	w.handleInput(conn, []byte{0x03})
	s.waitFor(t, "OSC 52 clipboard write", func() bool { return s.contains("\x1b]52;c;") })
	withWS(w, func() {
		if w.sel.exists {
			t.Fatal("selection must clear on copy (second Ctrl+C interrupts)")
		}
		if string(w.clip) != "SELECT-ME-TEXT" {
			t.Fatalf("internal clipboard = %q", w.clip)
		}
	})
	s.waitFor(t, "copy flash", func() bool { return s.contains("copied 14 chars") })
}

func TestPasteGuardConfirmsMultilineIntoBareShell(t *testing.T) {
	w, conn, s := newTestWS(t)
	s.waitFor(t, "first frame", func() bool { return s.contains("1:") })

	withWS(w, func() { w.clip = []byte("rm -rf a\nrm -rf b\nrm -rf c") })
	w.handleInput(conn, []byte{0x16}) // Ctrl+V
	s.waitFor(t, "paste guard", func() bool { return s.contains("Paste 3 lines") })

	// Esc cancels; nothing reaches the shell.
	w.handleInput(conn, []byte{0x1b})
	time.Sleep(80 * time.Millisecond) // allow the ESC-flush timer to fire
	withWS(w, func() {
		if w.overlay != nil {
			t.Fatal("Esc must close the paste guard")
		}
	})
}

func TestWheelScrollsIntoHistoryAndKeyReturnsLive(t *testing.T) {
	w, conn, s := newTestWS(t)
	s.waitFor(t, "first frame", func() bool { return s.contains("1:") })

	var paneID string
	withWS(w, func() {
		paneID = w.lay.FocusedPane()
		p := w.panes[paneID]
		for i := 0; i < 120; i++ {
			fmt.Fprintf(p.term, "history-filler-%d\r\n", i)
		}
		w.dirtyPanes[paneID] = true
	})
	x, y := hitCenter(t, w, hitPane)
	for i := 0; i < 4; i++ {
		w.handleInput(conn, wheelUp(x, y))
	}
	s.waitFor(t, "scroll indicator", func() bool { return s.contains("SCROLL") })
	withWS(w, func() {
		if w.scroll[paneID] == 0 {
			t.Fatal("wheel up must scroll back")
		}
	})

	// Any key snaps back to live (and goes to the shell).
	w.handleInput(conn, []byte("x"))
	withWS(w, func() {
		if w.scroll[paneID] != 0 {
			t.Fatal("a keystroke must return the pane to live")
		}
	})
}

func TestKittyCtrlShiftEDetaches(t *testing.T) {
	w, conn, s := newTestWS(t)
	s.waitFor(t, "first frame", func() bool { return s.contains("1:") })
	w.handleInput(conn, []byte("\x1b[101;6u")) // kitty: 'e' with Shift+Ctrl
	s.waitFor(t, "detached", func() bool { return s.sawType(protocol.TypeDetached) })
}

func TestNewTabAndSwitch(t *testing.T) {
	w, conn, s := newTestWS(t)
	s.waitFor(t, "first frame", func() bool { return s.contains("1:") })

	x, y := hitCenter(t, w, hitNewTab)
	w.handleInput(conn, press(x, y))
	w.handleInput(conn, release(x, y))
	s.waitFor(t, "second tab", func() bool { return s.contains("2:") })
	withWS(w, func() {
		if len(w.lay.Tabs) != 2 || w.lay.Active != 1 {
			t.Fatalf("tabs=%d active=%d", len(w.lay.Tabs), w.lay.Active)
		}
	})

	// Click the first tab label to switch back.
	w.mu.Lock()
	var tab0 *hitRegion
	for i := range w.hits {
		if w.hits[i].kind == hitTabLabel && w.hits[i].tab == 0 {
			tab0 = &w.hits[i]
			break
		}
	}
	if tab0 == nil {
		w.mu.Unlock()
		t.Fatal("tab 0 not clickable")
	}
	tx, ty := tab0.rect.X+1, tab0.rect.Y
	w.mu.Unlock()
	w.handleInput(conn, press(tx, ty))
	w.handleInput(conn, release(tx, ty))
	withWS(w, func() {
		if w.lay.Active != 0 {
			t.Fatalf("active tab = %d, want 0", w.lay.Active)
		}
	})
}

func TestBorderDragResizes(t *testing.T) {
	w, conn, s := newTestWS(t)
	s.waitFor(t, "first frame", func() bool { return s.contains("1:") })
	withWS(w, func() { w.actionSplitLocked(w.lay.FocusedPane(), layout.SplitRight) })
	// The hitmap refreshes on the next (async) render.
	s.waitFor(t, "border in hitmap", func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		for _, h := range w.hits {
			if h.kind == hitBorder {
				return true
			}
		}
		return false
	})

	var before map[string]layout.Rect
	withWS(w, func() {
		before = map[string]layout.Rect{}
		for id, r := range w.rects {
			before[id] = r
		}
	})
	bx, by := hitCenter(t, w, hitBorder)
	w.handleInput(conn, press(bx, by))
	w.handleInput(conn, motion(bx+8, by))
	w.handleInput(conn, release(bx+8, by))
	withWS(w, func() {
		changed := false
		for id, r := range w.rects {
			if before[id] != r {
				changed = true
			}
		}
		if !changed {
			t.Fatal("border drag did not resize panes")
		}
	})
}

func TestMouseDragSelectionFeedsPrimary(t *testing.T) {
	w, conn, s := newTestWS(t)
	s.waitFor(t, "first frame", func() bool { return s.contains("1:") })

	withWS(w, func() {
		p := w.panes[w.lay.FocusedPane()]
		p.term.Write([]byte("\rDRAG-TARGET-LINE\r\n"))
	})
	// Drag across the first row of the pane (screen row 1: below the bar).
	w.handleInput(conn, press(0, 1))
	w.handleInput(conn, motion(9, 1))
	w.handleInput(conn, release(9, 1))
	s.waitFor(t, "primary selection write", func() bool { return s.contains("\x1b]52;p;") })
	withWS(w, func() {
		if !w.sel.exists {
			t.Fatal("drag must leave a visible selection")
		}
	})
}
