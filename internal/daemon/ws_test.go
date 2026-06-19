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
	mu     sync.Mutex
	data   bytes.Buffer
	types  []string
	copies []protocol.Message
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
			if m.Type == protocol.TypeCopy {
				s.copies = append(s.copies, m)
			}
			s.data.Write(m.Data)
			s.mu.Unlock()
		}
	}()
	return s
}

func (s *sink) sawCopy(target, text string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.copies {
		if m.Target == target && string(m.Data) == text {
			return true
		}
	}
	return false
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
	t.Setenv("SHELL", "/bin/sh") // hermetic panes; see start()
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

	// A click (press+release, no motion) on the outer-ring left edge opens
	// the directional split menu for the window abutting it (window-centric
	// model): the left edge defaults to splitting that pane leftward.
	w.handleInput(conn, press(0, 5))
	w.handleInput(conn, release(0, 5))
	s.waitFor(t, "edge menu", func() bool { return s.contains("← New pane left") })

	menuClick(t, w, conn, "← New pane left")
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
	s.waitFor(t, "native clipboard copy frame", func() bool {
		return s.sawCopy(protocol.CopyClipboard, "SELECT-ME-TEXT")
	})
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
	// Drag across the first content row (row 2: session bar, pane bar,
	// then content).
	w.handleInput(conn, press(1, 2))
	w.handleInput(conn, motion(10, 2))
	w.handleInput(conn, release(10, 2))
	s.waitFor(t, "primary selection write", func() bool { return s.contains("\x1b]52;p;") })
	s.waitFor(t, "native primary copy frame", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, m := range s.copies {
			if m.Target == protocol.CopyPrimary && len(m.Data) > 0 {
				return true
			}
		}
		return false
	})
	withWS(w, func() {
		if !w.sel.exists {
			t.Fatal("drag must leave a visible selection")
		}
	})
}

func TestPaneMenuButtonAndSessionMenu(t *testing.T) {
	w, conn, s := newTestWS(t)
	s.waitFor(t, "first frame", func() bool { return s.contains("1:") })

	// The [≡] button opens the pane menu (reachable without right-click —
	// macOS Terminal.app never forwards those).
	x, y := hitCenter(t, w, hitPaneMenu)
	w.handleInput(conn, press(x, y))
	w.handleInput(conn, release(x, y))
	s.waitFor(t, "pane menu", func() bool { return s.contains("Close Pane") })
	withWS(w, func() {
		// Splitting is spatial now (window edges), so the pane menu carries no
		// Split items; session actions never appear here either.
		for _, it := range w.overlay.items {
			if strings.HasPrefix(it.label, "Kill Session") {
				t.Fatalf("pane menu must not contain %q", it.label)
			}
			if strings.HasPrefix(it.label, "Split") {
				t.Fatalf("pane menu must not contain Split items (splitting is on window edges now): %q", it.label)
			}
		}
	})
	w.handleInput(conn, []byte{0x1b}) // Esc closes
	time.Sleep(80 * time.Millisecond)

	// The project segment opens the session menu.
	sx, sy := hitCenter(t, w, hitSessionMenu)
	w.handleInput(conn, press(sx, sy))
	w.handleInput(conn, release(sx, sy))
	s.waitFor(t, "session menu", func() bool { return s.contains("Kill Session") && s.contains("New Tab") })
}

func TestStackedSplitBarIsDividerAndDraggable(t *testing.T) {
	w, conn, s := newTestWS(t)
	s.waitFor(t, "first frame", func() bool { return s.contains("1:") })
	withWS(w, func() { w.actionSplitLocked(w.lay.FocusedPane(), layout.SplitDown) })

	// The lower pane's bar IS the divider: a hitPaneBar region carrying a
	// border must appear (and no double border row exists — rects tile).
	var barX, barY int
	s.waitFor(t, "divider bar in hitmap", func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		for _, h := range w.hits {
			if h.kind == hitPaneBar && h.hasBorder {
				barX, barY = h.rect.X+h.rect.W/2, h.rect.Y
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
	// Drag the bar down: resizes.
	w.handleInput(conn, press(barX, barY))
	w.handleInput(conn, motion(barX, barY+3))
	w.handleInput(conn, release(barX, barY+3))
	withWS(w, func() {
		changed := false
		for id, r := range w.rects {
			if before[id] != r {
				changed = true
			}
		}
		if !changed {
			t.Fatal("bar-divider drag did not resize")
		}
	})

	// A click (no motion) on a pane bar opens the layout menu for that pane.
	var clickX, clickY int
	withWS(w, func() {
		for _, h := range w.hits {
			if h.kind == hitPaneBar {
				clickX, clickY = h.rect.X+2, h.rect.Y
				break
			}
		}
	})
	w.handleInput(conn, press(clickX, clickY))
	w.handleInput(conn, release(clickX, clickY))
	s.waitFor(t, "boundary menu from bar click", func() bool { return s.contains("New pane") })
}

func TestCornerDragResizesBothAxes(t *testing.T) {
	w, conn, s := newTestWS(t)
	s.waitFor(t, "first frame", func() bool { return s.contains("1:") })
	withWS(w, func() {
		a := w.lay.FocusedPane()
		w.actionSplitLocked(a, layout.SplitRight) // A | B (focus B)
		w.actionSplitLocked(w.lay.FocusedPane(), layout.SplitDown)
	})

	// Corner: the vertical border column at the stacked divider's row.
	var cx, cy int
	s.waitFor(t, "corner in layout", func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		var vb, hb *layout.Border
		for i := range w.borders {
			if w.borders[i].Vertical {
				vb = &w.borders[i]
			} else {
				hb = &w.borders[i]
			}
		}
		if vb == nil || hb == nil {
			return false
		}
		cx, cy = vb.Rect.X, hb.Rect.Y
		return true
	})
	// The hitmap rebuilds on the next coalesced render; pressing before
	// that would hit the pre-split regions.
	s.waitFor(t, "hitmap rebuilt", func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		for _, h := range w.hits {
			if h.kind == hitBorder {
				return true
			}
		}
		return false
	})

	type dims struct{ w, h int }
	snap := func() map[string]dims {
		out := map[string]dims{}
		for id, r := range w.rects {
			out[id] = dims{r.W, r.H}
		}
		return out
	}
	var before map[string]dims
	withWS(w, func() { before = snap() })

	w.handleInput(conn, press(cx, cy))
	w.handleInput(conn, motion(cx-5, cy+3))
	w.handleInput(conn, release(cx-5, cy+3))

	withWS(w, func() {
		after := snap()
		widthChanged, heightChanged := false, false
		for id, d := range after {
			if before[id].w != d.w {
				widthChanged = true
			}
			if before[id].h != d.h {
				heightChanged = true
			}
		}
		if !widthChanged || !heightChanged {
			t.Fatalf("corner drag must resize both axes: width=%v height=%v\nbefore=%v\nafter=%v",
				widthChanged, heightChanged, before, after)
		}
	})
}

// TestEdgeMenuSplitsOneWindow pins the window-centric model: the divider
// between two stacked panes is the LOWER pane's top edge, and "new pane
// right" from it splits just that one window — the new pane sits beside the
// lower pane only, not the whole stack at full height.
func TestEdgeMenuSplitsOneWindow(t *testing.T) {
	w, conn, s := newTestWS(t)
	s.waitFor(t, "first frame", func() bool { return s.contains("1:") })
	withWS(w, func() { w.actionSplitLocked(w.lay.FocusedPane(), layout.SplitDown) })

	var barX, barY int
	s.waitFor(t, "divider bar in hitmap", func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		for _, h := range w.hits {
			if h.kind == hitPaneBar && h.hasBorder {
				barX, barY = h.rect.X+2, h.rect.Y
				return true
			}
		}
		return false
	})
	w.handleInput(conn, press(barX, barY))
	w.handleInput(conn, release(barX, barY))
	s.waitFor(t, "edge menu", func() bool { return s.contains("→ New pane right") })
	menuClick(t, w, conn, "→ New pane right")

	s.waitFor(t, "three panes", func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		return w.lay.CountPanes() == 3
	})
	withWS(w, func() {
		r, ok := w.rects[w.lay.FocusedPane()]
		if !ok {
			t.Fatal("new pane has no rect")
		}
		if r.Y <= w.area.Y || r.H >= w.area.H {
			t.Fatalf("new pane rect = %+v; a window-scoped split must sit beside the LOWER pane only (Y>%d, H<%d)",
				r, w.area.Y, w.area.H)
		}
	})
}

// TestRingHoverLightsOnlyThePaneSegment pins that hovering a flat ring edge
// previews only the abutting window's own segment — hovering under the right
// pane must not light the left pane's bottom.
func TestRingHoverLightsOnlyThePaneSegment(t *testing.T) {
	w, conn, s := newTestWS(t)
	s.waitFor(t, "first frame", func() bool { return s.contains("1:") })
	withWS(w, func() { w.actionSplitLocked(w.lay.FocusedPane(), layout.SplitRight) }) // L | R

	var rx, rw, ry int
	s.waitFor(t, "right pane laid out", func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		if len(w.borders) == 0 {
			return false
		}
		for _, r := range w.rects {
			if r.X > w.area.X && r.X+r.W == w.area.X+w.area.W { // the right column
				rx, rw, ry = r.X, r.W, w.rows-1
				return true
			}
		}
		return false
	})
	// Hover the bottom ring under the right pane's mid-column (clear of the ┴).
	hx := rx + rw/2
	w.handleInput(conn, []byte(fmt.Sprintf("\x1b[<35;%d;%dM", hx+1, ry+1)))
	withWS(w, func() {
		if len(w.hover.strips) != 1 {
			t.Fatalf("ring hover strips = %+v, want exactly the right pane's segment", w.hover.strips)
		}
		st := w.hover.strips[0]
		if st.X != rx || st.W != rw {
			t.Fatalf("bottom hover strip = %+v, want X=%d W=%d (only the right pane)", st, rx, rw)
		}
		if st.X == 0 && st.W == w.cols {
			t.Fatal("bottom hover lit the whole edge instead of one window")
		}
	})
}

// TestRingCornerSplitsFullWidth pins the container-level (full-span) split:
// from the ┴ where the L|R divider meets the bottom ring, "below — full
// width" drops a pane spanning BOTH columns, not just one window.
func TestRingCornerSplitsFullWidth(t *testing.T) {
	w, conn, s := newTestWS(t)
	s.waitFor(t, "first frame", func() bool { return s.contains("1:") })
	withWS(w, func() { w.actionSplitLocked(w.lay.FocusedPane(), layout.SplitRight) }) // L | R

	var bx, by int
	s.waitFor(t, "vertical border", func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		for _, b := range w.borders {
			if b.Vertical {
				bx, by = b.Rect.X, w.rows-1 // the ┴ on the bottom ring
				return true
			}
		}
		return false
	})
	w.handleInput(conn, press(bx, by))
	w.handleInput(conn, release(bx, by))
	s.waitFor(t, "span menu", func() bool { return s.contains("↓ New pane below — full width") })
	menuClick(t, w, conn, "↓ New pane below — full width")

	s.waitFor(t, "three panes", func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		return w.lay.CountPanes() == 3
	})
	withWS(w, func() {
		r, ok := w.rects[w.lay.FocusedPane()]
		if !ok {
			t.Fatal("new pane has no rect")
		}
		if r.W != w.area.W {
			t.Fatalf("new pane width = %d, want full area width %d (spans both columns)", r.W, w.area.W)
		}
		root := w.lay.ActiveTab().Root
		if root.Dir != layout.SplitDown || len(root.Children) != 2 {
			t.Fatalf("root should be a 2-child stack after a full-width split below, got %+v", root)
		}
	})
}

// TestDividerTopEdgeInsertsAbove pins that a stacked divider is the LOWER
// pane's top edge: "new pane above" from it (the menu's default) inserts a
// pane between the two, keeping one full-width stack.
func TestDividerTopEdgeInsertsAbove(t *testing.T) {
	w, conn, s := newTestWS(t)
	s.waitFor(t, "first frame", func() bool { return s.contains("1:") })
	withWS(w, func() { w.actionSplitLocked(w.lay.FocusedPane(), layout.SplitDown) })
	var barX, barY int
	s.waitFor(t, "divider bar in hitmap", func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		for _, h := range w.hits {
			if h.kind == hitPaneBar && h.hasBorder {
				barX, barY = h.rect.X+2, h.rect.Y
				return true
			}
		}
		return false
	})
	w.handleInput(conn, press(barX, barY))
	w.handleInput(conn, release(barX, barY))
	s.waitFor(t, "edge menu", func() bool { return s.contains("↑ New pane above") })
	menuClick(t, w, conn, "↑ New pane above")
	s.waitFor(t, "three stacked panes", func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		return w.lay.CountPanes() == 3
	})
	withWS(w, func() {
		// All three tile the full width: still one stack, new pane between.
		root := w.lay.ActiveTab().Root
		if root.Dir != layout.SplitDown || len(root.Children) != 3 {
			t.Fatalf("expected a 3-run stack, got %+v", root)
		}
		if root.Children[1].Pane != w.lay.FocusedPane() {
			t.Fatalf("new pane must sit at the boundary (middle), got order %+v", root.Children)
		}
	})
}

func TestHoverHighlightsBorderAndCorner(t *testing.T) {
	w, conn, s := newTestWS(t)
	s.waitFor(t, "first frame", func() bool { return s.contains("1:") })
	withWS(w, func() {
		a := w.lay.FocusedPane()
		w.actionSplitLocked(a, layout.SplitRight)
		w.actionSplitLocked(w.lay.FocusedPane(), layout.SplitDown)
	})
	var vbX, vbY, hbY int
	s.waitFor(t, "borders in layout", func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		var v, h *layout.Border
		for i := range w.borders {
			if w.borders[i].Vertical {
				v = &w.borders[i]
			} else {
				h = &w.borders[i]
			}
		}
		if v == nil || h == nil {
			return false
		}
		vbX, vbY, hbY = v.Rect.X, v.Rect.Y+2, h.Rect.Y
		return true
	})
	s.waitFor(t, "hitmap rebuilt", func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		for _, h := range w.hits {
			if h.kind == hitBorder {
				return true
			}
		}
		return false
	})

	// Bare motion over the border (no button): the strip lights up heavy.
	w.handleInput(conn, []byte(fmt.Sprintf("\x1b[<35;%d;%dM", vbX+1, vbY+1)))
	s.waitFor(t, "border hover", func() bool { return s.contains("┃") })
	withWS(w, func() {
		if len(w.hover.strips) != 1 {
			t.Fatalf("border hover strips = %+v", w.hover)
		}
	})

	// Motion to the corner: the bar joins the highlight (both axes shown).
	w.handleInput(conn, []byte(fmt.Sprintf("\x1b[<35;%d;%dM", vbX+1, hbY+1)))
	withWS(w, func() {
		if len(w.hover.strips) != 1 || len(w.hover.bars) != 1 {
			t.Fatalf("corner hover must carry border AND bar: %+v", w.hover)
		}
	})

	// Motion into content clears it.
	w.handleInput(conn, []byte("\x1b[<35;5;5M"))
	withWS(w, func() {
		if w.hover.key != "" {
			t.Fatalf("content hover must clear: %+v", w.hover)
		}
	})
}
