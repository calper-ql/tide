// The compositor renders a workspace — bar, panes, borders, overlays —
// into ANSI frames clients write verbatim. Every clickable thing it draws
// it also records in the hitmap, which is what makes the chrome mouse-first
// and discoverable (spec requirements 3 and 5): the router never guesses at
// geometry, it asks the last render.
package daemon

import (
	"bytes"
	"fmt"
	"strings"
	"time"

	"github.com/mattn/go-runewidth"

	"github.com/calper-ql/tide/internal/layout"
	"github.com/calper-ql/tide/internal/protocol"
	"github.com/calper-ql/tide/internal/vt"
)

type hitKind int

const (
	hitNone hitKind = iota
	hitTabLabel
	hitNewTab
	hitDetach
	hitSessionMenu // the project-name segment (ratified: session menu)
	hitPane        // pane CONTENT (inside the frame)
	hitPaneBar     // a pane's top-border bar; doubles as the stacked-divider drag handle
	hitPaneSplit   // the [+] button in a pane bar: the visible split affordance
	hitPaneMenu    // the [≡] button in a pane bar
	hitBorder      // shared vertical border between side-by-side panes
	hitFrameEdge   // outer ring cells; owner pane resolved by adjacency
	hitMenuItem
	hitOverlayBody // inside an overlay but not on an item: swallow the click
)

type hitRegion struct {
	rect      layout.Rect
	kind      hitKind
	tab       int
	pane      string
	border    layout.Border
	hasBorder bool // pane bars that are stacked dividers carry their border
	item      int
}

// hitAtLocked returns the topmost region under a screen cell; regions are
// appended in z-order during render, so the scan runs backwards.
func (w *ws) hitAtLocked(x, y int) hitRegion {
	for i := len(w.hits) - 1; i >= 0; i-- {
		r := w.hits[i].rect
		if x >= r.X && x < r.X+r.W && y >= r.Y && y < r.Y+r.H {
			return w.hits[i]
		}
	}
	return hitRegion{kind: hitNone}
}

type menuItem struct {
	label     string
	enabled   bool
	separator bool // a dim rule row: not hittable, skipped by Enter/arrows
	danger    bool // destructive action: red at rest
	run       func(w *ws, origin *protocol.Conn)
}

// overlay is a popup: a context menu, or a confirm dialog (a titled menu).
// sel is the highlighted item — pre-lit when the menu opens (so click-click
// runs the default and Enter is advertised without needing 1003 hover) and
// moved by pointer hover or Up/Down. Esc closes; a click outside closes.
type overlay struct {
	x, y  int // top-left (clamped into the screen at render time)
	title string
	items []menuItem
	pane  string // context pane
	sel   int    // highlighted item; -1 none
}

const sgrReset = "\x1b[0m"

func cup(b *bytes.Buffer, y, x int) {
	fmt.Fprintf(b, "\x1b[%d;%dH", y+1, x+1)
}

// renderLocked composites one frame: dirty panes (or everything), the bar,
// borders, overlay, cursor. It rebuilds the hitmap as it draws.
func (w *ws) renderLocked() []byte {
	// The minimum must exceed the bar's right-side reservation (the detach
	// button) and leave room for session bar + frame ring + a pane bar +
	// content.
	if w.cols < 16 || w.rows < 6 {
		return nil
	}
	if w.flash != "" && time.Now().After(w.flashOff) {
		w.flash = ""
	}
	w.th = w.d.themeNow() // one theme per frame; switches land atomically
	var b bytes.Buffer
	b.WriteString("\x1b[?25l" + sgrReset)
	full := w.allDirty
	// Chrome-only repaints (hover transitions) redraw the frame, bars, and
	// hover strokes in place — no screen clear, no content redraw — so moving
	// the pointer across an edge doesn't blank and rebuild the whole screen.
	chrome := full || w.chromeDirty
	if full {
		b.WriteString("\x1b[2J")
	}
	w.hits = w.hits[:0]

	w.renderBarLocked(&b)

	if tab := w.lay.ActiveTab(); tab != nil {
		if chrome {
			w.renderFrameLocked(&b)
		}
		// Stacked dividers are pane bars: index the horizontal borders by
		// the bar row they coincide with.
		barBorder := map[[2]int]layout.Border{}
		for _, bd := range w.borders {
			if !bd.Vertical {
				barBorder[[2]int{bd.Rect.X, bd.Rect.Y}] = bd
			}
		}
		focused := w.lay.FocusedPane()
		for id, r := range w.rects {
			c := contentRect(r)
			w.hits = append(w.hits, hitRegion{rect: c, kind: hitPane, pane: id})
			w.renderPaneBarLocked(&b, id, r, id == focused, barBorder)
			if full || w.dirtyPanes[id] {
				w.renderPaneContentLocked(&b, id, c)
			}
		}
		if chrome {
			w.renderHoverLocked(&b)
		}
		for _, bd := range w.borders {
			if bd.Vertical {
				w.hits = append(w.hits, hitRegion{rect: bd.Rect, kind: hitBorder, border: bd, hasBorder: true})
			}
		}
		// The outer ring: one strip per side, owner resolved by adjacency.
		w.hits = append(w.hits,
			hitRegion{rect: layout.Rect{X: 0, Y: w.area.Y, W: 1, H: w.area.H + 1}, kind: hitFrameEdge},
			hitRegion{rect: layout.Rect{X: w.cols - 1, Y: w.area.Y, W: 1, H: w.area.H + 1}, kind: hitFrameEdge},
			hitRegion{rect: layout.Rect{X: 0, Y: w.rows - 1, W: w.cols, H: 1}, kind: hitFrameEdge},
		)
	}
	if w.overlay != nil {
		w.renderOverlayLocked(&b)
	}
	w.placeCursorLocked(&b)
	w.dirtyPanes = map[string]bool{}
	w.allDirty = false
	w.chromeDirty = false
	return b.Bytes()
}

// barSeg writes one bar segment at the running column, recording a hit
// region when kind != hitNone, and returns the new column.
func (w *ws) barSeg(b *bytes.Buffer, col int, text, style string, kind hitKind, tabIdx int) int {
	wd := runewidth.StringWidth(text)
	if col+wd > w.cols {
		return col
	}
	b.WriteString(style)
	b.WriteString(text)
	if kind != hitNone {
		w.hits = append(w.hits, hitRegion{rect: layout.Rect{X: col, Y: 0, W: wd, H: 1}, kind: kind, tab: tabIdx})
	}
	return col + wd
}

// renderBarLocked draws the top bar: project, tabs, [+], status, and the
// '-' detach button at the far right (ratified: bar on top, '-' detaches).
func (w *ws) renderBarLocked(b *bytes.Buffer) {
	cup(b, 0, 0)
	b.WriteString(w.th.bar)
	b.WriteString(strings.Repeat(" ", w.cols)) // paint the row, then place segments
	cup(b, 0, 0)

	base := w.root
	if i := strings.LastIndexByte(base, '/'); i >= 0 && i < len(base)-1 {
		base = base[i+1:]
	}
	// Bar buttons brighten under the pointer (1003 terminals).
	seg := func(kind hitKind, tab int, base string) string {
		if w.hover.barKind == kind && (kind != hitTabLabel || w.hover.barTab == tab) {
			return w.th.barHover
		}
		return base
	}
	// The project segment is the session menu button (ratified): New Tab,
	// Detach, Kill Session live behind it. It is prefixed with the host the
	// session runs on (host:project) so you always know which machine you're
	// on — the cue lost when attaching remotely via `tide -r`.
	label := runewidth.Truncate(base, 24, "…")
	if w.host != "" {
		label = runewidth.Truncate(w.host, 24, "…") + ":" + label
	}
	col := w.barSeg(b, 0, " "+label+" ▾", seg(hitSessionMenu, 0, w.th.accentBar), hitSessionMenu, 0)
	col = w.barSeg(b, col, "▏", w.th.bar, hitNone, 0)

	for i, tab := range w.lay.Tabs {
		style := w.th.bar
		if i == w.lay.Active {
			style = w.th.accentBar
		}
		col = w.barSeg(b, col, " "+fmt.Sprintf("%d:%s", i+1, w.tabTitleLocked(tab))+" ", seg(hitTabLabel, i, style), hitTabLabel, i)
	}
	col = w.barSeg(b, col, " + ", seg(hitNewTab, 0, w.th.bar), hitNewTab, 0)

	// Right side: transient status / scroll indicator, then the detach
	// button pinned to the corner. Flashes pop (bold reverse); the scroll
	// indicator is ambient and stays on the bar's soft strip.
	status, statusStyle := w.flash, w.th.flash
	if status == "" {
		if s := w.scroll[w.lay.FocusedPane()]; s > 0 {
			status = fmt.Sprintf("SCROLL %d — wheel down or any key to resume", s)
			statusStyle = w.th.bar
		}
	}
	detach := " ─ detach "
	dw := runewidth.StringWidth(detach)
	if status != "" {
		// Truncate to the room between the tabs and the detach button: a
		// shortened "copied 17 c…" still confirms the action; dropping the
		// flash entirely reads as the copy having failed.
		if avail := w.cols - dw - col - 3; avail >= 8 {
			status = runewidth.Truncate(status, avail, "…")
			cup(b, 0, w.cols-dw-runewidth.StringWidth(status)-2)
			b.WriteString(statusStyle + status)
		}
	}
	cup(b, 0, w.cols-dw)
	detachStyle := w.th.accentBar
	if w.hover.barKind == hitDetach {
		detachStyle = w.th.barHover
	}
	b.WriteString(detachStyle + detach + sgrReset)
	w.hits = append(w.hits, hitRegion{rect: layout.Rect{X: w.cols - dw, Y: 0, W: dw, H: 1}, kind: hitDetach})
}

// tabTitleLocked names a tab: explicit name, else the focused pane's OSC
// title, else "shell". Titles are PANE-CONTROLLED bytes: control and
// zero-width runes would corrupt the bar's column accounting (and with it
// the hitmap), so they are stripped before display.
func (w *ws) tabTitleLocked(t *layout.Tab) string {
	if t.Name != "" {
		return runewidth.Truncate(sanitizeLabel(t.Name), 16, "…")
	}
	if p := w.panes[t.Focused]; p != nil {
		if title := sanitizeLabel(p.term.TitleSnapshot()); title != "" {
			return runewidth.Truncate(title, 16, "…")
		}
	}
	return "shell"
}

// sanitizeLabel drops runes that print nothing or move the cursor: C0/C1
// controls and zero-width characters.
func sanitizeLabel(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r < 0x20 || (r >= 0x7f && r < 0xa0) || runewidth.RuneWidth(r) == 0 {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// renderFrameLocked draws the outer ring and the shared vertical borders
// (full renders only; bars and contents overwrite their own cells after).
func (w *ws) renderFrameLocked(b *bytes.Buffer) {
	b.WriteString(w.th.frame)
	top, bottom := w.area.Y, w.rows-1
	// Side columns.
	for y := top; y < bottom; y++ {
		cup(b, y, 0)
		b.WriteString("│")
		cup(b, y, w.cols-1)
		b.WriteString("│")
	}
	// Vertical shared borders, with their bottom junctions noted.
	junction := map[int]bool{}
	for _, bd := range w.borders {
		if !bd.Vertical {
			continue
		}
		for y := 0; y < bd.Rect.H; y++ {
			cup(b, bd.Rect.Y+y, bd.Rect.X)
			b.WriteString("│")
		}
		if bd.Rect.Y+bd.Rect.H == bottom {
			junction[bd.Rect.X] = true
		}
	}
	// Bottom row.
	cup(b, bottom, 0)
	b.WriteString("╰")
	for x := 1; x < w.cols-1; x++ {
		if junction[x] {
			b.WriteString("┴")
		} else {
			b.WriteString("─")
		}
	}
	b.WriteString("╯" + sgrReset)

	// Focused pane perimeter: its side columns (and bottom run when it
	// rests on the ring) pick up the accent, Zellij-style — "where am I"
	// at a glance. Bars re-stamp their junction cells right after.
	if r, ok := w.rects[w.lay.FocusedPane()]; ok {
		b.WriteString(w.th.focus)
		for y := r.Y; y < r.Y+r.H; y++ {
			cup(b, y, r.X-1)
			b.WriteString("│")
			cup(b, y, r.X+r.W)
			b.WriteString("│")
		}
		if r.Y+r.H == bottom {
			cup(b, bottom, r.X)
			b.WriteString(strings.Repeat("─", r.W))
		}
		b.WriteString(sgrReset)
	}
}

// renderPaneBarLocked draws a pane's top border as its bar — title left,
// [+] split and [≡] menu buttons right — including the junction characters
// in the flanking border columns. A bar that coincides with a stacked
// divider carries that border so the router can drag it. On narrow panes
// [+] drops before the bar does: the bar is also the focus handle.
func (w *ws) renderPaneBarLocked(b *bytes.Buffer, id string, r layout.Rect, focused bool, barBorder map[[2]int]layout.Border) {
	if r.W < 6 || r.H < 2 {
		return
	}
	p := w.panes[id]
	dead := p != nil && p.isDead()
	style, stroke := w.th.frame, "─"
	if focused {
		style = w.th.focus
	}
	if dead {
		style = w.th.dead
	}
	if w.hover.bars[id] {
		style, stroke = w.th.hover, "━" // the boundary under the pointer
	}
	// Flanking junctions live in the neighboring border/ring columns. Their
	// glyph depends on whether the vertical line continues ABOVE this row: a
	// bar whose border only drops downward (e.g. a full-width pane was split
	// in above it) gets a ┬, not a ├/┤ that would poke up into the pane above.
	leftCol, rightCol := r.X-1, r.X+r.W
	left := barJunction(w.lineAboveLocked(leftCol, r.Y), leftCol != 0, true)
	right := barJunction(w.lineAboveLocked(rightCol, r.Y), true, rightCol != w.cols-1)
	cup(b, r.Y, r.X-1)
	b.WriteString(style + left)

	title := ""
	if p != nil {
		title = sanitizeLabel(p.term.TitleSnapshot())
	}
	if title == "" {
		title = "shell"
	}
	// Buttons drop before the bar does (the bar is also the focus handle,
	// and a row that overflows corrupts the flanking junction): [+] needs a
	// 1-cell title beside both buttons, [≡] a 1-cell title beside itself.
	const menu, split = "[≡]", "[+]"
	hasSplit := r.W-4-6 >= 1 // stroke+" "+title+" "+fill+[+][≡]+stroke
	hasMenu := hasSplit || r.W-4-3 >= 1
	btns := ""
	if hasSplit {
		btns = split + menu
	} else if hasMenu {
		btns = menu
	}
	maxTitle := r.W - 4 - runewidth.StringWidth(btns)
	if dead {
		// The bar is the restart affordance; say so when there is room —
		// hidden behind a click is fine, hidden behind knowledge is not.
		title += " (exited)"
		if hint := title + " — click to restart"; runewidth.StringWidth(hint) <= maxTitle {
			title = hint
		}
	}
	title = runewidth.Truncate(title, max(maxTitle, 1), "…")
	tw := runewidth.StringWidth(title)
	fill := maxTitle - tw
	fmt.Fprintf(b, "%s %s %s%s%s", stroke, title, strings.Repeat(stroke, max(fill, 0)), btns, stroke)
	b.WriteString(right + sgrReset)

	bd, hasBorder := barBorder[[2]int{r.X, r.Y}]
	w.hits = append(w.hits, hitRegion{rect: layout.Rect{X: r.X, Y: r.Y, W: r.W, H: 1}, kind: hitPaneBar, pane: id, border: bd, hasBorder: hasBorder})
	if hasMenu {
		w.hits = append(w.hits, hitRegion{rect: layout.Rect{X: r.X + r.W - 4, Y: r.Y, W: 4, H: 1}, kind: hitPaneMenu, pane: id})
	}
	if hasSplit {
		w.hits = append(w.hits, hitRegion{rect: layout.Rect{X: r.X + r.W - 7, Y: r.Y, W: 3, H: 1}, kind: hitPaneSplit, pane: id})
	}
}

// barJunction picks the box-drawing glyph for a pane bar's flanking column.
// A bar always continues downward (the border/ring below it), so the choice
// turns on up (a vertical line also above) and left/right (the bar extends
// to each side): e.g. up&left&right → ┼, !up&left&right → ┬ (a border that
// only drops below, the top of an L|R split with a full-width pane above).
func barJunction(up, left, right bool) string {
	switch {
	case up && left && right:
		return "┼"
	case up && right: // up && !left
		return "├"
	case up && left: // up && !right
		return "┤"
	case left && right: // !up
		return "┬"
	case right: // !up && !left
		return "╭"
	case left: // !up && !right
		return "╮"
	default:
		return "┬"
	}
}

// lineAboveLocked reports whether a vertical frame line occupies the cell
// directly above (col, y): the outer ring runs the full height, an internal
// vertical border only where its span reaches. Above the area's top row sits
// the session bar, which carries no frame line.
func (w *ws) lineAboveLocked(col, y int) bool {
	if y <= w.area.Y {
		return false
	}
	if col == 0 || col == w.cols-1 {
		return true
	}
	for i := range w.borders {
		b := w.borders[i]
		if b.Vertical && b.Rect.X == col && b.Rect.Y <= y-1 && y-1 < b.Rect.Y+b.Rect.H {
			return true
		}
	}
	return false
}

// renderHoverLocked overdraws the hovered boundary strips with heavy
// bright strokes (bars highlight via their own style; this covers vertical
// borders and ring segments). Corner hovers carry several strips — the
// preview of what a corner gesture affects.
func (w *ws) renderHoverLocked(b *bytes.Buffer) {
	if len(w.hover.strips) == 0 {
		return
	}
	b.WriteString(w.th.hover)
	for _, s := range w.hover.strips {
		if s.W == 1 {
			for y := 0; y < s.H; y++ {
				cup(b, s.Y+y, s.X)
				b.WriteString("┃")
			}
		} else {
			cup(b, s.Y, s.X)
			b.WriteString(strings.Repeat("━", s.W))
		}
	}
	b.WriteString(sgrReset)
}

// renderPaneContentLocked draws one pane's viewport, applying the scroll
// offset and inverting the selection. r is the CONTENT rect (inside the
// frame).
func (w *ws) renderPaneContentLocked(b *bytes.Buffer, id string, r layout.Rect) {
	p := w.panes[id]
	if p == nil || r.W < 1 || r.H < 1 {
		return
	}
	// View clamps the scroll itself and reports the content start it used,
	// captured under one lock — racing history growth must not shift the
	// selection mapping.
	view, contentStart := p.term.View(w.scroll[id], r.H)

	selOn := w.sel.exists || w.sel.dragging
	var l0, x0, l1, x1 int
	if selOn && w.sel.pane == id {
		l0, x0, l1, x1 = w.sel.normalized()
	} else {
		selOn = false
	}

	for row := 0; row < r.H && row < len(view); row++ {
		invFrom, invTo := -1, -1
		if selOn {
			cl := contentStart + row
			if cl >= l0 && cl <= l1 {
				invFrom, invTo = 0, r.W
				if cl == l0 {
					invFrom = x0
				}
				if cl == l1 {
					invTo = x1
				}
			}
		}
		cup(b, r.Y+row, r.X)
		vt.RenderSegment(b, view[row], 0, r.W, invFrom, invTo)
	}
	b.WriteString(sgrReset)
	if s := w.scroll[id]; s > 0 {
		tag := fmt.Sprintf(" +%d ", s)
		if tw := runewidth.StringWidth(tag); tw <= r.W {
			cup(b, r.Y, r.X+r.W-tw)
			b.WriteString(w.th.accentBar + tag + sgrReset)
		}
	}
}

// overlaySize returns the popup's outer dimensions; placement (the anchor
// math in the router's open* helpers) and rendering must agree on them.
func (w *ws) overlaySize(o *overlay) (wd, ht int) {
	wd = runewidth.StringWidth(o.title)
	for _, it := range o.items {
		if l := runewidth.StringWidth(it.label); l > wd {
			wd = l
		}
	}
	wd += 2                       // one space of padding each side
	wd = clampInt(wd, 14, w.cols) // pad() truncates content to fit
	ht = len(o.items)
	if o.title != "" {
		ht += 2 // title + its rule
	}
	ht = clampInt(ht, 1, w.rows-1)
	return wd, ht
}

// overlayHeadRows is the row count above the first item (title + rule).
func overlayHeadRows(o *overlay) int {
	if o.title != "" {
		return 2
	}
	return 0
}

// renderOverlayLocked draws the popup — a borderless card: the surface
// background IS the boundary (no box drawing). Rows are title, a dim rule,
// then items (separators are dim rules too). It records its hit regions
// last, so it owns every cell it covers.
func (w *ws) renderOverlayLocked(b *bytes.Buffer) {
	o := w.overlay
	wd, ht := w.overlaySize(o)
	x := clampInt(o.x, 0, w.cols-wd)
	y := clampInt(o.y, 1, w.rows-ht)
	rule := strings.Repeat("─", wd-2)

	row := y
	if o.title != "" {
		cup(b, row, x)
		b.WriteString(w.th.menuTitle + " " + pad(o.title, wd-2) + " ")
		row++
		cup(b, row, x)
		b.WriteString(w.th.menuDim + " " + rule + " ")
		row++
	}
	itemsStart := len(w.hits)
	for i, it := range o.items {
		if row >= y+ht || row >= w.rows {
			break // clipped by a tiny screen; unreachable items stay unclickable
		}
		cup(b, row, x)
		text := it.label
		style := w.th.menu
		switch {
		case it.separator:
			style, text = w.th.menuDim, rule
		case !it.enabled:
			style = w.th.menuDim
		case it.danger:
			style = w.th.menuDanger
		}
		if i == o.sel && it.enabled && !it.separator {
			style = w.th.menuHover
		}
		b.WriteString(style + " " + pad(text, wd-2) + " ")
		if !it.separator {
			w.hits = append(w.hits, hitRegion{rect: layout.Rect{X: x, Y: row, W: wd, H: 1}, kind: hitMenuItem, item: i})
		}
		row++
	}
	b.WriteString(sgrReset)
	// The body region must sit BELOW the item regions in z-order (the
	// hitmap scans backwards): splice it in before them.
	body := hitRegion{rect: layout.Rect{X: x, Y: y, W: wd, H: ht}, kind: hitOverlayBody}
	itemsCopy := append([]hitRegion(nil), w.hits[itemsStart:]...)
	w.hits = append(w.hits[:itemsStart], body)
	w.hits = append(w.hits, itemsCopy...)
}

func pad(s string, w int) string {
	s = runewidth.Truncate(s, w, "…")
	return s + strings.Repeat(" ", max(0, w-runewidth.StringWidth(s)))
}

func clampInt(v, lo, hi int) int {
	if hi < lo {
		hi = lo
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// placeCursorLocked shows the focused pane's cursor when it is live (not
// scrolled back) and no overlay is up.
func (w *ws) placeCursorLocked(b *bytes.Buffer) {
	if w.overlay != nil {
		return
	}
	focused := w.lay.FocusedPane()
	p := w.panes[focused]
	r, ok := w.rects[focused]
	if p == nil || !ok || w.scroll[focused] != 0 {
		return
	}
	c := contentRect(r)
	x, y, visible := p.term.CursorState()
	if !visible || x >= c.W || y >= c.H {
		return
	}
	cup(b, c.Y+y, c.X+x)
	// Forward the focused pane's cursor shape (DECSCUSR) so vim's beam/block
	// reaches the client. Only when the pane set one — otherwise the client
	// keeps its own default.
	if shape := p.term.CursorShape(); shape > 0 {
		fmt.Fprintf(b, "\x1b[%d q", shape)
	}
	b.WriteString("\x1b[?25h")
}

// detachClientLocked sends the polite detach frame; the client exits and
// closes the conn, which triggers normal cleanup.
func (w *ws) detachClientLocked(conn *protocol.Conn) {
	w.sendToLocked(conn, protocol.Message{Type: protocol.TypeDetached})
}
