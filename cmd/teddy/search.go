package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/calper-ql/tide/internal/input"
	"github.com/calper-ql/tide/internal/tui"
)

// searchState is the Search activity's UI + query state.
type searchState struct {
	query     string
	results   []searchResult
	running   bool
	truncated bool
	focused   bool // the query box has keyboard focus

	top int // first visible result
	sel int // selected result

	inputHit tui.Rect // query box, for hit-testing
	contentY int      // screen y of the first result row
}

// searchActive reports whether keystrokes and the cursor belong to the search
// box (Search selected, focused, and the sidebar visible).
func (a *App) searchActive() bool {
	return a.selected == 1 && a.search.focused && !a.sideCollapsed
}

// startSearch cancels any in-flight search and launches a new one for query.
// An empty query just clears results. Results return asynchronously on
// a.searchCh, tagged with a.searchSeq so stale ones are ignored.
func (a *App) startSearch(query string) {
	if a.searchCancel != nil {
		a.searchCancel()
		a.searchCancel = nil
	}
	a.search.query = query
	a.search.results = nil
	a.search.truncated = false
	a.search.top, a.search.sel = 0, 0
	if strings.TrimSpace(query) == "" {
		a.search.running = false
		return
	}
	a.searchSeq++
	seq := a.searchSeq
	a.search.running = true
	ctx, cancel := context.WithCancel(context.Background())
	a.searchCancel = cancel
	go runSearch(ctx, a.root, query, seq, a.searchCh)
}

// applySearch adopts a completed search if it is still the current one.
func (a *App) applySearch(msg searchMsg) {
	if msg.seq != a.searchSeq {
		return // superseded
	}
	a.search.results = msg.results
	a.search.truncated = msg.truncated
	a.search.running = false
	a.search.top, a.search.sel = 0, 0
}

// handleSearchKey routes a key to the focused query box: typing edits the
// query and re-searches live, arrows move the selection, Enter opens the
// selected match (or runs the search when there are none), Esc unfocuses.
func (a *App) handleSearchKey(ev input.Event) {
	switch ev.Key {
	case input.KeyRune:
		if ev.Mods&input.Ctrl != 0 {
			return
		}
		a.startSearch(a.search.query + string(ev.Rune))
	case input.KeyBackspace:
		if r := []rune(a.search.query); len(r) > 0 {
			a.startSearch(string(r[:len(r)-1]))
		}
	case input.KeyEnter:
		if len(a.search.results) > 0 {
			a.openSearchResult(a.search.sel)
		} else {
			a.startSearch(a.search.query)
		}
	case input.KeyEscape:
		a.search.focused = false
	case input.KeyDown:
		a.search.sel = clampInt(a.search.sel+1, 0, max(len(a.search.results)-1, 0))
	case input.KeyUp:
		a.search.sel = clampInt(a.search.sel-1, 0, max(len(a.search.results)-1, 0))
	}
}

// openSearchResult opens the match's file and places the cursor on it,
// handing focus to the editor.
func (a *App) openSearchResult(idx int) {
	if idx < 0 || idx >= len(a.search.results) {
		return
	}
	a.search.sel = idx
	res := a.search.results[idx]
	if err := a.openFile(res.path); err != nil {
		return
	}
	if d := a.activeDoc(); d != nil {
		d.cy = clampInt(res.line-1, 0, len(d.lines)-1)
		d.cx = clampInt(res.col-1, 0, len(d.line()))
		d.top = max(d.cy-3, 0) // bring the match into view (viewport not known yet)
		d.breakUndo()
		d.setGoal()
	}
	a.search.focused = false
}

// clickSearch handles a click in the search panel: focus the query box, or
// open the result row under the pointer.
func (a *App) clickSearch(x, y int) {
	if a.search.inputHit.Contains(x, y) {
		a.search.focused = true
		return
	}
	if y >= a.search.contentY {
		if idx := a.search.top + (y - a.search.contentY); idx >= 0 && idx < len(a.search.results) {
			a.openSearchResult(idx)
			return
		}
	}
	a.search.focused = true
}

func (a *App) drawSearch(buf *tui.Buffer, inner tui.Rect) {
	// Query box on row 1 (row 0 is the panel title).
	box := tui.Rect{X: inner.X, Y: inner.Y + 1, W: inner.W, H: 1}
	a.search.inputHit = box
	ist := stStatusDim
	if a.searchActive() {
		ist = stStatus // brighter when focused
	}
	buf.Fill(box, ' ', ist)
	drawIn(buf, box, 0, 0, ist, "⚲ ")
	shown := a.search.query
	if maxQ := box.W - 3; maxQ > 0 && strWidth(shown) > maxQ {
		shown = string([]rune(shown)[strLen(shown)-maxQ:]) // keep the tail near the cursor
	}
	qx := drawIn(buf, box, 2, 0, ist, shown)
	if a.searchActive() {
		a.screen.SetCursor(min(qx, box.X+box.W-1), box.Y)
		a.screen.ShowCursor()
	}

	// Summary on row 2.
	var summary string
	switch {
	case a.search.running:
		summary = "searching…"
	case strings.TrimSpace(a.search.query) == "":
		summary = "type to search"
	case len(a.search.results) == 0:
		summary = "no matches"
	default:
		summary = fmt.Sprintf("%d matches", len(a.search.results))
		if a.search.truncated {
			summary += " (capped)"
		}
	}
	drawIn(buf, inner, 0, 2, stDim, summary)

	// Results from row 3.
	const top = 3
	rows := inner.H - top
	if rows < 1 {
		return
	}
	a.search.contentY = inner.Y + top
	if a.search.sel < a.search.top {
		a.search.top = a.search.sel
	}
	if a.search.sel >= a.search.top+rows {
		a.search.top = a.search.sel - rows + 1
	}
	a.search.top = clampInt(a.search.top, 0, max(len(a.search.results)-rows, 0))

	for i := 0; i < rows; i++ {
		ri := a.search.top + i
		if ri >= len(a.search.results) {
			break
		}
		res := a.search.results[ri]
		y := top + i
		locSt, txtSt := stDir, stText
		if ri == a.search.sel {
			buf.Fill(tui.Rect{X: inner.X, Y: inner.Y + y, W: inner.W, H: 1}, ' ', stSelected)
			locSt, txtSt = stSelected, stSelected
		}
		loc := fmt.Sprintf("%s:%d ", filepath.Base(res.path), res.line)
		x := drawIn(buf, inner, 0, y, locSt, loc)
		drawIn(buf, inner, x-inner.X, y, txtSt, strings.TrimSpace(res.text))
	}
}

func strLen(s string) int { return len([]rune(s)) }
