package main

import (
	"fmt"

	"github.com/mattn/go-runewidth"

	"github.com/calper-ql/tide/internal/tui"
)

// drawIn draws s into rect at offset (dx,dy) from the rect's top-left,
// clipped to the rect's right edge and to the single row. It returns the
// column (absolute) just past the last rune drawn.
func drawIn(buf *tui.Buffer, rect tui.Rect, dx, dy int, st tui.Style, s string) int {
	x, y := rect.X+dx, rect.Y+dy
	if y < rect.Y || y >= rect.Y+rect.H {
		return x
	}
	maxX := rect.X + rect.W
	for _, r := range s {
		if x >= maxX {
			break
		}
		x += buf.Set(x, y, r, st)
	}
	return x
}

func strWidth(s string) int { return runewidth.StringWidth(s) }

// shortenPath trims a path to width w with a leading ellipsis, keeping the
// tail (the part that disambiguates).
func shortenPath(p string, w int) string {
	if strWidth(p) <= w || w <= 1 {
		return p
	}
	r := []rune(p)
	for len(r) > 0 && strWidth("…"+string(r)) > w {
		r = r[1:]
	}
	return "…" + string(r)
}

func (a *App) drawActivityBar(buf *tui.Buffer, r tui.Rect) {
	// A separator down the right edge divides the buttons from the sidebar
	// (and from the editor when the sidebar is collapsed).
	for y := r.Y; y < r.Y+r.H; y++ {
		buf.Set(r.X+r.W-1, y, '│', stBorder)
	}
	cx := r.X + (r.W-1)/2
	for i, act := range activities {
		y := r.Y + i
		if y >= r.Y+r.H {
			break
		}
		st := stDim
		selected := i == a.selected && !a.sideCollapsed
		if selected {
			st = stAccent
			buf.Set(r.X, y, '▎', stAccent) // VS Code's active-icon left bar
		}
		buf.Set(cx, y, []rune(act.icon)[0], st)
	}
}

func (a *App) drawSidePanel(buf *tui.Buffer, r tui.Rect) {
	if r.W <= 0 {
		return
	}
	// A faint right border separates the panel from the editor.
	for y := r.Y; y < r.Y+r.H; y++ {
		buf.Set(r.X+r.W-1, y, '│', stBorder)
	}
	inner := tui.Rect{X: r.X, Y: r.Y, W: r.W - 1, H: r.H}
	drawIn(buf, inner, 1, 0, stSideTitle, activities[a.selected].title)

	switch a.selected {
	case 0:
		a.drawBrowser(buf, inner)
	case 1:
		a.drawSearch(buf, inner)
	case 2:
		a.drawGit(buf, inner)
	}
}

// drawTabSeparator draws the rule dividing the tab strip from the editor
// content (the reserved row 1 of the editor column).
func (a *App) drawTabSeparator(buf *tui.Buffer, r regions) {
	y := r.tabs.Y + 1
	if y >= r.status.Y || r.tabs.W <= 0 {
		return
	}
	for x := r.tabs.X; x < r.tabs.X+r.tabs.W; x++ {
		buf.Set(x, y, '─', stBorder)
	}
}

func (a *App) drawStatusBar(buf *tui.Buffer, r tui.Rect) {
	buf.Fill(r, ' ', stStatus)
	// The teddy pill is the actions-menu button (▴: the menu opens upward).
	end := drawIn(buf, r, 0, 0, stAccentPill, " teddy ▴ ")
	a.teddyHit = tui.Rect{X: r.X, Y: r.Y, W: end - r.X, H: 1}
	x := drawIn(buf, r, end-r.X+1, 0, stStatusDim, shortenPath(a.root, max(r.W/3, 8)))

	// Clickable markdown viz/raw pill (Ctrl+E also toggles).
	a.mdToggle = tui.Rect{}
	if d := a.activeDoc(); d != nil && d.diff == nil && isMarkdown(d.path) {
		mode := "raw"
		if d.preview {
			mode = "viz"
		}
		px := x - r.X + 1
		mend := drawIn(buf, r, px, 0, stAccentPill, " md:"+mode+" ")
		a.mdToggle = tui.Rect{X: r.X + px, Y: r.Y, W: mend - (r.X + px), H: 1}
		x = mend
	}

	// Clickable diff layout pill (Ctrl+D also toggles), shown for diff tabs.
	a.diffToggle = tui.Rect{}
	if d := a.activeDoc(); d != nil && d.diff != nil {
		mode := "inline"
		if a.diffMode == diffSideBySide {
			mode = "side"
		}
		px := x - r.X + 1
		mend := drawIn(buf, r, px, 0, stAccentPill, " diff:"+mode+" ")
		a.diffToggle = tui.Rect{X: r.X + px, Y: r.Y, W: mend - (r.X + px), H: 1}
		x = mend
	}

	// Right side: cursor position + a dirty marker. The action hints moved
	// into the teddy menu, so they stay available with a file open.
	if d := a.activeDoc(); d != nil {
		mark := ""
		if d.modified() {
			mark = " ●"
		}
		right := fmt.Sprintf("Ln %d, Col %d%s ", d.cy+1, d.cx+1, mark)
		if hx := r.W - strWidth(right); hx > (x-r.X)+2 {
			drawIn(buf, r, hx, 0, stStatusDim, right)
		}
	}
}
