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
	minSideWidth     = 14
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

	openArg string // file named on the command line, opened at startup (editor task)

	last regions // geometry from the last render, for mouse hit-testing
}

func newApp(scr *tui.Screen, root string) *App {
	return &App{screen: scr, root: root, sideWidth: defaultSideWidth}
}

// Run drives the event loop until quit, coalescing bursts of events into one
// render so mouse-motion and paste floods don't repaint per byte.
func (a *App) Run() error {
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
		}
	}
}

func (a *App) handleKey(ev input.Event) {
	if ev.Mods&input.Ctrl != 0 && ev.Key == input.KeyRune {
		switch ev.Rune {
		case 'q':
			a.quit = true
		case 'b':
			a.sideCollapsed = !a.sideCollapsed
		}
	}
}

func (a *App) handleMouse(ev input.Event) {
	if ev.Mouse != input.MousePress || ev.Button != 1 {
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
	}
}

func (a *App) render() {
	buf := a.screen.Back()
	cols, rows := a.screen.Size()
	buf.Clear(stText)

	r := computeLayout(cols, rows, a.sideWidth, a.sideCollapsed)
	a.last = r

	a.drawActivityBar(buf, r.activity)
	if !a.sideCollapsed {
		a.drawSidePanel(buf, r.side)
	}
	a.drawTabStrip(buf, r.tabs)
	a.drawEditor(buf, r.editor)
	a.drawStatusBar(buf, r.status)

	// Nothing owns a text cursor yet (the editor will); keep it hidden.
	a.screen.HideCursor()
	_ = a.screen.Flush()
}
