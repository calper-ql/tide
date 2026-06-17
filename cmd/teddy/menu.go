package main

import "github.com/calper-ql/tide/internal/tui"

// menuItem is one action in the bottom-left actions menu: a label, its
// keyboard accelerator (shown so the chord stays discoverable), and what it
// does. Disabled items render dim and do nothing.
type menuItem struct {
	label   string
	hint    string
	enabled bool
	action  func(*App)
}

// menuActions builds the actions menu for the current state. The keyboard
// shortcuts that used to crowd the status bar live here now — always one
// click away, even with a file open (the status bar shows Ln/Col instead).
func (a *App) menuActions() []menuItem {
	d := a.activeDoc()
	items := []menuItem{
		{label: "Save", hint: "^S", enabled: d != nil && d.modified(), action: func(a *App) { a.saveActive() }},
		{label: "Reload from disk", hint: "^R", enabled: d != nil, action: func(a *App) { a.reloadActive() }},
	}
	if d != nil && isMarkdown(d.path) {
		label := "Show raw"
		if !d.preview {
			label = "Show markdown"
		}
		items = append(items, menuItem{label: label, hint: "^E", enabled: true, action: (*App).togglePreview})
	}
	panel := "Hide sidebar"
	if a.sideCollapsed {
		panel = "Show sidebar"
	}
	items = append(items,
		menuItem{label: panel, hint: "^B", enabled: true, action: func(a *App) { a.sideCollapsed = !a.sideCollapsed }},
		menuItem{label: "Close all tabs", enabled: len(a.tabs) > 0, action: func(a *App) { a.closeAllTabs() }},
		menuItem{label: "Quit", hint: "^Q", enabled: true, action: func(a *App) { a.quit = true }},
	)
	return items
}

// drawMenu paints the actions menu as a popup above the teddy pill, growing
// upward from the status bar, and records each row's hit rect.
func (a *App) drawMenu(buf *tui.Buffer, cols, rows int) {
	a.menuItems = a.menuActions()
	a.menuRects = a.menuRects[:0]

	contentW := 10
	for _, it := range a.menuItems {
		contentW = max(contentW, strWidth(it.label)+2+strWidth(it.hint))
	}
	boxW := min(contentW+4, cols)
	boxH := len(a.menuItems) + 2
	x := 0
	y := max((rows-1)-boxH, 0) // sit just above the status bar

	drawBox(buf, x, y, boxW, boxH, stMenu)
	for i, it := range a.menuItems {
		row := tui.Rect{X: x + 1, Y: y + 1 + i, W: boxW - 2, H: 1}
		st := stMenu
		if !it.enabled {
			st = stMenuDim
		}
		buf.Fill(row, ' ', st)
		drawIn(buf, row, 1, 0, st, it.label)
		drawIn(buf, row, row.W-strWidth(it.hint)-1, 0, stMenuHint, it.hint)
		a.menuRects = append(a.menuRects, row)
	}
}

// clickMenu dispatches a click while the menu is open: run the hit item (if
// enabled), otherwise just dismiss. Either way the menu closes.
func (a *App) clickMenu(x, y int) {
	for i, rect := range a.menuRects {
		if rect.Contains(x, y) {
			if it := a.menuItems[i]; it.enabled && it.action != nil {
				it.action(a)
			}
			break
		}
	}
	a.menuOpen = false
}

// drawBox draws a rounded popup box filled in st.
func drawBox(buf *tui.Buffer, x, y, w, h int, st tui.Style) {
	if w < 2 || h < 2 {
		return
	}
	buf.Fill(tui.Rect{X: x, Y: y, W: w, H: h}, ' ', st)
	buf.Set(x, y, '╭', st)
	buf.Set(x+w-1, y, '╮', st)
	buf.Set(x, y+h-1, '╰', st)
	buf.Set(x+w-1, y+h-1, '╯', st)
	for i := x + 1; i < x+w-1; i++ {
		buf.Set(i, y, '─', st)
		buf.Set(i, y+h-1, '─', st)
	}
	for j := y + 1; j < y+h-1; j++ {
		buf.Set(x, j, '│', st)
		buf.Set(x+w-1, j, '│', st)
	}
}
