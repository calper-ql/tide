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
	label   string
	enabled bool
	run     func(w *ws, origin *protocol.Conn)
}

// overlay is a popup: a context menu, or a confirm dialog (a titled menu).
// Enter runs the first enabled item, Esc closes (also stated in the title
// bar of confirms — hidden behind a click is fine, hidden behind knowledge
// is not).
type overlay struct {
	x, y  int // anchor (clamped into the screen at render time)
	title string
	items []menuItem
	pane  string // context pane
}

// The theme is built strictly from the terminal's own 16-color palette and
// default fg/bg, so tide inherits whatever theme the user's terminal
// wears: one accent (ANSI cyan — re-themed by the user's palette), dim
// structure, a soft reverse strip for the session bar. No hardcoded RGB:
// it adapts to light and dark themes alike and survives terminals without
// truecolor (stock macOS Terminal.app included).
const (
	sgrReset = "\x1b[0m"

	thAccent    = "\x1b[0;1;36m"   // interactive emphasis (text on default bg)
	thAccentBar = "\x1b[0;7;1;36m" // accent "pill" on the chrome strip
	thBar       = "\x1b[0;7;2m"    // session bar base: soft adaptive strip
	thFrame     = "\x1b[0;2m"      // pane frames at rest
	thFocus     = "\x1b[0;36m"     // focused pane frame/bar
	thHover     = "\x1b[0;1;96m"   // hovered boundary: bright accent, heavy strokes
	thMenu      = "\x1b[0;100;97m" // popup surface: bright-black bg, bright text
	thMenuDim   = "\x1b[0;100;90m" // disabled item
	thMenuHover = "\x1b[0;7;1;36m" // item under the pointer
	thDead      = "\x1b[0;2;31m"   // exited pane bar
)

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
	var b bytes.Buffer
	b.WriteString("\x1b[?25l" + sgrReset)
	full := w.allDirty
	if full {
		b.WriteString("\x1b[2J")
	}
	w.hits = w.hits[:0]

	w.renderBarLocked(&b)

	if tab := w.lay.ActiveTab(); tab != nil {
		if full {
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
		if full {
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
	b.WriteString(thBar)
	b.WriteString(strings.Repeat(" ", w.cols)) // paint the row, then place segments
	cup(b, 0, 0)

	base := w.root
	if i := strings.LastIndexByte(base, '/'); i >= 0 && i < len(base)-1 {
		base = base[i+1:]
	}
	// Bar buttons brighten under the pointer (1003 terminals).
	const hoverPill = "\x1b[0;7;1;96m"
	seg := func(kind hitKind, tab int, base string) string {
		if w.hover.barKind == kind && (kind != hitTabLabel || w.hover.barTab == tab) {
			return hoverPill
		}
		return base
	}
	// The project segment is the session menu button (ratified): New Tab,
	// Detach, Kill Session live behind it.
	col := w.barSeg(b, 0, " "+runewidth.Truncate(base, 24, "…")+" ▾", seg(hitSessionMenu, 0, thAccentBar), hitSessionMenu, 0)
	col = w.barSeg(b, col, "▏", thBar, hitNone, 0)

	for i, tab := range w.lay.Tabs {
		style := thBar
		if i == w.lay.Active {
			style = thAccentBar
		}
		col = w.barSeg(b, col, " "+fmt.Sprintf("%d:%s", i+1, w.tabTitleLocked(tab))+" ", seg(hitTabLabel, i, style), hitTabLabel, i)
	}
	col = w.barSeg(b, col, " + ", seg(hitNewTab, 0, thBar), hitNewTab, 0)

	// Right side: transient status / scroll indicator, then the detach
	// button pinned to the corner.
	status := w.flash
	if status == "" {
		if s := w.scroll[w.lay.FocusedPane()]; s > 0 {
			status = fmt.Sprintf("SCROLL %d — wheel down or any key to resume", s)
		}
	}
	detach := " ─ detach "
	dw := runewidth.StringWidth(detach)
	if status != "" {
		sw := runewidth.StringWidth(status)
		x := w.cols - dw - sw - 2
		if x > col {
			cup(b, 0, x)
			b.WriteString(thBar + status)
		}
	}
	cup(b, 0, w.cols-dw)
	detachStyle := thAccentBar
	if w.hover.barKind == hitDetach {
		detachStyle = "\x1b[0;7;1;96m"
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
	b.WriteString(thFrame)
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
		b.WriteString(thFocus)
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
// [≡] menu button right (ratified) — including the junction characters in
// the flanking border columns. A bar that coincides with a stacked divider
// carries that border so the router can drag it.
func (w *ws) renderPaneBarLocked(b *bytes.Buffer, id string, r layout.Rect, focused bool, barBorder map[[2]int]layout.Border) {
	if r.W < 6 || r.H < 2 {
		return
	}
	p := w.panes[id]
	style, stroke := thFrame, "─"
	if focused {
		style = thFocus
	}
	if p != nil && p.isDead() {
		style = thDead
	}
	if w.hover.bars[id] {
		style, stroke = thHover, "━" // the boundary under the pointer
	}
	// Flanking junctions live in the neighboring border/ring columns.
	atTop := r.Y == w.area.Y
	left, right := "├", "┤"
	if atTop {
		left, right = "┬", "┬"
		if r.X-1 == 0 {
			left = "╭"
		}
		if r.X+r.W == w.cols-1 {
			right = "╮"
		}
	} else if r.X-1 == 0 {
		left = "├"
	}
	cup(b, r.Y, r.X-1)
	b.WriteString(style + left)

	title := ""
	if p != nil {
		title = sanitizeLabel(p.term.TitleSnapshot())
	}
	if title == "" {
		title = "shell"
	}
	if p != nil && p.isDead() {
		title += " (exited)"
	}
	const menu = "[≡]"
	maxTitle := r.W - 4 - runewidth.StringWidth(menu) // stroke + " " + title + " " + fill + menu + stroke
	title = runewidth.Truncate(title, max(maxTitle, 1), "…")
	tw := runewidth.StringWidth(title)
	fill := r.W - 4 - tw - runewidth.StringWidth(menu)
	fmt.Fprintf(b, "%s %s %s%s%s", stroke, title, strings.Repeat(stroke, max(fill, 0)), menu, stroke)
	b.WriteString(right + sgrReset)

	bd, hasBorder := barBorder[[2]int{r.X, r.Y}]
	w.hits = append(w.hits,
		hitRegion{rect: layout.Rect{X: r.X, Y: r.Y, W: r.W, H: 1}, kind: hitPaneBar, pane: id, border: bd, hasBorder: hasBorder},
		hitRegion{rect: layout.Rect{X: r.X + r.W - 4, Y: r.Y, W: 4, H: 1}, kind: hitPaneMenu, pane: id},
	)
}

// renderHoverLocked overdraws the hovered boundary strips with heavy
// bright strokes (bars highlight via their own style; this covers vertical
// borders and ring segments). Corner hovers carry several strips — the
// preview of what a corner gesture affects.
func (w *ws) renderHoverLocked(b *bytes.Buffer) {
	if len(w.hover.strips) == 0 {
		return
	}
	b.WriteString(thHover)
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
			b.WriteString(thAccentBar + tag + sgrReset)
		}
	}
}

// renderOverlayLocked draws the popup and records its hit regions last, so
// it owns every cell it covers.
func (w *ws) renderOverlayLocked(b *bytes.Buffer) {
	o := w.overlay
	wd := runewidth.StringWidth(o.title)
	for _, it := range o.items {
		if l := runewidth.StringWidth(it.label); l > wd {
			wd = l
		}
	}
	wd += 4                      // "│ " + " │"
	wd = clampInt(wd, 6, w.cols) // pad() truncates content to fit
	ht := len(o.items) + 2
	if o.title != "" {
		ht++
	}
	ht = clampInt(ht, 3, w.rows-1)
	x := clampInt(o.x, 0, w.cols-wd)
	y := clampInt(o.y, 1, w.rows-ht)

	row := y
	cup(b, row, x)
	b.WriteString(thMenu + "╭" + strings.Repeat("─", wd-2) + "╮")
	row++
	if o.title != "" {
		cup(b, row, x)
		b.WriteString(thMenuDim)
		fmt.Fprintf(b, "│ %s │", pad(o.title, wd-4))
		b.WriteString(thMenu)
		row++
	}
	itemsStart := len(w.hits)
	for i, it := range o.items {
		if row >= y+ht-1 || row >= w.rows-1 {
			break // clipped by a tiny screen; unreachable items stay unclickable
		}
		cup(b, row, x)
		style := thMenu
		if !it.enabled {
			style = thMenuDim
		}
		if i == w.hover.menuItem && it.enabled && strings.HasPrefix(w.hover.key, "mi:") {
			style = thMenuHover
		}
		b.WriteString(style)
		fmt.Fprintf(b, "│ %s │", pad(it.label, wd-4))
		w.hits = append(w.hits, hitRegion{rect: layout.Rect{X: x, Y: row, W: wd, H: 1}, kind: hitMenuItem, item: i})
		row++
	}
	cup(b, clampInt(row, 1, w.rows-1), x)
	b.WriteString(thMenu + "╰" + strings.Repeat("─", wd-2) + "╯" + sgrReset)
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
	b.WriteString("\x1b[?25h")
}

// detachClientLocked sends the polite detach frame; the client exits and
// closes the conn, which triggers normal cleanup.
func (w *ws) detachClientLocked(conn *protocol.Conn) {
	w.sendToLocked(conn, protocol.Message{Type: protocol.TypeDetached})
}
