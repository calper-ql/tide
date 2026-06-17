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
	default:
		drawIn(buf, inner, 1, 2, stHint, "not yet")
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
	x := drawIn(buf, r, 0, 0, stAccentPill, " teddy ")
	root := shortenPath(a.root, max(r.W/3, 8))
	x = drawIn(buf, r, x-r.X+1, 0, stStatusDim, root)

	// A clickable viz/raw pill for markdown docs (mouse-first toggle; Ctrl+E
	// also works). The pill shows the current mode.
	a.mdToggle = tui.Rect{}
	if d := a.activeDoc(); d != nil && isMarkdown(d.path) {
		mode := "raw"
		if d.preview {
			mode = "viz"
		}
		px := x - r.X + 1
		end := drawIn(buf, r, px, 0, stAccentPill, " md:"+mode+" ")
		a.mdToggle = tui.Rect{X: r.X + px, Y: r.Y, W: end - (r.X + px), H: 1}
		x = end
	}

	// Right side: cursor position + dirty marker when a doc is open, else hints.
	right := "^S save  ^B panel  ^Q quit "
	if d := a.activeDoc(); d != nil {
		mark := ""
		if d.dirty {
			mark = " ●"
		}
		right = fmt.Sprintf("Ln %d, Col %d%s ", d.cy+1, d.cx+1, mark)
	}
	hx := r.W - strWidth(right)
	if hx > (x-r.X)+2 {
		drawIn(buf, r, hx, 0, stStatusDim, right)
	}
}
