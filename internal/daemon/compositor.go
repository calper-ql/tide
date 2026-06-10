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
	hitPane
	hitBorder
	hitMenuItem
	hitOverlayBody // inside an overlay but not on an item: swallow the click
)

type hitRegion struct {
	rect   layout.Rect
	kind   hitKind
	tab    int
	pane   string
	border layout.Border
	item   int
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

const (
	sgrReset  = "\x1b[0m"
	sgrBar    = "\x1b[0;7m"   // bar: reverse video
	sgrActive = "\x1b[0;7;1m" // active tab: reverse + bold
	sgrDim    = "\x1b[0;2m"
	sgrMenu   = "\x1b[0;48;5;236;38;5;252m" // popup body
	sgrMenuDi = "\x1b[0;48;5;236;38;5;243m" // disabled item
)

func cup(b *bytes.Buffer, y, x int) {
	fmt.Fprintf(b, "\x1b[%d;%dH", y+1, x+1)
}

// renderLocked composites one frame: dirty panes (or everything), the bar,
// borders, overlay, cursor. It rebuilds the hitmap as it draws.
func (w *ws) renderLocked() []byte {
	// The minimum must exceed the bar's right-side reservation (the detach
	// button), or its CUP column math goes negative and wraps into panes.
	if w.cols < 16 || w.rows < 3 {
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
		for id, r := range w.rects {
			w.hits = append(w.hits, hitRegion{rect: r, kind: hitPane, pane: id})
			if full || w.dirtyPanes[id] {
				w.renderPaneLocked(&b, id, r)
			}
		}
		if full {
			w.renderBordersLocked(&b)
		}
		for _, bd := range w.borders {
			w.hits = append(w.hits, hitRegion{rect: bd.Rect, kind: hitBorder, border: bd})
		}
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
	b.WriteString(sgrBar)
	b.WriteString(strings.Repeat(" ", w.cols)) // paint the row, then place segments
	cup(b, 0, 0)

	base := w.root
	if i := strings.LastIndexByte(base, '/'); i >= 0 && i < len(base)-1 {
		base = base[i+1:]
	}
	col := w.barSeg(b, 0, " "+runewidth.Truncate(base, 24, "…")+" ", sgrBar, hitNone, 0)
	col = w.barSeg(b, col, "▏", sgrBar, hitNone, 0)

	for i, tab := range w.lay.Tabs {
		style := sgrBar
		if i == w.lay.Active {
			style = sgrActive
		}
		col = w.barSeg(b, col, " "+fmt.Sprintf("%d:%s", i+1, w.tabTitleLocked(tab))+" ", style, hitTabLabel, i)
	}
	col = w.barSeg(b, col, " + ", sgrBar, hitNewTab, 0)

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
			b.WriteString(sgrBar + status)
		}
	}
	cup(b, 0, w.cols-dw)
	b.WriteString(sgrActive + detach + sgrReset)
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

// renderPaneLocked draws one pane's viewport, applying the scroll offset
// and inverting the selection.
func (w *ws) renderPaneLocked(b *bytes.Buffer, id string, r layout.Rect) {
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
			b.WriteString(sgrActive + tag + sgrReset)
		}
	}
}

func (w *ws) renderBordersLocked(b *bytes.Buffer) {
	b.WriteString(sgrDim)
	for _, bd := range w.borders {
		if bd.Vertical {
			for y := 0; y < bd.Rect.H; y++ {
				cup(b, bd.Rect.Y+y, bd.Rect.X)
				b.WriteString("│")
			}
		} else {
			cup(b, bd.Rect.Y, bd.Rect.X)
			b.WriteString(strings.Repeat("─", bd.Rect.W))
		}
	}
	b.WriteString(sgrReset)
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
	b.WriteString(sgrMenu + "┌" + strings.Repeat("─", wd-2) + "┐")
	row++
	if o.title != "" {
		cup(b, row, x)
		fmt.Fprintf(b, "│ %s │", pad(o.title, wd-4))
		row++
	}
	itemsStart := len(w.hits)
	for i, it := range o.items {
		if row >= y+ht-1 || row >= w.rows-1 {
			break // clipped by a tiny screen; unreachable items stay unclickable
		}
		cup(b, row, x)
		style := sgrMenu
		if !it.enabled {
			style = sgrMenuDi
		}
		b.WriteString(style)
		fmt.Fprintf(b, "│ %s │", pad(it.label, wd-4))
		w.hits = append(w.hits, hitRegion{rect: layout.Rect{X: x, Y: row, W: wd, H: 1}, kind: hitMenuItem, item: i})
		row++
	}
	cup(b, clampInt(row, 1, w.rows-1), x)
	b.WriteString(sgrMenu + "└" + strings.Repeat("─", wd-2) + "┘" + sgrReset)
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
	x, y, visible := p.term.CursorState()
	if !visible || x >= r.W || y >= r.H {
		return
	}
	cup(b, r.Y+y, r.X+x)
	b.WriteString("\x1b[?25h")
}

// detachClientLocked sends the polite detach frame; the client exits and
// closes the conn, which triggers normal cleanup.
func (w *ws) detachClientLocked(conn *protocol.Conn) {
	w.sendToLocked(conn, protocol.Message{Type: protocol.TypeDetached})
}
