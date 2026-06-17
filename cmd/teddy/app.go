package main

import (
	"github.com/calper-ql/tide/internal/input"
	"github.com/calper-ql/tide/internal/tui"
)

// activityW is the fixed width of the far-left activity bar (VS Code's
// vertical icon strip). defaultSideWidth is the explorer panel's width.
const (
	activityW        = 3
	defaultSideWidth = 28
)

// activity is one button in the activity bar. Only Explorer is wired in T1;
// Search and Git render but are inert (the spec: "don't need all of them
// immediately").
type activity struct {
	icon    string
	title   string
	enabled bool
}

var activities = []activity{
	{"▤", "EXPLORER", true},
	{"⌕", "SEARCH", false},
	{"⎇", "SOURCE CONTROL", false},
}

// regions is the computed geometry of teddy's panels for one frame.
type regions struct {
	activity tui.Rect // far-left icon strip
	side     tui.Rect // explorer/search/git panel (zero width when collapsed)
	tabs     tui.Rect // editor tab strip (one row)
	editor   tui.Rect // editor/viewer content
	status   tui.Rect // bottom status line (full width)
}

// computeLayout lays out the panels for a cols×rows terminal. It degrades
// gracefully on tiny terminals (never negative sizes) so the renderer's
// bounds checks do the rest.
func computeLayout(cols, rows, sideWidth int, collapsed bool) regions {
	cols, rows = max(cols, 1), max(rows, 1)
	statusY := rows - 1
	workH := max(rows-1, 1) // rows above the status line

	aw := min(activityW, cols)
	sw := 0
	if !collapsed {
		sw = sideWidth
	}
	// Always leave at least one column for the editor.
	if aw+sw > cols-1 {
		sw = max(cols-1-aw, 0)
	}
	ex := aw + sw
	ew := max(cols-ex, 0)

	return regions{
		activity: tui.Rect{X: 0, Y: 0, W: aw, H: workH},
		side:     tui.Rect{X: aw, Y: 0, W: sw, H: workH},
		tabs:     tui.Rect{X: ex, Y: 0, W: ew, H: 1},
		editor:   tui.Rect{X: ex, Y: 1, W: ew, H: max(workH-1, 0)},
		status:   tui.Rect{X: 0, Y: statusY, W: cols, H: 1},
	}
}

// App is teddy's whole UI state.
type App struct {
	screen *tui.Screen
	root   string
	quit   bool

	selected      int // index into activities
	sideCollapsed bool
	sideWidth     int

	openArg string // file named on the command line, opened at startup

	tabs    []*doc // open documents, one per tab
	active  int    // index of the focused tab (invalid when no tabs)
	browser *browser

	tabFirst   int      // first tab drawn (strip scroll)
	tabHits    []tabHit // drawn tab extents, for hit-testing
	dragFrom   int      // tab being dragged, or -1
	dragMoved  bool
	pressClose int // tab whose close glyph was pressed, or -1

	last regions // geometry from the last render, for mouse hit-testing
}

func newApp(scr *tui.Screen, root string) *App {
	return &App{
		screen: scr, root: root, sideWidth: defaultSideWidth,
		browser:  newBrowser(root),
		dragFrom: -1, pressClose: -1,
	}
}

// Run drives the event loop until quit, coalescing bursts of events into one
// render so mouse-motion and paste floods don't repaint per byte.
func (a *App) Run() error {
	if a.openArg != "" {
		_ = a.openFile(a.openArg)
	}
	events := a.screen.Events()
	a.render()
	for !a.quit {
		a.handle(<-events)
		for draining := true; draining; {
			select {
			case ev := <-events:
				a.handle(ev)
			default:
				draining = false
			}
		}
		a.render()
	}
	return nil
}

func (a *App) handle(ev tui.Event) {
	switch {
	case ev.Closed:
		a.quit = true
	case ev.Resize:
		a.screen.Resize(ev.Cols, ev.Rows)
	default:
		switch ev.Input.Type {
		case input.EvKey:
			a.handleKey(ev.Input)
		case input.EvMouse:
			a.handleMouse(ev.Input)
		case input.EvPaste:
			if d := a.activeDoc(); d != nil {
				d.breakUndo()
				d.insertString(string(ev.Input.Paste))
				d.breakUndo()
				d.scrollToCursor()
			}
		}
	}
}

func (a *App) handleKey(ev input.Event) {
	d := a.activeDoc()
	if ev.Mods&input.Ctrl != 0 && ev.Key == input.KeyRune {
		switch ev.Rune {
		case 'q':
			a.quit = true
			return
		case 'b':
			a.sideCollapsed = !a.sideCollapsed
			return
		case 's':
			a.saveActive()
			return
		case 'z':
			if d != nil {
				d.Undo()
				d.scrollToCursor()
			}
			return
		case 'y':
			if d != nil {
				d.Redo()
				d.scrollToCursor()
			}
			return
		}
	}
	if d != nil {
		d.handleKey(ev)
		d.scrollToCursor()
	}
}

func (a *App) handleMouse(ev input.Event) {
	switch ev.Mouse {
	case input.MouseWheelUp:
		a.wheel(ev, -3)
		return
	case input.MouseWheelDown:
		a.wheel(ev, 3)
		return
	case input.MouseMotion:
		if a.dragFrom >= 0 {
			a.dragTab(ev.X)
		}
		return
	case input.MouseRelease:
		a.releaseTab(ev)
		return
	case input.MousePress:
		if ev.Button != 1 {
			return
		}
	default:
		return
	}

	// A fresh left-press: clear any stale tab-drag arming.
	a.dragFrom, a.pressClose = -1, -1

	if a.last.tabs.Contains(ev.X, ev.Y) {
		a.pressTab(ev.X)
		return
	}
	// Activity bar: each icon sits on its own row at the top of the strip.
	if a.last.activity.Contains(ev.X, ev.Y) {
		idx := ev.Y - a.last.activity.Y
		if idx >= 0 && idx < len(activities) && activities[idx].enabled {
			if idx == a.selected && !a.sideCollapsed {
				a.sideCollapsed = true // click the active icon to collapse
			} else {
				a.selected = idx
				a.sideCollapsed = false
			}
		}
		return
	}
	if a.selected == 0 && !a.sideCollapsed && a.last.side.Contains(ev.X, ev.Y) {
		a.clickBrowser(ev.Y)
		return
	}
	if d := a.activeDoc(); d != nil && a.last.editor.Contains(ev.X, ev.Y) {
		a.clickEditor(d, ev.X, ev.Y)
	}
}

// wheel scrolls whichever panel the pointer is over.
func (a *App) wheel(ev input.Event, delta int) {
	if a.selected == 0 && !a.sideCollapsed && a.last.side.Contains(ev.X, ev.Y) {
		a.browser.scroll(delta)
		return
	}
	if d := a.activeDoc(); d != nil && a.last.editor.Contains(ev.X, ev.Y) {
		d.top = clampInt(d.top+delta, 0, max(len(d.lines)-1, 0))
	}
}

// clickEditor places the cursor at the clicked cell, mapping through the
// gutter, scroll offsets, and tab/wide-rune expansion.
func (a *App) clickEditor(d *doc, x, y int) {
	r := a.last.editor
	gw := gutterWidth(len(d.lines))
	d.cy = clampInt(d.top+(y-r.Y), 0, len(d.lines)-1)
	dc := max((x-r.X-gw)+d.left, 0)
	d.cx = colFromDisplay(d.line(), dc)
	d.breakUndo()
	d.setGoal()
}

func (a *App) render() {
	buf := a.screen.Back()
	cols, rows := a.screen.Size()
	buf.Clear(stText)

	r := computeLayout(cols, rows, a.sideWidth, a.sideCollapsed)
	a.last = r

	// Default to a hidden cursor; the editor shows it when a doc is focused.
	a.screen.HideCursor()
	a.drawActivityBar(buf, r.activity)
	if !a.sideCollapsed {
		a.drawSidePanel(buf, r.side)
	}
	a.drawTabStrip(buf, r.tabs)
	a.drawEditor(buf, r.editor)
	a.drawStatusBar(buf, r.status)
	_ = a.screen.Flush()
}
