package daemon

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/calper-ql/tide/internal/layout"
	"github.com/calper-ql/tide/internal/session"
)

// TestEdgeMenuDefaultUnderPointerClickClickSplits pins the click-click
// gesture: a border click opens the menu with the default item's row under
// the pointer and pre-lit, so a second click on the SAME cell runs it.
func TestEdgeMenuDefaultUnderPointerClickClickSplits(t *testing.T) {
	w, conn, s := newTestWS(t)
	s.waitFor(t, "first frame", func() bool { return s.contains("1:") })
	withWS(w, func() { w.actionSplitLocked(w.lay.FocusedPane(), layout.SplitRight) }) // L | R

	var bx, by int
	s.waitFor(t, "vertical border in hitmap", func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		for _, h := range w.hits {
			if h.kind == hitBorder {
				bx, by = h.rect.X, h.rect.Y+h.rect.H/2 // mid-span, clear of junctions
				return true
			}
		}
		return false
	})

	w.handleInput(conn, press(bx, by))
	w.handleInput(conn, release(bx, by))
	s.waitFor(t, "edge menu", func() bool { return s.contains("→ New pane right") })
	withWS(w, func() {
		o := w.overlay
		if o == nil {
			t.Fatal("no overlay after border click")
		}
		if o.sel < 0 || !strings.HasPrefix(o.items[o.sel].label, "→") {
			t.Fatalf("default item must be the border's own direction, sel=%d items=%+v", o.sel, o.items)
		}
		// The truth rule: the pre-lit item's row sits under the pointer.
		w.renderLocked()
		for _, h := range w.hits {
			if h.kind == hitMenuItem && h.item == o.sel {
				r := h.rect
				if by < r.Y || by >= r.Y+r.H || bx < r.X || bx >= r.X+r.W {
					t.Fatalf("default item rect %+v does not contain the click (%d,%d)", r, bx, by)
				}
			}
		}
	})

	// Second click on the same cell runs the default: a third pane appears.
	w.handleInput(conn, press(bx, by))
	w.handleInput(conn, release(bx, by))
	s.waitFor(t, "three panes after click-click", func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		return w.lay.CountPanes() == 3
	})
}

// TestMenuDisabledItemsVisibleWithReasons pins the fix for the invisible
// menu rows: disabled items must not render bright-black-on-bright-black
// (the "gaps"), and they say why they are disabled.
func TestMenuDisabledItemsVisibleWithReasons(t *testing.T) {
	w, conn, s := newTestWS(t)
	s.waitFor(t, "first frame", func() bool { return s.contains("1:") })

	x, y := hitCenter(t, w, hitPaneMenu)
	w.handleInput(conn, press(x, y))
	w.handleInput(conn, release(x, y))
	s.waitFor(t, "pane menu with reasons", func() bool {
		return s.contains("Copy — select text first") && s.contains("Close Pane — last pane")
	})
	if s.contains("\x1b[0;100;90m") {
		t.Fatal("menu still uses fg and bg from palette slot 8 (invisible rows)")
	}
	// Tide's disabled rows: slot-8 glyphs swapped onto the cyan card fill.
	s.waitFor(t, "readable dim style", func() bool { return s.contains("\x1b[0;7;36;100m") })
	// Borderless: no box-drawing gutters on the menu surface (the ╭╰ that
	// remain on screen belong to the pane frame, not the popup).
	if s.contains("│ Copy") || s.contains("\x1b[0;7;36m╭") {
		t.Fatal("menu still renders a box-drawing border")
	}
}

// TestArrowKeysMoveMenuSelectionAndEnterRuns pins keyboard navigation:
// Down skips the separator, Enter runs the highlighted item.
func TestArrowKeysMoveMenuSelectionAndEnterRuns(t *testing.T) {
	w, conn, s := newTestWS(t)
	s.waitFor(t, "first frame", func() bool { return s.contains("1:") })

	x, y := hitCenter(t, w, hitPaneSplit)
	w.handleInput(conn, press(x, y))
	w.handleInput(conn, release(x, y))
	s.waitFor(t, "split menu", func() bool { return s.contains("→ New pane right") })

	w.handleInput(conn, []byte("\x1b[B")) // Down: over the separator to "↑ New pane above"
	withWS(w, func() {
		o := w.overlay
		if o == nil || o.sel < 0 || !strings.HasPrefix(o.items[o.sel].label, "↑") {
			t.Fatalf("Down must land on the first item after the separator, overlay=%+v", o)
		}
	})
	w.handleInput(conn, []byte("\r")) // Enter runs it
	s.waitFor(t, "stacked split via keyboard", func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		root := w.lay.ActiveTab().Root
		return w.lay.CountPanes() == 2 && root.Dir == layout.SplitDown &&
			len(root.Children) == 2 && root.Children[0].Pane == w.lay.FocusedPane()
	})
}

// TestReleaseSlopKeepsJitteryClicksAndTravelCancels pins the 3×3 release
// slop on non-draggable elements: a 1-cell wobble still opens the menu, a
// real drag away does not.
func TestReleaseSlopKeepsJitteryClicksAndTravelCancels(t *testing.T) {
	w, conn, s := newTestWS(t)
	s.waitFor(t, "first frame", func() bool { return s.contains("1:") })

	w.handleInput(conn, press(0, 5))
	w.handleInput(conn, motion(0, 6)) // 1-cell jitter: still a click
	w.handleInput(conn, release(0, 6))
	s.waitFor(t, "edge menu despite jitter", func() bool { return s.contains("← New pane left") })
	w.handleInput(conn, []byte{0x1b}) // Esc
	time.Sleep(80 * time.Millisecond)

	w.handleInput(conn, press(0, 5))
	w.handleInput(conn, motion(0, 9)) // real travel: cancels the click
	w.handleInput(conn, release(0, 9))
	time.Sleep(100 * time.Millisecond)
	withWS(w, func() {
		if w.overlay != nil {
			t.Fatalf("travel past the slop must not open a menu; got %q", w.overlay.title)
		}
	})
}

// TestBottomRingMenuFlipsUpward pins the flip rule: at the bottom ring the
// menu grows upward with the primary direction moved to the last row —
// still directly under the pointer, still pre-lit.
func TestBottomRingMenuFlipsUpward(t *testing.T) {
	w, conn, s := newTestWS(t)
	s.waitFor(t, "first frame", func() bool { return s.contains("1:") })

	var x, y int
	withWS(w, func() { x, y = w.cols/2, w.rows-1 })
	w.handleInput(conn, press(x, y))
	w.handleInput(conn, release(x, y))
	s.waitFor(t, "edge menu", func() bool { return s.contains("↓ New pane below") })
	withWS(w, func() {
		o := w.overlay
		if o == nil {
			t.Fatal("no overlay after bottom ring click")
		}
		if o.sel != len(o.items)-1 || !strings.HasPrefix(o.items[o.sel].label, "↓") {
			t.Fatalf("flipped menu must pre-light the primary on its last row, sel=%d items=%+v", o.sel, o.items)
		}
		// The full flipped order: the other three directions in fixed
		// ↑ ← → order, then the separator still adjacent to the primary.
		var got []string
		for _, it := range o.items {
			if it.separator {
				got = append(got, "|")
			} else {
				got = append(got, string([]rune(it.label)[0]))
			}
		}
		if strings.Join(got, "") != "↑←→|↓" {
			t.Fatalf("flipped item order = %v, want ↑←→|↓", got)
		}
		w.renderLocked()
		for _, h := range w.hits {
			if h.kind == hitMenuItem && h.item == o.sel {
				if h.rect.Y != y {
					t.Fatalf("primary row at Y=%d, want under the pointer at Y=%d", h.rect.Y, y)
				}
			}
		}
	})
}

// TestTopBorderClickClickSplits pins the headless fallback: in the top
// rows of the pane area there is no room for the menu's title above the
// pointer and no downward flip — the card opens headless so the default
// still sits pre-lit under the pointer and click-click still splits.
func TestTopBorderClickClickSplits(t *testing.T) {
	w, conn, s := newTestWS(t)
	s.waitFor(t, "first frame", func() bool { return s.contains("1:") })
	withWS(w, func() { w.actionSplitLocked(w.lay.FocusedPane(), layout.SplitRight) }) // L | R

	var bx int
	s.waitFor(t, "vertical border in hitmap", func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		for _, h := range w.hits {
			if h.kind == hitBorder {
				bx = h.rect.X
				return true
			}
		}
		return false
	})
	by := 2 // first mid-border cell, directly under the pane bars
	w.handleInput(conn, press(bx, by))
	w.handleInput(conn, release(bx, by))
	s.waitFor(t, "edge menu", func() bool { return s.contains("→ New pane right") })
	withWS(w, func() {
		o := w.overlay
		if o == nil || o.title != "" {
			t.Fatalf("top-row menu must open headless, overlay=%+v", o)
		}
		if o.sel < 0 || !strings.HasPrefix(o.items[o.sel].label, "→") {
			t.Fatalf("default must be pre-lit under the pointer, sel=%d", o.sel)
		}
	})
	w.handleInput(conn, press(bx, by))
	w.handleInput(conn, release(bx, by))
	s.waitFor(t, "three panes after top-row click-click", func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		return w.lay.CountPanes() == 3
	})
}

// TestDeadPaneBarClickRestarts pins that the bar's "(exited) — click to
// restart" label tells the truth: a click on the dead pane's bar respawns
// its shell.
func TestDeadPaneBarClickRestarts(t *testing.T) {
	w, conn, s := newTestWS(t)
	s.waitFor(t, "first frame", func() bool { return s.contains("1:") })

	w.handleInput(conn, []byte("exit\r"))
	s.waitFor(t, "pane dead with restart hint", func() bool { return s.contains("click to restart") })

	var bx, by int
	withWS(w, func() {
		for _, h := range w.hits {
			if h.kind == hitPaneBar {
				bx, by = h.rect.X+2, h.rect.Y
			}
		}
	})
	w.handleInput(conn, press(bx, by))
	w.handleInput(conn, release(bx, by))
	s.waitFor(t, "shell respawned by bar click", func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		p := w.panes[w.lay.FocusedPane()]
		return p != nil && !p.isDead()
	})
}

// TestPaneMenuEnabledItemsHaveNoSuffix pins the other half of the reason
// rule: items that ARE runnable carry plain labels.
func TestPaneMenuEnabledItemsHaveNoSuffix(t *testing.T) {
	w, conn, s := newTestWS(t)
	s.waitFor(t, "first frame", func() bool { return s.contains("1:") })
	withWS(w, func() {
		w.actionSplitLocked(w.lay.FocusedPane(), layout.SplitRight) // 2 panes: Close enabled
		w.clip = []byte("x")                                        // Paste enabled
	})
	x, y := hitCenter(t, w, hitPaneMenu)
	w.handleInput(conn, press(x, y))
	w.handleInput(conn, release(x, y))
	s.waitFor(t, "pane menu", func() bool { return s.contains("Close Pane") })
	withWS(w, func() {
		for _, it := range w.overlay.items {
			if it.separator {
				continue
			}
			base, _, hasSuffix := strings.Cut(it.label, " — ")
			if it.enabled && hasSuffix {
				t.Fatalf("enabled item %q must not carry a reason suffix", it.label)
			}
			if !it.enabled && !hasSuffix {
				t.Fatalf("disabled item %q must say why", it.label)
			}
			_ = base
		}
		if i := firstEnabledIdx(w.overlay.items); i < 0 {
			t.Fatal("expected enabled items in this state")
		}
	})
}

// TestSlopBoundaryTwoCellsCancels pins the 3×3 slop edge: exactly 2 cells
// of travel is past the slop and must cancel the click's menu.
func TestSlopBoundaryTwoCellsCancels(t *testing.T) {
	w, conn, s := newTestWS(t)
	s.waitFor(t, "first frame", func() bool { return s.contains("1:") })

	w.handleInput(conn, press(0, 5))
	w.handleInput(conn, motion(0, 7)) // exactly 2 cells: outside the 3×3
	w.handleInput(conn, release(0, 7))
	time.Sleep(100 * time.Millisecond)
	withWS(w, func() {
		if w.overlay != nil {
			t.Fatalf("2-cell travel must cancel the menu; got %q", w.overlay.title)
		}
	})
}

// TestSessionMenuAnchorsBelowBar pins the dropdown anchor: the session
// menu opens under the bar at the project segment's column, not at the
// pointer cell inside the segment.
func TestSessionMenuAnchorsBelowBar(t *testing.T) {
	w, conn, s := newTestWS(t)
	s.waitFor(t, "first frame", func() bool { return s.contains("1:") })

	var rectX int
	withWS(w, func() {
		for _, h := range w.hits {
			if h.kind == hitSessionMenu {
				rectX = h.rect.X
			}
		}
	})
	sx, sy := hitCenter(t, w, hitSessionMenu)
	w.handleInput(conn, press(sx, sy))
	w.handleInput(conn, release(sx, sy))
	s.waitFor(t, "session menu", func() bool { return s.contains("New Tab") })
	withWS(w, func() {
		if w.overlay == nil || w.overlay.x != rectX || w.overlay.y != 1 {
			t.Fatalf("session menu at (%d,%d), want dropdown at (%d,1)", w.overlay.x, w.overlay.y, rectX)
		}
	})
}

// TestThemePresetInvariants enforces the theme contract programmatically:
// no style in any preset pairs a foreground and background from the same
// palette slot (the class of bug that rendered menu rows invisible).
func TestThemePresetInvariants(t *testing.T) {
	slot := func(code int) (int, bool) {
		switch {
		case code >= 30 && code <= 37:
			return code - 30, true
		case code >= 90 && code <= 97:
			return code - 90 + 8, true
		}
		return 0, false
	}
	bgSlot := func(code int) (int, bool) {
		switch {
		case code >= 40 && code <= 47:
			return code - 40, true
		case code >= 100 && code <= 107:
			return code - 100 + 8, true
		}
		return 0, false
	}
	for _, th := range themes {
		styles := map[string]string{
			"bar": th.bar, "accentBar": th.accentBar, "barHover": th.barHover,
			"frame": th.frame, "focus": th.focus, "hover": th.hover,
			"dead": th.dead, "flash": th.flash,
			"menu": th.menu, "menuTitle": th.menuTitle, "menuDim": th.menuDim,
			"menuHover": th.menuHover, "menuDanger": th.menuDanger,
		}
		for role, sgr := range styles {
			if sgr == "" {
				t.Fatalf("%s.%s is empty", th.name, role)
			}
			if !strings.HasPrefix(sgr, "\x1b[0") {
				t.Fatalf("%s.%s = %q must self-reset", th.name, role, sgr)
			}
			body := strings.TrimSuffix(strings.TrimPrefix(sgr, "\x1b["), "m")
			fg, bg := -1, -1
			for _, part := range strings.Split(body, ";") {
				var code int
				if _, err := fmt.Sscanf(part, "%d", &code); err != nil {
					continue
				}
				if s, ok := slot(code); ok {
					fg = s
				}
				if s, ok := bgSlot(code); ok {
					bg = s
				}
			}
			if fg >= 0 && bg >= 0 && fg == bg {
				t.Fatalf("%s.%s = %q pairs fg and bg from palette slot %d (invisible text)", th.name, role, sgr, fg)
			}
		}
	}
	if _, known := themeByName("tide"); !known {
		t.Fatal("themeByName must resolve preset names case-insensitively")
	}
	if fallback, known := themeByName("no-such-theme"); known || fallback.name != "Tide" {
		t.Fatalf("unknown theme must fall back to Tide, got %s known=%v", fallback.name, known)
	}
}

// TestThemePickerAppliesPersistsAndSticks drives the full theme flow: the
// session menu names the active preset, picking another applies it to the
// live frame, persists it to prefs.json, and re-opens the picker (sticky).
func TestThemePickerAppliesPersistsAndSticks(t *testing.T) {
	w, conn, s := newTestWS(t)
	w.d.prefsPath = filepath.Join(t.TempDir(), "prefs.json")
	s.waitFor(t, "first frame", func() bool { return s.contains("1:") })

	sx, sy := hitCenter(t, w, hitSessionMenu)
	w.handleInput(conn, press(sx, sy))
	w.handleInput(conn, release(sx, sy))
	s.waitFor(t, "session menu names the theme", func() bool { return s.contains("Theme — Tide") })

	menuClick(t, w, conn, "Theme — Tide")
	s.waitFor(t, "theme picker", func() bool { return s.contains("● Tide") && s.contains("○ Ocean") })

	menuClick(t, w, conn, "○ Ocean")
	s.waitFor(t, "ocean card live on the wire", func() bool { return s.contains("\x1b[0;7;34;107m") })
	s.waitFor(t, "picker re-opened with the new mark", func() bool { return s.contains("● Ocean") })
	withWS(w, func() {
		if w.overlay == nil || w.overlay.title != "Theme" {
			t.Fatal("picker must stay open after choosing a preset")
		}
	})
	waitPrefs := func(want string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			if p := session.LoadPrefs(w.d.prefsPath); p.Theme == want {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("prefs.json not persisted; want %q got %+v", want, session.LoadPrefs(w.d.prefsPath))
			}
			time.Sleep(15 * time.Millisecond)
		}
	}
	waitPrefs("ocean")

	// Cycle on to Ink: the reverse-video fallback renders (its card is pure
	// reverse — the bare 0;7 no chromatic preset emits) and persists like
	// any preset.
	menuClick(t, w, conn, "○ Ink")
	s.waitFor(t, "ink card live on the wire", func() bool { return s.contains("\x1b[0;7m ○ ") })
	s.waitFor(t, "picker marks Ink", func() bool { return s.contains("● Ink") })
	waitPrefs("ink")

	// Esc keeps the choice; the session menu now names it.
	w.handleInput(conn, []byte{0x1b})
	time.Sleep(80 * time.Millisecond)
	w.handleInput(conn, press(sx, sy))
	w.handleInput(conn, release(sx, sy))
	s.waitFor(t, "session menu names Ink", func() bool { return s.contains("Theme — Ink") })
}

// TestBorderClickFocusesLeftPane pins focus-follows-border-click: the
// shared border is the right edge of the pane on its left, so pressing it
// focuses that window (the accent perimeter then shows what a split
// would target).
func TestBorderClickFocusesLeftPane(t *testing.T) {
	w, conn, s := newTestWS(t)
	s.waitFor(t, "first frame", func() bool { return s.contains("1:") })
	withWS(w, func() { w.actionSplitLocked(w.lay.FocusedPane(), layout.SplitRight) }) // L | R, focus R

	var bx, by int
	var left string
	s.waitFor(t, "vertical border in hitmap", func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		for _, h := range w.hits {
			if h.kind == hitBorder {
				bx, by = h.rect.X, h.rect.Y+h.rect.H/2
				left = w.paneLeftOfBorderLocked(h.border, by)
				return left != ""
			}
		}
		return false
	})
	withWS(w, func() {
		if w.lay.FocusedPane() == left {
			t.Fatal("test setup: the left pane must start unfocused")
		}
	})
	w.handleInput(conn, press(bx, by))
	w.handleInput(conn, release(bx, by))
	s.waitFor(t, "left pane focused", func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		return w.lay.FocusedPane() == left
	})
}
