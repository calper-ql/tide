// The workspace is one session's live runtime: the layout of panes, the
// attached clients, selection/clipboard/scroll state, and the render loop
// that composites everything into the screen clients see. It is the "one
// mouse/keyboard routing layer the whole app shares" (spec: core
// foundation §4) — panes know nothing about clients, clients are dumb
// glass.
package daemon

import (
	"encoding/json"
	"log"
	"sync"
	"time"

	"github.com/calper-ql/tide/internal/input"
	"github.com/calper-ql/tide/internal/layout"
	"github.com/calper-ql/tide/internal/protocol"
	"github.com/calper-ql/tide/internal/session"
)

const (
	outboxSize  = 512                  // frames buffered per client before it is dropped
	renderDelay = 8 * time.Millisecond // output coalescing window (~120 fps cap)
)

type wsClient struct {
	out     chan protocol.Message
	decoder *input.Decoder
	feedGen uint64 // bumps per Feed; the idle-flush timer keys off it
}

// selectionState is the transient drag-selection of one pane, in content
// coordinates (history index space) so it stays glued to its text while
// output scrolls. Never structural, cleared by any keystroke (ratified
// Ctrl+C ruling guardrails).
type selectionState struct {
	pane      string
	dragging  bool
	exists    bool
	aLine, aX int // anchor
	eLine, eX int // end (moves with the drag)
}

// normalized returns the selection ordered start-before-end with an
// exclusive end column.
func (s selectionState) normalized() (l0, x0, l1, x1 int) {
	if s.aLine < s.eLine || (s.aLine == s.eLine && s.aX <= s.eX) {
		return s.aLine, s.aX, s.eLine, s.eX + 1
	}
	return s.eLine, s.eX, s.aLine, s.aX + 1
}

type dragState struct {
	border layout.Border
	lastX  int
	lastY  int
}

// pendingPress disambiguates frame gestures (ratified two-menu model):
// press+motion becomes a border drag (when there is a border to drag),
// press+release in place opens the layout menu for the owning pane.
type pendingPress struct {
	x, y      int
	pane      string
	border    layout.Border
	hasBorder bool
	moved     bool
}

type ws struct {
	d    *daemon
	root string
	logf *log.Logger

	mu       sync.Mutex
	lay      *layout.Layout
	panes    map[string]*pane
	clients  map[*protocol.Conn]*wsClient
	cols     int
	rows     int
	area     layout.Rect            // content area below the bar
	rects    map[string]layout.Rect // active tab pane rects, from last layout pass
	borders  []layout.Border
	scroll   map[string]int // pane id → scrollback offset (0 = live)
	sel      selectionState
	drag     *dragState
	pending  *pendingPress // frame press awaiting drag-vs-click resolution
	appGrab  string        // pane holding an app-forwarded mouse drag
	overlay  *overlay
	hits     []hitRegion
	clip     []byte // internal clipboard (ratified clipboard model)
	flash    string // transient bar status
	flashOff time.Time
	closing  bool
	killed   bool // session explicitly ended: checkpoints must stop

	dirtyPanes map[string]bool
	allDirty   bool
	renderSig  chan struct{}
	quit       chan struct{}
}

// newWS builds the workspace for a session, restoring layout and pane
// content from the registry and spawning a shell per pane.
func newWS(d *daemon, root string, stored session.Session, cols, rows int) (*ws, error) {
	w := &ws{
		d:          d,
		root:       root,
		logf:       d.logf,
		panes:      map[string]*pane{},
		clients:    map[*protocol.Conn]*wsClient{},
		scroll:     map[string]int{},
		dirtyPanes: map[string]bool{},
		renderSig:  make(chan struct{}, 1),
		quit:       make(chan struct{}),
	}
	w.setSizeLocked(cols, rows)

	if len(stored.Layout) > 0 {
		var l layout.Layout
		if err := json.Unmarshal(stored.Layout, &l); err == nil && len(l.Tabs) > 0 {
			w.lay = &l
		}
	}
	if w.lay == nil || !layoutValid(w.lay) {
		if w.lay != nil {
			// A stored layout that unmarshals but is structurally unsound
			// (hand edit, version skew) must not brick attaches — same
			// philosophy as the registry's quarantine.
			w.logf.Printf("WARNING: stored layout for %s is malformed; starting a fresh layout", root)
		}
		w.lay = layout.New(newPaneID())
	}
	// Persist immediately: pane content checkpoints are keyed by pane id,
	// and an unpersisted layout would orphan them on restore.
	w.checkpointLayoutLocked()
	// Spawn every pane in the layout; content restores from per-pane
	// checkpoints when present. Panes go live (and their hooks fire) the
	// moment they spawn, so publication into w.panes happens under the
	// lock the hooks read it with.
	spawned := map[string]*pane{}
	for _, id := range w.lay.PaneIDs() {
		var pc *session.PaneContent
		if c, ok := d.registry.LoadPaneContent(id); ok {
			pc = &c
		}
		p, err := w.spawnPane(id, pc, w.area.W, w.area.H)
		if err != nil {
			// A pane that cannot spawn (e.g. deleted root) fails the whole
			// attach; the caller reports it.
			for _, q := range spawned {
				q.shutdown()
			}
			close(w.quit)
			return nil, err
		}
		spawned[id] = p
	}
	w.mu.Lock()
	w.panes = spawned
	w.recomputeLocked()
	w.allDirty = true
	w.mu.Unlock()
	go w.renderLoop()
	return w, nil
}

// layoutValid rejects structurally unsound stored layouts: nil tabs or
// roots, empty or duplicate pane ids, child/ratio mismatches. The layout
// package never produces these; tampered state files can.
func layoutValid(l *layout.Layout) bool {
	if len(l.Tabs) == 0 {
		return false
	}
	seen := map[string]bool{}
	var validNode func(n *layout.Node) bool
	validNode = func(n *layout.Node) bool {
		if n == nil {
			return false
		}
		if n.Pane != "" {
			if seen[n.Pane] || len(n.Children) > 0 {
				return false
			}
			seen[n.Pane] = true
			return true
		}
		if len(n.Children) < 2 || len(n.Ratios) != len(n.Children) {
			return false
		}
		for _, c := range n.Children {
			if !validNode(c) {
				return false
			}
		}
		return true
	}
	for _, t := range l.Tabs {
		if t == nil || !validNode(t.Root) {
			return false
		}
	}
	if l.Active < 0 || l.Active >= len(l.Tabs) {
		l.Active = 0
	}
	return true
}

func (w *ws) spawnPane(id string, stored *session.PaneContent, cols, rows int) (*pane, error) {
	hooks := paneHooks{
		dirty:  func() { w.markDirty(id) },
		exited: func() { w.markDirty(id) },
		save: func(paneID string, cols, rows int, snapshot []byte, history [][]byte) {
			w.savePane(paneID, cols, rows, snapshot, history)
		},
	}
	return newPane(id, w.root, stored, cols, rows, w.d.socket, w.logf, hooks)
}

// savePane persists a pane checkpoint, rejecting stale panes (closed, or
// replaced) and killed workspaces, so a dying pane can never resurrect
// ended content. The closing bypass exists for daemon shutdown, where the
// final checkpoints run after the panes map is conceptually frozen.
func (w *ws) savePane(paneID string, cols, rows int, snapshot []byte, history [][]byte) {
	w.mu.Lock()
	_, current := w.panes[paneID]
	closing := w.closing
	killed := w.killed
	w.mu.Unlock()
	if killed || (!current && !closing) {
		return
	}
	pc := session.PaneContent{Cols: cols, Rows: rows, Snapshot: snapshot, History: history}
	if err := w.d.registry.UpdatePaneContent(w.root, paneID, pc); err != nil {
		w.logf.Printf("checkpoint failed pane=%s: %v", paneID, err)
	}
}

// checkpointLayoutLocked persists the layout tree; structural changes are
// rare, so this is synchronous.
func (w *ws) checkpointLayoutLocked() {
	data, err := json.Marshal(w.lay)
	if err != nil {
		return
	}
	if err := w.d.registry.UpdateLayout(w.root, data); err != nil {
		w.logf.Printf("layout checkpoint failed root=%s: %v", w.root, err)
	}
}

func (w *ws) paneIDs() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.lay.PaneIDs()
}

func (w *ws) paneCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.lay.CountPanes()
}

func (w *ws) clientCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.clients)
}

// setSizeLocked applies the virtual screen size (latest-wins across
// clients) and derives the pane area: below the session bar (ratified:
// bar on top), inside the outer frame ring (ratified: pane frames — the
// ring's left/right columns and bottom row belong to the frames).
func (w *ws) setSizeLocked(cols, rows int) {
	cols, rows = clampDim(cols, 80), clampDim(rows, 24)
	w.cols, w.rows = cols, rows
	w.area = layout.Rect{X: 1, Y: 1, W: cols - 2, H: rows - 2}
}

// contentRect is the part of a pane's rect its grid renders into: the
// rect minus its top bar row.
func contentRect(r layout.Rect) layout.Rect {
	return layout.Rect{X: r.X, Y: r.Y + 1, W: r.W, H: r.H - 1}
}

// recomputeLocked lays out the active tab and sizes its panes to their
// rects. Background tabs keep their sizes until activated.
func (w *ws) recomputeLocked() {
	tab := w.lay.ActiveTab()
	if tab == nil {
		w.rects, w.borders = map[string]layout.Rect{}, nil
		return
	}
	w.rects, w.borders = tab.Compute(w.area)
	for id, r := range w.rects {
		if p := w.panes[id]; p != nil {
			c := contentRect(r)
			p.resize(c.W, c.H)
		}
	}
}

// attach registers a client and enqueues the attach reply (carrying a full
// repaint) as the first frame of its outbox, so no render frame can ever
// precede it on the wire. The reply builder runs under w.mu and must not
// call back into the workspace — everything it needs is passed in.
func (w *ws) attach(conn *protocol.Conn, cols, rows int,
	reply func(firstFrame []byte, clients, panes int) protocol.Message) (clients int, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.setSizeLocked(cols, rows)
	w.recomputeLocked()
	w.allDirty = true
	c := &wsClient{out: make(chan protocol.Message, outboxSize), decoder: input.NewDecoder()}
	w.clients[conn] = c
	frame := w.renderLocked() // full repaint; also rebuilds the hitmap
	c.out <- reply(frame, len(w.clients), w.lay.CountPanes())
	go clientWriter(conn, c.out)
	// renderLocked consumed the dirty state into the newcomer's frame, but
	// the relayout (and any dirt pending at attach time) belongs to every
	// already-attached client too.
	w.allDirty = true
	w.signalRender()
	return len(w.clients), nil
}

// clientWriter drains one client's outbox so a slow client can never stall
// the workspace.
func clientWriter(conn *protocol.Conn, out <-chan protocol.Message) {
	for m := range out {
		if conn.Send(m) != nil {
			conn.Close()
			for range out { // drain until the workspace closes the channel
			}
			return
		}
	}
}

func (w *ws) removeClient(conn *protocol.Conn) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if c, ok := w.clients[conn]; ok {
		delete(w.clients, conn)
		close(c.out)
	}
}

// sendTo enqueues a frame for one client without ever blocking the
// workspace; an unresponsive client is dropped.
func (w *ws) sendToLocked(conn *protocol.Conn, m protocol.Message) {
	c, ok := w.clients[conn]
	if !ok {
		return
	}
	select {
	case c.out <- m:
	default:
		delete(w.clients, conn)
		close(c.out)
		w.logf.Printf("drop slow client root=%s", w.root)
		go func() {
			_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
			_ = conn.Send(protocol.Message{Type: protocol.TypeDropped, Err: "client too slow consuming output"})
			_ = conn.Close()
		}()
	}
}

func (w *ws) broadcastLocked(m protocol.Message) {
	for conn := range w.clients {
		w.sendToLocked(conn, m)
	}
}

// takeClients hands every attached conn to the caller (kill sweep) and
// stops their writers.
func (w *ws) takeClients() []*protocol.Conn {
	w.mu.Lock()
	defer w.mu.Unlock()
	conns := make([]*protocol.Conn, 0, len(w.clients))
	for conn, c := range w.clients {
		delete(w.clients, conn)
		close(c.out)
		conns = append(conns, conn)
	}
	return conns
}

func (w *ws) resizeClient(cols, rows int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if cols == w.cols && rows == w.rows {
		return
	}
	w.setSizeLocked(cols, rows)
	w.recomputeLocked()
	w.allDirty = true
	w.signalRender()
}

// markDirty is the pane dirt callback; it must be safe from pane
// goroutines at any time.
func (w *ws) markDirty(paneID string) {
	w.mu.Lock()
	w.dirtyPanes[paneID] = true
	w.mu.Unlock()
	w.signalRender()
}

func (w *ws) markAllDirtyLocked() {
	w.allDirty = true
	w.signalRender()
}

func (w *ws) signalRender() {
	select {
	case w.renderSig <- struct{}{}:
	default:
	}
}

// renderLoop coalesces dirt into at most one composite render per
// renderDelay and broadcasts it.
func (w *ws) renderLoop() {
	for {
		select {
		case <-w.quit:
			return
		case <-w.renderSig:
		}
		time.Sleep(renderDelay)
		select { // drain anything that accumulated during the window
		case <-w.renderSig:
		default:
		}
		w.mu.Lock()
		if w.closing {
			w.mu.Unlock()
			return
		}
		frame := w.renderLocked()
		if len(frame) > 0 {
			w.broadcastLocked(protocol.Message{Type: protocol.TypeRender, Data: frame})
		}
		w.mu.Unlock()
	}
}

// flashStatus shows a transient message in the bar.
func (w *ws) flashStatusLocked(msg string) {
	w.flash = msg
	w.flashOff = time.Now().Add(2 * time.Second)
	w.allDirty = true
	w.signalRender()
	time.AfterFunc(2100*time.Millisecond, func() {
		w.mu.Lock()
		if time.Now().After(w.flashOff) && w.flash != "" {
			w.flash = ""
			w.allDirty = true
			w.signalRender()
		}
		w.mu.Unlock()
	})
}

// markKilled stops all future checkpoints: the session was explicitly
// ended and its state files are being removed — a dying pane's final
// checkpoint must not resurrect them. Call before teardown on kill paths.
func (w *ws) markKilled() {
	w.mu.Lock()
	w.killed = true
	w.mu.Unlock()
}

// teardown checkpoints and stops every pane; used by both session kill and
// daemon shutdown. The layout (including focus) checkpoints with it so a
// controlled restart restores exactly what was on screen.
func (w *ws) teardown() {
	w.mu.Lock()
	if w.closing {
		w.mu.Unlock()
		return
	}
	w.closing = true
	if !w.killed {
		w.checkpointLayoutLocked()
	}
	panes := make([]*pane, 0, len(w.panes))
	for _, p := range w.panes {
		panes = append(panes, p)
	}
	w.mu.Unlock()
	close(w.quit)
	for _, p := range panes {
		p.shutdown()
	}
}
