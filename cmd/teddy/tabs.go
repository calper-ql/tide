package main

import (
	"os"
	"path/filepath"

	"github.com/calper-ql/tide/internal/input"
	"github.com/calper-ql/tide/internal/tui"
)

// tabHit records a drawn tab's screen extent for hit-testing: [x0,x1) is the
// whole tab, closeX the column of its close/dirty glyph.
type tabHit struct {
	idx, x0, x1, closeX int
}

func (a *App) activeDoc() *doc {
	if a.active >= 0 && a.active < len(a.tabs) {
		return a.tabs[a.active]
	}
	return nil
}

// openFile focuses an already-open tab for path, or opens a new one. Paths
// are absolutized so the same file never opens twice under different spellings.
func (a *App) openFile(path string) error {
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	a.tabPinActive = true
	for i, d := range a.tabs {
		if d.path == path {
			a.active = i
			return nil
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		data = nil // opening a not-yet-created file
	}
	a.tabs = append(a.tabs, newDoc(path, data))
	a.active = len(a.tabs) - 1
	return nil
}

func (a *App) closeTab(i int) {
	if i < 0 || i >= len(a.tabs) {
		return
	}
	a.tabs = append(a.tabs[:i], a.tabs[i+1:]...)
	if i < a.active {
		a.active--
	}
	a.active = clampInt(a.active, 0, max(len(a.tabs)-1, 0))
	a.tabPinActive = true
}

// closeAllTabs closes every tab at once.
func (a *App) closeAllTabs() {
	a.tabs = nil
	a.active = 0
	a.tabFirst = 0
	a.tabPinActive = true
}

// moveTab relocates the tab at from to index to (the drag-reorder primitive).
func (a *App) moveTab(from, to int) {
	if from == to || from < 0 || to < 0 || from >= len(a.tabs) || to >= len(a.tabs) {
		return
	}
	d := a.tabs[from]
	a.tabs = append(a.tabs[:from], a.tabs[from+1:]...)
	a.tabs = append(a.tabs, nil)
	copy(a.tabs[to+1:], a.tabs[to:])
	a.tabs[to] = d
}

// tabLabels names each tab by basename, disambiguating collisions with the
// parent directory ("pkg/main.go" vs "cmd/main.go").
func tabLabels(tabs []*doc) []string {
	base := make([]string, len(tabs))
	count := map[string]int{}
	for i, d := range tabs {
		base[i] = filepath.Base(d.path)
		count[base[i]]++
	}
	out := make([]string, len(tabs))
	for i, d := range tabs {
		if count[base[i]] > 1 {
			out[i] = filepath.Base(filepath.Dir(d.path)) + "/" + base[i]
		} else {
			out[i] = base[i]
		}
	}
	return out
}

func (a *App) drawTabStrip(buf *tui.Buffer, r tui.Rect) {
	a.tabHits = a.tabHits[:0]
	if len(a.tabs) == 0 || r.W <= 0 {
		return
	}
	labels := tabLabels(a.tabs)
	segs := make([]string, len(a.tabs))
	widths := make([]int, len(a.tabs))
	for i, d := range a.tabs {
		mark := "✕"
		if d.modified() {
			mark = "●"
		}
		segs[i] = " " + labels[i] + " " + mark + " "
		widths[i] = strWidth(segs[i]) + 1 // + separator
	}

	// Cap the scroll so the trailing tabs always fill the strip: tabMaxFirst is
	// the smallest start index whose suffix still fits, so you can't scroll
	// past the last tab into empty space.
	maxFirst, sum := 0, 0
	for f := len(a.tabs) - 1; f >= 0; f-- {
		sum += widths[f]
		if sum > r.W {
			maxFirst = f + 1
			break
		}
	}
	a.tabMaxFirst = min(maxFirst, len(a.tabs)-1)
	a.tabFirst = clampInt(a.tabFirst, 0, a.tabMaxFirst)

	// Snap the strip to reveal the active tab only when something just made it
	// active; otherwise honor the manual wheel scroll.
	if a.tabPinActive {
		if a.active < a.tabFirst {
			a.tabFirst = a.active
		}
		for a.tabFirst < a.active {
			w := 0
			for i := a.tabFirst; i <= a.active; i++ {
				w += widths[i]
			}
			if w <= r.W {
				break
			}
			a.tabFirst++
		}
		a.tabFirst = clampInt(a.tabFirst, 0, a.tabMaxFirst)
		a.tabPinActive = false
	}

	x := r.X
	for i := a.tabFirst; i < len(a.tabs); i++ {
		if x >= r.X+r.W {
			break
		}
		st := stTab
		if i == a.active {
			st = stTabActive
		}
		x0 := x
		end := drawIn(buf, r, x-r.X, 0, st, segs[i])
		closeX := end - 2 // seg ends " …<mark> ": mark sits one before the trailing space
		if a.tabs[i].modified() && closeX >= x0 && closeX < r.X+r.W {
			buf.Set(closeX, r.Y, '●', stDirty) // tint the dirty marker
		}
		a.tabHits = append(a.tabHits, tabHit{idx: i, x0: x0, x1: end, closeX: closeX})
		x = end
		if x < r.X+r.W {
			buf.Set(x, r.Y, '│', stBorder)
			x++
		}
	}
}

// --- tab mouse interactions ---

// tabAtDrag is the reorder target for pointer column x: the tab under it, or
// the nearest end so a tab can be dragged past the strip edges.
func (a *App) tabAtDrag(x int) int {
	if len(a.tabHits) == 0 {
		return -1
	}
	for _, h := range a.tabHits {
		if x >= h.x0 && x < h.x1 {
			return h.idx
		}
	}
	if x < a.tabHits[0].x0 {
		return a.tabHits[0].idx
	}
	return a.tabHits[len(a.tabHits)-1].idx
}

func (a *App) pressTab(x int) {
	for _, h := range a.tabHits {
		if x >= h.x0 && x < h.x1 {
			if x == h.closeX {
				a.pressClose = h.idx
				return
			}
			a.active = h.idx
			a.tabPinActive = true
			a.dragFrom = h.idx
			a.dragMoved = false
			return
		}
	}
}

// dragTab live-reorders as the pointer crosses tab boundaries — the standalone
// in-strip drag. (Dragging out of the strip is delivered through tide in T2.)
func (a *App) dragTab(x int) {
	target := a.tabAtDrag(x)
	if target >= 0 && target != a.dragFrom {
		a.moveTab(a.dragFrom, target)
		a.dragFrom = target
		a.active = target
		a.tabPinActive = true
		a.dragMoved = true
	}
}

func (a *App) releaseTab(ev input.Event) {
	if a.pressClose >= 0 {
		for _, h := range a.tabHits {
			if h.idx == a.pressClose && ev.X == h.closeX && a.last.tabs.Contains(ev.X, ev.Y) {
				a.closeTab(a.pressClose)
				break
			}
		}
		a.pressClose = -1
	}
	a.dragFrom = -1
	a.dragMoved = false
}
