// The router turns raw client input into action: the single keymap (CUA
// per the ratified rulings — selection-aware Ctrl+C, guarded Ctrl+V,
// Ctrl+Shift+E detach), mouse-first chrome interaction, drag selection,
// border resizing, and pane forwarding with per-pane re-encoding (a pane
// gets keys encoded for ITS terminal modes, never the client's raw bytes).
package daemon

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"time"
	"unicode"

	"github.com/mattn/go-runewidth"

	"github.com/calper-ql/tide/internal/input"
	"github.com/calper-ql/tide/internal/layout"
	"github.com/calper-ql/tide/internal/protocol"
	"github.com/calper-ql/tide/internal/vt"
)

const wheelStep = 3

// handleInput is the entry point for a client's raw input bytes.
func (w *ws) handleInput(conn *protocol.Conn, data []byte) {
	w.mu.Lock()
	defer w.mu.Unlock()
	c, ok := w.clients[conn]
	if !ok {
		return
	}
	c.feedGen++
	for _, ev := range c.decoder.Feed(data) {
		w.routeEventLocked(conn, ev)
	}
	if c.decoder.Pending() {
		w.armFlushLocked(conn, c.feedGen)
	}
}

// armFlushLocked schedules an idle flush for a buffered partial sequence
// (a lone ESC waiting to see whether it starts a sequence). The generation
// check makes it a true idle timer: if more bytes arrived since arming,
// the pending data is a NEWER in-flight sequence whose tail is still on
// the wire — flushing it would shred it into garbage keystrokes.
func (w *ws) armFlushLocked(conn *protocol.Conn, gen uint64) {
	time.AfterFunc(50*time.Millisecond, func() {
		w.mu.Lock()
		defer w.mu.Unlock()
		c, ok := w.clients[conn]
		if !ok || !c.decoder.Pending() {
			return
		}
		if c.feedGen != gen {
			w.armFlushLocked(conn, c.feedGen) // newer feed: wait for ITS idle window
			return
		}
		for _, ev := range c.decoder.Flush() {
			w.routeEventLocked(conn, ev)
		}
	})
}

func (w *ws) routeEventLocked(conn *protocol.Conn, ev input.Event) {
	switch ev.Type {
	case input.EvKey:
		w.routeKeyLocked(conn, ev)
	case input.EvMouse:
		w.routeMouseLocked(conn, ev)
	case input.EvPaste:
		// Terminal-native paste obeys the same guards as Ctrl+V (ruling).
		w.clearSelectionLocked()
		w.pasteLocked(conn, append([]byte(nil), ev.Paste...))
	case input.EvFocus:
		// The client terminal's focus belongs to the focused pane, when
		// its app asked for focus reporting.
		if p := w.panes[w.lay.FocusedPane()]; p != nil && p.term.ModeSnapshot()&vt.ModeFocus != 0 {
			if ev.Gained {
				p.input([]byte("\x1b[I"))
			} else {
				p.input([]byte("\x1b[O"))
			}
		}
	case input.EvUnknown:
		// Unknown sequences are terminal chatter addressed to whoever
		// queried — not a pane app: the VT answers the queries it
		// implements (DSR/CPR/DA/OSC color) itself, and pane queries never
		// reach the client terminal. Dropping is safer than forwarding
		// blind.
	}
}

// routeKeyLocked implements the keymap. Order matters: overlays capture
// everything, then the reserved CUA chords, then the focused pane.
func (w *ws) routeKeyLocked(conn *protocol.Conn, ev input.Event) {
	// Detach must always work — even with an overlay open (a menu must
	// never hold a client hostage).
	if ev.Mods&input.Ctrl != 0 && ev.Mods&input.Shift != 0 && ev.Key == input.KeyRune && unicode.ToLower(ev.Rune) == 'e' {
		w.detachClientLocked(conn)
		return
	}

	if w.overlay != nil {
		switch ev.Key {
		case input.KeyEscape:
			w.closeOverlayLocked()
		case input.KeyEnter:
			w.runSelectedLocked(conn)
		case input.KeyUp:
			w.moveSelLocked(-1)
		case input.KeyDown:
			w.moveSelLocked(1)
		}
		return
	}

	if ev.Mods&input.Ctrl != 0 && ev.Key == input.KeyRune {
		r := unicode.ToLower(ev.Rune)
		shift := ev.Mods&input.Shift != 0
		switch {
		case r == 'c' && !shift:
			// The ratified ruling: selection active → copy and clear; no
			// selection → the byte goes to the pane (SIGINT et al).
			if w.sel.exists && w.sel.pane == w.lay.FocusedPane() {
				w.copySelectionLocked(conn)
				return
			}
		case r == 'c' && shift:
			// Kitty-protocol alias: copy, or explicitly nothing — never a
			// fall-through control byte (the ruling's WT-mistake guard).
			if w.sel.exists && w.sel.pane == w.lay.FocusedPane() {
				w.copySelectionLocked(conn)
			}
			return
		case r == 'v':
			w.pasteLocked(conn, append([]byte(nil), w.clip...))
			return
		}
	}

	// Everything else belongs to the focused pane: any keystroke clears the
	// selection (ruling guardrail) and snaps out of scrollback.
	w.clearSelectionLocked()
	focused := w.lay.FocusedPane()
	p := w.panes[focused]
	if p == nil {
		return
	}
	w.snapLiveLocked(focused)
	if b := input.EncodeKey(ev, w.encodeOptsLocked(p)); b != nil {
		p.input(b)
	}
}

// snapLiveLocked returns a scrolled-back pane to the live view; anything
// that sends input to a pane goes through it, so typing or pasting always
// lands where the user can see it.
func (w *ws) snapLiveLocked(paneID string) {
	if w.scroll[paneID] != 0 {
		w.scroll[paneID] = 0
		w.dirtyPanes[paneID] = true
		w.allDirty = true // bar scroll indicator
		w.signalRender()
	}
}

func (w *ws) encodeOptsLocked(p *pane) input.EncodeOpts {
	m := p.term.ModeSnapshot()
	kitty, modify := p.term.KeyboardProtoSnapshot()
	return input.EncodeOpts{
		AppCursor:       m&vt.ModeAppCursor != 0,
		AppKeypad:       m&vt.ModeAppKeypad != 0,
		BracketedPaste:  m&vt.ModeBracketedPaste != 0,
		CRLF:            m&vt.ModeCRLF != 0,
		KittyFlags:      kitty,
		ModifyOtherKeys: modify,
	}
}

func (w *ws) routeMouseLocked(conn *protocol.Conn, ev input.Event) {
	// An app-forwarded press grabs the mouse for its pane: motion and the
	// release must reach the SAME pane even when the pointer crosses a
	// border, or the app is left with a stuck button and a neighbor gets a
	// release it never saw a press for.
	if w.appGrab != "" {
		switch ev.Mouse {
		case input.MouseMotion, input.MouseWheelUp, input.MouseWheelDown:
			w.forwardMouseClampedLocked(w.appGrab, ev)
		case input.MouseRelease:
			w.forwardMouseClampedLocked(w.appGrab, ev)
			w.appGrab = ""
		case input.MousePress:
			w.forwardMouseClampedLocked(w.appGrab, ev)
		}
		return
	}

	// An in-progress border drag owns the mouse; corner grabs drive both
	// axes at once.
	if w.drag != nil {
		switch ev.Mouse {
		case input.MouseMotion:
			dx, dy := ev.X-w.drag.lastX, ev.Y-w.drag.lastY
			if tab := w.lay.ActiveTab(); tab != nil && (dx != 0 || dy != 0) {
				if w.drag.hasV && dx != 0 {
					tab.DragBorder(w.drag.v, dx, w.area)
				}
				if w.drag.hasH && dy != 0 {
					tab.DragBorder(w.drag.h, dy, w.area)
				}
				w.drag.lastX, w.drag.lastY = ev.X, ev.Y
				w.recomputeLocked()
				w.markAllDirtyLocked()
			}
		case input.MouseRelease:
			w.drag = nil
			w.checkpointLayoutLocked()
		}
		return
	}

	// A frame press waiting to become either a drag or a click (ratified
	// gesture model: motion resizes, release-in-place opens the layout
	// menu for the pressed element).
	if w.pending != nil {
		switch ev.Mouse {
		case input.MouseMotion:
			if w.pending.hasV || w.pending.hasH {
				// Draggable boundaries convert on FIRST motion: a deliberate
				// 1-cell resize must stay instant (the tmux benchmark).
				w.drag = &dragState{v: w.pending.v, h: w.pending.h, hasV: w.pending.hasV, hasH: w.pending.hasH,
					lastX: w.pending.x, lastY: w.pending.y}
				w.pending = nil
				w.routeMouseLocked(conn, ev) // re-dispatch into the drag
				return
			}
			// Nothing to drag here (outer ring, topmost bar): a 3×3 slop
			// keeps a jittery click a click instead of eating its menu.
			if absInt(ev.X-w.pending.x) > 1 || absInt(ev.Y-w.pending.y) > 1 {
				w.pending.moved = true
			}
		case input.MouseRelease:
			p := w.pending
			w.pending = nil
			if !p.moved && p.menu != nil {
				// Anchor at the RELEASE cell: that is where the pointer sits
				// now, so that is where the second click of click-click lands.
				p.menu(w, ev.X, ev.Y)
			}
		}
		return
	}

	// Bare motion (no button, no gesture in flight): hover tracking for
	// terminals that report it.
	if ev.Mouse == input.MouseMotion && ev.Button == 0 {
		w.updateHoverLocked(ev.X, ev.Y)
	}

	// An in-progress selection drag owns the mouse.
	if w.sel.dragging {
		switch ev.Mouse {
		case input.MouseMotion:
			if line, x, ok := w.contentAtLocked(w.sel.pane, ev.X, ev.Y); ok {
				w.sel.eLine, w.sel.eX = line, x
				w.dirtyPanes[w.sel.pane] = true
				w.signalRender()
			}
		case input.MouseRelease:
			w.sel.dragging = false
			if w.sel.aLine == w.sel.eLine && w.sel.aX == w.sel.eX {
				w.sel.exists = false
			} else {
				w.sel.exists = true
				// Mouse selection feeds PRIMARY on release (ruling);
				// CLIPBOARD only on explicit copy.
				if text := w.selectionTextLocked(); text != "" {
					w.sendToLocked(conn, protocol.Message{Type: protocol.TypeRender, Data: osc52('p', text)})
					w.sendToLocked(conn, protocol.Message{Type: protocol.TypeCopy, Target: protocol.CopyPrimary, Data: []byte(text)})
				}
			}
			w.dirtyPanes[w.sel.pane] = true
			w.signalRender()
		}
		return
	}

	hit := w.hitAtLocked(ev.X, ev.Y)

	if ev.Mouse == input.MouseWheelUp || ev.Mouse == input.MouseWheelDown {
		if hit.kind == hitPane {
			w.wheelLocked(conn, hit.pane, ev)
		}
		return
	}

	if ev.Mouse == input.MousePress {
		if w.overlay != nil {
			switch hit.kind {
			case hitMenuItem:
				w.runItemLocked(conn, hit.item)
			case hitOverlayBody:
				// swallow
			default:
				w.closeOverlayLocked()
			}
			return
		}
		switch hit.kind {
		case hitTabLabel:
			oldFocus := w.lay.FocusedPane()
			w.lay.SetActive(hit.tab)
			w.clearSelectionLocked()
			w.notifyFocusLocked(oldFocus, w.lay.FocusedPane())
			w.recomputeLocked()
			w.checkpointLayoutLocked()
			w.markAllDirtyLocked()
		case hitNewTab:
			w.actionNewTabLocked()
		case hitDetach:
			w.detachClientLocked(conn)
		case hitSessionMenu:
			w.openSessionMenuLocked(hit.rect.X, 1) // dropdown below the bar
		case hitPaneSplit:
			w.focusPaneLocked(hit.pane)
			w.openSplitMenuLocked(hit.pane, hit.rect.X, hit.rect.Y+1)
		case hitPaneMenu:
			w.focusPaneLocked(hit.pane)
			w.openPaneMenuLocked(hit.pane, hit.rect.X, hit.rect.Y+1)
		case hitPaneBar:
			// Bars are focus and drag handles, never menus — i3's rule:
			// clicking a title bar focuses, period. Splits live on the [+]
			// button and the window's edges. Exceptions: the junction where
			// a divider bar meets a vertical border keeps its full-span menu
			// (junction semantics — junctions belong to containers, so no
			// focus), and a dead pane's bar honors its own "click to
			// restart" label.
			p := &pendingPress{x: ev.X, y: ev.Y, h: hit.border, hasH: hit.hasBorder}
			if vb, ok := w.cornerVBorderLocked(ev.X, ev.Y); ok {
				p.v, p.hasV = vb, true
			}
			if p.hasH && p.hasV {
				vb, hb := p.v, p.h
				p.menu = func(w *ws, x, y int) { w.openSpanMenuLocked(vb.Node, hb.Node, x, y) }
			} else {
				w.focusPaneLocked(hit.pane)
				if pp := w.panes[hit.pane]; pp != nil && pp.isDead() {
					pane := hit.pane
					p.menu = func(w *ws, _, _ int) { w.restartPaneLocked(pane) }
				}
			}
			w.pending = p
		case hitBorder:
			p := &pendingPress{x: ev.X, y: ev.Y, v: hit.border, hasV: true}
			vb := hit.border
			if hb, ok := w.cornerHBorderLocked(ev.X, ev.Y); ok {
				// Corner (border meets a divider): drag resizes both axes, a
				// click offers the full-span splits of both containers.
				p.h, p.hasH = hb, true
				p.menu = func(w *ws, x, y int) { w.openSpanMenuLocked(vb.Node, hb.Node, x, y) }
			} else if ev.Y == vb.Rect.Y {
				// The border's top end (the ┬): full-span above/below the
				// container it divides.
				p.menu = func(w *ws, x, y int) { w.openSpanMenuLocked(vb.Node, nil, x, y) }
			} else if left := w.paneLeftOfBorderLocked(vb, ev.Y); left != "" {
				// Mid-border: the right edge of the pane on its left. Focus
				// follows the CLICK (i3), so the accent perimeter shows the
				// window the menu will split — but only on click resolution:
				// a resize drag must never steal focus or clear a selection.
				p.menu = func(w *ws, x, y int) {
					w.focusPaneLocked(left)
					w.openEdgeMenuLocked(left, layout.SplitRight, x, y)
				}
			}
			w.pending = p
		case hitFrameEdge:
			// A ring junction (┴ where a border meets the bottom, ├/┤ where a
			// divider meets a side) offers full-span splits; the flat ring is
			// the abutting window's own left/right/bottom edge, and a CLICK
			// on it focuses that window like any of its edges (never a press
			// that turns out to be travel).
			if vNode, hNode, ok := w.ringCornerLocked(ev.X, ev.Y); ok {
				w.pending = &pendingPress{x: ev.X, y: ev.Y,
					menu: func(w *ws, x, y int) { w.openSpanMenuLocked(vNode, hNode, x, y) }}
			} else if pane, dir, ok := w.ringEdgeTargetLocked(ev.X, ev.Y); ok {
				w.pending = &pendingPress{x: ev.X, y: ev.Y,
					menu: func(w *ws, x, y int) {
						w.focusPaneLocked(pane)
						w.openEdgeMenuLocked(pane, dir, x, y)
					}}
			} else {
				w.pending = &pendingPress{x: ev.X, y: ev.Y}
			}
		case hitPane:
			w.panePressLocked(conn, hit.pane, ev)
		}
		return
	}

	// Motion/release with no drag in progress: an app that asked for
	// motion gets it.
	if hit.kind == hitPane {
		w.forwardMouseLocked(hit.pane, ev)
	}
}

// restartPaneLocked respawns a dead pane's shell — the action behind both
// a dead pane's content click and its bar's "click to restart" label.
func (w *ws) restartPaneLocked(paneID string) {
	p := w.panes[paneID]
	if p == nil {
		return
	}
	if err := p.respawnIfDead(w.d.socket); err != nil {
		w.flashStatusLocked("restart failed: " + err.Error())
	}
	w.dirtyPanes[paneID] = true
	w.signalRender()
}

// focusPaneLocked moves focus with all its invariants: selection clears
// (ruling guardrail), focus reporting fires, the layout checkpoint keeps
// focus across a controlled restart.
func (w *ws) focusPaneLocked(paneID string) {
	if w.lay.FocusedPane() == paneID || w.panes[paneID] == nil {
		return
	}
	oldFocus := w.lay.FocusedPane()
	w.lay.Focus(paneID)
	w.clearSelectionLocked()
	w.notifyFocusLocked(oldFocus, paneID)
	w.checkpointLayoutLocked()
	w.markAllDirtyLocked() // bar styling + cursor move
}

// cornerVBorderLocked finds a vertical border within one column of x whose
// span covers y — the perpendicular half of a corner grab.
func (w *ws) cornerVBorderLocked(x, y int) (layout.Border, bool) {
	for _, bd := range w.borders {
		if !bd.Vertical {
			continue
		}
		if absInt(bd.Rect.X-x) <= 1 && y >= bd.Rect.Y && y < bd.Rect.Y+bd.Rect.H {
			return bd, true
		}
	}
	return layout.Border{}, false
}

// cornerHBorderLocked finds a bar divider within one row of y whose span
// flanks column x.
func (w *ws) cornerHBorderLocked(x, y int) (layout.Border, bool) {
	for _, bd := range w.borders {
		if bd.Vertical {
			continue
		}
		if absInt(bd.Rect.Y-y) <= 1 && x >= bd.Rect.X-1 && x <= bd.Rect.X+bd.Rect.W {
			return bd, true
		}
	}
	return layout.Border{}, false
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// updateHoverLocked tracks the frame element under the pointer. Corner
// zones include every border meeting there — the highlight previews what a
// corner drag or click affects. Renders happen only when the hovered
// REGION changes, so the 1003 motion stream stays cheap.
func (w *ws) updateHoverLocked(x, y int) {
	h := hoverState{menuItem: -1}
	hit := w.hitAtLocked(x, y)
	switch hit.kind {
	case hitMenuItem:
		h.menuItem = hit.item
		h.key = fmt.Sprintf("mi:%d", hit.item)
	case hitOverlayBody:
		h.key = "overlay"
	case hitTabLabel, hitNewTab, hitDetach, hitSessionMenu:
		h.barKind, h.barTab = hit.kind, hit.tab
		h.key = fmt.Sprintf("bb:%d:%d", hit.kind, hit.tab)
	case hitPaneBar, hitPaneSplit, hitPaneMenu:
		h.bars = map[string]bool{hit.pane: true}
		h.key = "bar:" + hit.pane
		if vb, ok := w.cornerVBorderLocked(x, y); ok {
			h.strips = append(h.strips, vb.Rect)
			h.key += fmt.Sprintf("+v%d", vb.Rect.X)
		}
	case hitBorder:
		h.strips = append(h.strips, hit.border.Rect)
		h.key = fmt.Sprintf("vb:%d", hit.border.Rect.X)
		if hb, ok := w.cornerHBorderLocked(x, y); ok {
			// The horizontal divider is a pane's bar: highlight it as one.
			if id := w.paneAtBarLocked(hb.Rect); id != "" {
				h.bars = map[string]bool{id: true}
			}
			h.key += fmt.Sprintf("+h%d", hb.Rect.Y)
		}
	case hitFrameEdge:
		// Mirror the press resolution so the highlight previews exactly what a
		// click does: a junction lights the full-span edge; a flat ring strip
		// lights ONLY the abutting window's own segment, not the whole edge.
		if _, _, ok := w.ringCornerLocked(x, y); ok {
			if y >= w.rows-1 {
				h.strips = append(h.strips, layout.Rect{X: 0, Y: w.rows - 1, W: w.cols, H: 1})
			} else {
				col := 0
				if x >= w.cols-1 {
					col = w.cols - 1
				}
				h.strips = append(h.strips, layout.Rect{X: col, Y: w.area.Y, W: 1, H: w.area.H})
			}
			h.key = fmt.Sprintf("span:%d:%d", x, y)
		} else if pane, dir, found := w.ringEdgeTargetLocked(x, y); found {
			if r, okr := w.rects[pane]; okr {
				switch dir {
				case layout.SplitDown:
					h.strips = append(h.strips, layout.Rect{X: r.X, Y: w.rows - 1, W: r.W, H: 1})
				case layout.SplitLeft:
					h.strips = append(h.strips, layout.Rect{X: 0, Y: r.Y, W: 1, H: r.H})
				case layout.SplitRight:
					h.strips = append(h.strips, layout.Rect{X: w.cols - 1, Y: r.Y, W: 1, H: r.H})
				}
				h.key = fmt.Sprintf("edge:%s:%d", pane, dir)
			}
		}
	}
	if h.key != w.hover.key {
		// The menu highlight follows the pointer only when the hovered
		// element actually CHANGES, inside the same gate that schedules the
		// repaint — a jiggle within one cell must never silently retarget
		// Enter away from an arrow-key selection the screen still shows.
		if o := w.overlay; o != nil && hit.kind == hitMenuItem &&
			hit.item < len(o.items) && o.items[hit.item].enabled && !o.items[hit.item].separator {
			o.sel = hit.item
		}
		w.hover = h
		w.markChromeDirtyLocked()
	}
}

// paneAtBarLocked finds the pane whose bar occupies a divider rect.
func (w *ws) paneAtBarLocked(bar layout.Rect) string {
	for id, r := range w.rects {
		if r.X == bar.X && r.Y == bar.Y {
			return id
		}
	}
	return ""
}

// panePressLocked: focus, then right-click menu / dead-pane restart /
// app forwarding / selection start.
func (w *ws) panePressLocked(conn *protocol.Conn, paneID string, ev input.Event) {
	p := w.panes[paneID]
	if p == nil {
		return
	}
	w.focusPaneLocked(paneID)
	if ev.Button == 3 {
		// Right-click stays as a pane-menu accelerator where terminals
		// forward it (ratified); the [≡] button covers the rest.
		w.openPaneMenuLocked(paneID, ev.X, ev.Y)
		return
	}
	if p.isDead() {
		w.restartPaneLocked(paneID)
		return
	}
	if w.appWantsMouseLocked(p) && ev.Mods&input.Shift == 0 {
		w.appGrab = paneID // motion/release stay with this pane until release
		w.forwardMouseLocked(paneID, ev)
		return
	}
	if ev.Button == 1 {
		w.clearSelectionLocked()
		if line, x, ok := w.contentAtLocked(paneID, ev.X, ev.Y); ok {
			w.sel = selectionState{pane: paneID, dragging: true, aLine: line, aX: x, eLine: line, eX: x}
		}
	}
}

func (w *ws) wheelLocked(conn *protocol.Conn, paneID string, ev input.Event) {
	p := w.panes[paneID]
	if p == nil {
		return
	}
	if w.appWantsMouseLocked(p) && ev.Mods&input.Shift == 0 {
		w.forwardMouseLocked(paneID, ev)
		return
	}
	hist, _, _ := p.term.ContentSize()
	s := w.scroll[paneID]
	if ev.Mouse == input.MouseWheelUp {
		s += wheelStep
	} else {
		s -= wheelStep
	}
	w.scroll[paneID] = clampInt(s, 0, hist)
	w.dirtyPanes[paneID] = true
	w.allDirty = true // bar indicator
	w.signalRender()
}

// appWantsMouseLocked reports whether the pane's application enabled any
// mouse reporting; holding Shift always bypasses the app (escape hatch,
// Zellij convention).
func (w *ws) appWantsMouseLocked(p *pane) bool {
	m := p.term.MouseSnapshot()
	return m.X10 || m.Normal || m.ButtonDrag || m.AnyMotion
}

// forwardMouseLocked re-encodes a mouse event for the pane's protocol at
// pane-local coordinates; events outside the pane's rect are dropped.
func (w *ws) forwardMouseLocked(paneID string, ev input.Event) {
	w.forwardMouseAtLocked(paneID, ev, false)
}

// forwardMouseClampedLocked is the grabbed-drag variant: coordinates clamp
// to the pane's rect instead of dropping, so the app sees a continuous
// drag even when the pointer leaves the pane.
func (w *ws) forwardMouseClampedLocked(paneID string, ev input.Event) {
	w.forwardMouseAtLocked(paneID, ev, true)
}

func (w *ws) forwardMouseAtLocked(paneID string, ev input.Event, clamp bool) {
	p := w.panes[paneID]
	rr, ok := w.rects[paneID]
	if p == nil || !ok {
		return
	}
	r := contentRect(rr)
	if r.W < 1 || r.H < 1 {
		return
	}
	m := p.term.MouseSnapshot()
	proto := input.MouseOff
	switch {
	case m.AnyMotion:
		proto = input.MouseAnyMotion
	case m.ButtonDrag:
		proto = input.MouseButtonMotion
	case m.Normal:
		proto = input.MouseNormal
	case m.X10:
		proto = input.MouseX10
	}
	if proto == input.MouseOff {
		return
	}
	lx, ly := ev.X-r.X, ev.Y-r.Y
	if clamp {
		lx, ly = clampInt(lx, 0, r.W-1), clampInt(ly, 0, r.H-1)
	} else if lx < 0 || ly < 0 || lx >= r.W || ly >= r.H {
		return
	}
	if b := input.EncodeMouse(ev, proto, m.SGR, lx, ly); b != nil {
		p.input(b)
	}
}

// notifyFocusLocked delivers CSI I/O to panes whose applications enabled
// focus reporting (DECSET 1004).
func (w *ws) notifyFocusLocked(oldID, newID string) {
	if oldID == newID {
		return
	}
	if p := w.panes[oldID]; p != nil && p.term.ModeSnapshot()&vt.ModeFocus != 0 {
		p.input([]byte("\x1b[O"))
	}
	if p := w.panes[newID]; p != nil && p.term.ModeSnapshot()&vt.ModeFocus != 0 {
		p.input([]byte("\x1b[I"))
	}
}

// contentAtLocked maps a screen cell to a pane's content coordinates
// (history-index space), so selections stay glued to their text while
// output scrolls underneath. Coordinates are relative to the content rect
// (inside the pane's frame).
func (w *ws) contentAtLocked(paneID string, x, y int) (line, col int, ok bool) {
	rr, found := w.rects[paneID]
	p := w.panes[paneID]
	if !found || p == nil {
		return 0, 0, false
	}
	r := contentRect(rr)
	hist, _, cols := p.term.ContentSize()
	ly := clampInt(y-r.Y, 0, r.H-1)
	lx := clampInt(x-r.X, 0, min(cols, r.W)-1)
	return hist - w.scroll[paneID] + ly, lx, true
}

func (w *ws) clearSelectionLocked() {
	if w.sel.exists || w.sel.dragging {
		w.dirtyPanes[w.sel.pane] = true
		w.sel = selectionState{}
		w.signalRender()
	}
}

func (w *ws) selectionTextLocked() string {
	p := w.panes[w.sel.pane]
	if p == nil {
		return ""
	}
	l0, x0, l1, x1 := w.sel.normalized()
	return p.term.ContentText(l0, x0, l1, x1)
}

// copySelectionLocked implements the copy half of the Ctrl+C ruling: the
// internal clipboard and the requesting client's system clipboard both get
// the text, the selection clears (second Ctrl+C interrupts), and the bar
// confirms it happened (discoverability). The system clipboard is fed two
// ways: OSC 52 on the render stream (works over SSH where the terminal
// honors it) and a copy frame the client pipes into the platform tool
// (works in terminals that discard OSC 52, e.g. Terminal.app).
func (w *ws) copySelectionLocked(conn *protocol.Conn) {
	text := w.selectionTextLocked()
	if text == "" {
		w.clearSelectionLocked()
		return
	}
	w.clip = []byte(text)
	w.sendToLocked(conn, protocol.Message{Type: protocol.TypeRender, Data: osc52('c', text)})
	w.sendToLocked(conn, protocol.Message{Type: protocol.TypeCopy, Target: protocol.CopyClipboard, Data: []byte(text)})
	w.clearSelectionLocked()
	w.flashStatusLocked(fmt.Sprintf("copied %d chars — Ctrl+C again reaches the shell", len(text)))
}

func osc52(target byte, text string) []byte {
	var b bytes.Buffer
	b.WriteString("\x1b]52;")
	b.WriteByte(target)
	b.WriteByte(';')
	b.WriteString(base64.StdEncoding.EncodeToString([]byte(text)))
	b.WriteByte('\a')
	return b.Bytes()
}

// pasteLocked implements the Ctrl+V ruling: bracketed-paste-aware, and
// multi-line or control-laden pastes into a bare shell need a confirm
// (paste guards).
func (w *ws) pasteLocked(conn *protocol.Conn, data []byte) {
	if len(data) == 0 {
		return
	}
	focused := w.lay.FocusedPane()
	p := w.panes[focused]
	if p == nil {
		return
	}
	w.snapLiveLocked(focused)
	opts := w.encodeOptsLocked(p)
	if opts.BracketedPaste || !pasteNeedsConfirm(data) {
		p.input(input.EncodePaste(data, opts))
		return
	}
	lines := bytes.Count(data, []byte{'\n'}) + 1
	w.openCenteredLocked(&overlay{
		title: fmt.Sprintf("Paste %d lines into the shell? (Enter pastes, Esc cancels)", lines),
		items: []menuItem{
			{label: "Paste", enabled: true, run: func(w *ws, _ *protocol.Conn) {
				// Modes can change during the (human-time) confirm window;
				// re-read them so a pane that enabled bracketed paste
				// meanwhile gets a properly wrapped paste.
				if pp := w.panes[w.lay.FocusedPane()]; pp != nil {
					w.snapLiveLocked(w.lay.FocusedPane())
					pp.input(input.EncodePaste(data, w.encodeOptsLocked(pp)))
				}
			}},
			{label: "Cancel", enabled: true, run: func(w *ws, _ *protocol.Conn) {}},
		},
	})
}

// pasteNeedsConfirm flags multi-line pastes and control codes a bare shell
// would execute or misinterpret (tab is the one benign control).
func pasteNeedsConfirm(data []byte) bool {
	for _, c := range data {
		if c == '\n' || c == '\r' {
			return true
		}
		if c < 0x20 && c != '\t' {
			return true
		}
		if c == 0x7f {
			return true
		}
	}
	return false
}

// --- chrome actions ----------------------------------------------------

func (w *ws) actionNewTabLocked() {
	id := newPaneID()
	p, err := w.spawnPane(id, nil, w.area.W, w.area.H)
	if err != nil {
		w.flashStatusLocked("new tab failed: " + err.Error())
		return
	}
	w.panes[id] = p
	w.lay.NewTab(id)
	w.clearSelectionLocked()
	w.recomputeLocked()
	w.checkpointLayoutLocked()
	w.markAllDirtyLocked()
}

func (w *ws) actionSplitLocked(target string, dir layout.Dir) {
	if _, ok := w.panes[target]; !ok {
		return
	}
	id := newPaneID()
	r := w.rects[target]
	p, err := w.spawnPane(id, nil, max(r.W/2, layout.MinPaneW), max(r.H/2, layout.MinPaneH))
	if err != nil {
		w.flashStatusLocked("split failed: " + err.Error())
		return
	}
	if err := w.lay.Split(target, dir, id); err != nil {
		go p.shutdown()
		return
	}
	w.panes[id] = p
	w.clearSelectionLocked()
	w.recomputeLocked()
	w.checkpointLayoutLocked()
	w.markAllDirtyLocked()
}

func (w *ws) actionClosePaneLocked(id string) {
	if w.lay.CountPanes() <= 1 {
		w.flashStatusLocked("last pane — use Kill Session to end the session")
		return
	}
	p := w.panes[id]
	if p == nil {
		return
	}
	w.lay.ClosePane(id)
	delete(w.panes, id)
	delete(w.scroll, id)
	if w.sel.pane == id {
		w.sel = selectionState{}
	}
	go func() {
		p.shutdown()
		w.d.registry.RemovePaneContent(id)
	}()
	w.recomputeLocked()
	w.checkpointLayoutLocked()
	w.markAllDirtyLocked()
}

// paneTitleLocked names a pane for menu titles.
func (w *ws) paneTitleLocked(paneID string) string {
	if p := w.panes[paneID]; p != nil {
		if t := sanitizeLabel(p.term.TitleSnapshot()); t != "" {
			return runewidth.Truncate(t, 16, "…")
		}
	}
	return "shell"
}

// --- overlay placement ---------------------------------------------------

// firstEnabledIdx returns the first runnable item, or -1.
func firstEnabledIdx(items []menuItem) int {
	for i, it := range items {
		if it.enabled && !it.separator {
			return i
		}
	}
	return -1
}

// openAtLocked shows o as a dropdown with its top-left at (x, y) — the
// anchoring for bar buttons ([+], [≡], the session ▾) — with the first
// enabled item pre-lit so Enter always has a visible target.
func (w *ws) openAtLocked(o *overlay, x, y int) {
	wd, ht := w.overlaySize(o)
	o.x = clampInt(x, 0, w.cols-wd)
	o.y = clampInt(y, 1, w.rows-ht)
	o.sel = firstEnabledIdx(o.items)
	w.overlay = o
	w.markAllDirtyLocked()
}

// openCenteredLocked shows o centered — confirm dialogs.
func (w *ws) openCenteredLocked(o *overlay) {
	wd, ht := w.overlaySize(o)
	w.openAtLocked(o, (w.cols-wd)/2, (w.rows-ht)/2)
}

// openAtPointerLocked shows o so the default item's row sits directly
// under the pointer: a second click without moving runs the default —
// click-click splitting, no travel, no 1003 hover needed. Flippable menus
// (primary item first, separator second) grow upward when the pointer is
// near the bottom edge, keeping the default under the pointer. After
// clamping, the truth rule applies: pre-lit is exactly the item under the
// pointer, or nothing — never a remembered index the click would miss.
func (w *ws) openAtPointerLocked(o *overlay, clickX, clickY int, flippable bool) {
	wd, ht := w.overlaySize(o)
	head := overlayHeadRows(o)
	def := max(firstEnabledIdx(o.items), 0)
	y := clickY - head - def
	if clampInt(y, 1, w.rows-ht) != y && flippable && len(o.items) >= 2 && o.items[1].separator {
		if yb := clickY - (ht - 1); clampInt(yb, 1, w.rows-ht) == yb {
			// Move the primary to the last row, its rule still adjacent.
			items := make([]menuItem, 0, len(o.items))
			items = append(items, o.items[2:]...)
			items = append(items, o.items[1], o.items[0])
			o.items = items
			y = yb
		}
	}
	if y < 1 && head > 0 {
		// The top rows of the pane area: there is no room for the title
		// above the pointer and no downward flip exists. Drop the head —
		// click-click must survive everywhere, and the focused perimeter
		// still names the target.
		o.title = ""
		wd, ht = w.overlaySize(o)
		head = 0
		y = clickY - def
	}
	o.x = clampInt(clickX-3, 0, w.cols-wd)
	o.y = clampInt(y, 1, w.rows-ht)
	o.sel = -1
	if i := clickY - o.y - head; i >= 0 && i < len(o.items) && o.items[i].enabled && !o.items[i].separator {
		o.sel = i
	}
	w.overlay = o
	w.markAllDirtyLocked()
}

// moveSelLocked moves the overlay highlight (Up/Down), wrapping and
// skipping separators and disabled items.
func (w *ws) moveSelLocked(delta int) {
	o := w.overlay
	if o == nil || len(o.items) == 0 {
		return
	}
	i := o.sel
	for range o.items {
		i += delta
		if i < 0 {
			i = len(o.items) - 1
		} else if i >= len(o.items) {
			i = 0
		}
		if o.items[i].enabled && !o.items[i].separator {
			o.sel = i
			w.markChromeDirtyLocked()
			return
		}
	}
}

// --- menus ---------------------------------------------------------------

// openPaneMenuLocked is the [≡] button's menu: actions about THIS pane's
// contents and life. Splitting is spatial ([+] and window edges), so it
// carries no Split items. Disabled items stay visible and say why —
// discoverability (spec req 5) extends to "why can't I".
func (w *ws) openPaneMenuLocked(paneID string, x, y int) {
	p := w.panes[paneID]
	if p == nil {
		return
	}
	hasSel := w.sel.exists && w.sel.pane == paneID
	hasClip := len(w.clip) > 0
	dead := p.isDead()
	canClose := w.lay.CountPanes() > 1
	reason := func(label string, on bool, why string) string {
		if on {
			return label
		}
		return label + " — " + why
	}
	w.openAtLocked(&overlay{
		pane:  paneID,
		title: w.paneTitleLocked(paneID),
		items: []menuItem{
			{label: reason("Copy", hasSel, "select text first"), enabled: hasSel,
				run: func(w *ws, c *protocol.Conn) { w.copySelectionLocked(c) }},
			{label: reason("Paste", hasClip, "clipboard empty"), enabled: hasClip,
				run: func(w *ws, c *protocol.Conn) { w.pasteLocked(c, append([]byte(nil), w.clip...)) }},
			{separator: true},
			{label: reason("Restart Shell", dead, "still running"), enabled: dead,
				run: func(w *ws, _ *protocol.Conn) {
					if pp := w.panes[paneID]; pp != nil {
						_ = pp.respawnIfDead(w.d.socket)
					}
				}},
			{label: reason("Close Pane", canClose, "last pane"), enabled: canClose,
				run: func(w *ws, _ *protocol.Conn) { w.actionClosePaneLocked(paneID) }},
		},
	}, x, y)
}

// splitItems builds the four-direction split list for one window: the
// primary direction first (the default under the pointer), a rule, then
// the rest in fixed ↑ ↓ ← → order. Every item splits THIS pane;
// Layout.Split joins a same-axis run and nests a container on the cross
// axis, exactly like i3.
func splitItems(paneID string, primary layout.Dir) []menuItem {
	items := make([]menuItem, 0, 5)
	add := func(d layout.Dir) {
		items = append(items, menuItem{label: splitLabel(d), enabled: true,
			run: func(w *ws, _ *protocol.Conn) { w.actionSplitLocked(paneID, d) }})
	}
	add(primary)
	items = append(items, menuItem{separator: true})
	for _, d := range []layout.Dir{layout.SplitUp, layout.SplitDown, layout.SplitLeft, layout.SplitRight} {
		if d != primary {
			add(d)
		}
	}
	return items
}

// openEdgeMenuLocked opens the directional split menu for one window's
// edge (i3-style, pointer-anchored): click-click on the edge runs the
// clicked side's direction.
func (w *ws) openEdgeMenuLocked(paneID string, primary layout.Dir, x, y int) {
	if _, ok := w.panes[paneID]; !ok {
		return
	}
	o := &overlay{pane: paneID, title: w.paneTitleLocked(paneID), items: splitItems(paneID, primary)}
	w.openAtPointerLocked(o, x, y, true)
}

// openSplitMenuLocked is the [+] button's dropdown: the same four
// directions, for terminals (and first runs) where a 1-cell border is not
// a discoverable target.
func (w *ws) openSplitMenuLocked(paneID string, x, y int) {
	if _, ok := w.panes[paneID]; !ok {
		return
	}
	o := &overlay{pane: paneID, title: w.paneTitleLocked(paneID), items: splitItems(paneID, layout.SplitRight)}
	w.openAtLocked(o, x, y)
}

func splitLabel(d layout.Dir) string {
	switch d {
	case layout.SplitUp:
		return "↑ New pane above"
	case layout.SplitDown:
		return "↓ New pane below"
	case layout.SplitLeft:
		return "← New pane left"
	default:
		return "→ New pane right"
	}
}

// ringEdgeTargetLocked resolves an outer-ring cell to the window it abuts
// and the outward split direction. The ring is segmented per window so
// every pane owns its own left/right/bottom edge (a pane's top edge is its
// bar). The bottom wins at the bottom corners.
func (w *ws) ringEdgeTargetLocked(x, y int) (string, layout.Dir, bool) {
	switch {
	case y >= w.rows-1:
		if p := w.bottomPaneAtLocked(x); p != "" {
			return p, layout.SplitDown, true
		}
	case x <= 0:
		if p := w.edgePaneAtLocked(y, true); p != "" {
			return p, layout.SplitLeft, true
		}
	case x >= w.cols-1:
		if p := w.edgePaneAtLocked(y, false); p != "" {
			return p, layout.SplitRight, true
		}
	}
	return "", 0, false
}

// edgePaneAtLocked returns the left- or right-most pane whose rect covers
// row y — the window abutting the outer ring on that side.
func (w *ws) edgePaneAtLocked(y int, leftSide bool) string {
	for id, r := range w.rects {
		if y < r.Y || y >= r.Y+r.H {
			continue
		}
		if leftSide && r.X == w.area.X {
			return id
		}
		if !leftSide && r.X+r.W == w.area.X+w.area.W {
			return id
		}
	}
	return ""
}

// bottomPaneAtLocked returns the bottom-most pane whose rect covers column
// x — the window abutting the bottom ring there.
func (w *ws) bottomPaneAtLocked(x int) string {
	for id, r := range w.rects {
		if x < r.X || x >= r.X+r.W {
			continue
		}
		if r.Y+r.H == w.area.Y+w.area.H {
			return id
		}
	}
	return ""
}

// paneLeftOfBorderLocked returns the pane whose right edge is the vertical
// border bd at row y — the window that border belongs to.
func (w *ws) paneLeftOfBorderLocked(bd layout.Border, y int) string {
	for id, r := range w.rects {
		if y < r.Y || y >= r.Y+r.H {
			continue
		}
		if r.X+r.W == bd.Rect.X {
			return id
		}
	}
	return ""
}

// actionSplitNodeLocked inserts a fresh pane beside an arbitrary layout
// node — the full-span (container-level) split executor. The node pointer
// was captured when the menu opened; SplitNode revalidates it against the
// tree, so a layout that changed underneath fails loudly, not silently.
func (w *ws) actionSplitNodeLocked(tab int, target *layout.Node, d layout.Dir) {
	id := newPaneID()
	p, err := w.spawnPane(id, nil, max(w.area.W/2, layout.MinPaneW), max(w.area.H/2, layout.MinPaneH))
	if err != nil {
		w.flashStatusLocked("split failed: " + err.Error())
		return
	}
	if err := w.lay.SplitNode(tab, target, d, id); err != nil {
		go p.shutdown()
		w.flashStatusLocked("split failed: " + err.Error())
		return
	}
	w.panes[id] = p
	w.clearSelectionLocked()
	w.recomputeLocked()
	w.checkpointLayoutLocked()
	w.markAllDirtyLocked()
}

// spanItem builds a full-span split entry against a container node.
func (w *ws) spanItem(label string, node *layout.Node, d layout.Dir) menuItem {
	tab := w.lay.Active
	return menuItem{label: label, enabled: true, run: func(w *ws, _ *protocol.Conn) {
		w.actionSplitNodeLocked(tab, node, d)
	}}
}

// openSpanMenuLocked is the corner (junction) menu: the container-level,
// full-span counterpart to the per-window edge menu. vNode is the container
// a vertical boundary divides — splitting it up/down spans its full WIDTH;
// hNode is the container a horizontal boundary divides — splitting it
// left/right spans its full HEIGHT. Either may be nil (a ring junction
// touches only one).
func (w *ws) openSpanMenuLocked(vNode, hNode *layout.Node, x, y int) {
	var items []menuItem
	if vNode != nil {
		items = append(items,
			w.spanItem("↑ New pane above — full width", vNode, layout.SplitUp),
			w.spanItem("↓ New pane below — full width", vNode, layout.SplitDown),
		)
	}
	if hNode != nil {
		if len(items) > 0 {
			items = append(items, menuItem{separator: true})
		}
		items = append(items,
			w.spanItem("← New pane left — full height", hNode, layout.SplitLeft),
			w.spanItem("→ New pane right — full height", hNode, layout.SplitRight),
		)
	}
	if len(items) == 0 {
		return
	}
	w.openAtPointerLocked(&overlay{title: "Across", items: items}, x, y, false)
}

// ringCornerLocked detects an outer-ring junction and returns the container
// nodes to span: a vertical border reaching the bottom ring (┴ → full-width
// above/below its container) or a horizontal divider reaching a side ring
// (├/┤ → full-height left/right its container).
func (w *ws) ringCornerLocked(x, y int) (vNode, hNode *layout.Node, ok bool) {
	bottom := y >= w.rows-1
	side := x <= 0 || x >= w.cols-1
	for i := range w.borders {
		b := w.borders[i]
		if bottom && b.Vertical && absInt(b.Rect.X-x) <= 1 && b.Rect.Y+b.Rect.H >= w.rows-1 {
			vNode = b.Node
		}
		if side && !b.Vertical && b.Rect.Y == y {
			hNode = b.Node
		}
	}
	return vNode, hNode, vNode != nil || hNode != nil
}

// openSessionMenuLocked lives behind the session bar's project segment
// (ratified): session-level actions, and the theme picker's home — the
// label always names the active preset, so the state shows before any
// click.
func (w *ws) openSessionMenuLocked(x, y int) {
	w.openAtLocked(&overlay{
		title: "Session",
		items: []menuItem{
			{label: "New Tab", enabled: true, run: func(w *ws, _ *protocol.Conn) { w.actionNewTabLocked() }},
			{separator: true},
			{label: "Theme — " + w.d.themeNow().name, enabled: true,
				run: func(w *ws, _ *protocol.Conn) { w.openThemePickerLocked(x, y) }},
			{separator: true},
			{label: "Detach (Ctrl+Shift+E)", enabled: true, run: func(w *ws, c *protocol.Conn) { w.detachClientLocked(c) }},
			{label: "Kill Session…", enabled: true, danger: true, run: func(w *ws, _ *protocol.Conn) { w.openKillConfirmLocked() }},
		},
	}, x, y)
}

// openThemePickerLocked lists the presets, ● marking the active one.
// Choosing a preset applies it live to every session and client, persists
// it, and re-opens the picker in place — presets can be cycled and
// compared click by click, and Esc keeps whatever is showing.
func (w *ws) openThemePickerLocked(x, y int) {
	cur := w.d.themeNow().name
	items := make([]menuItem, 0, len(themes))
	sel := 0
	for i := range themes {
		t := &themes[i]
		mark := "○ "
		if t.name == cur {
			mark, sel = "● ", i
		}
		items = append(items, menuItem{label: mark + t.name, enabled: true,
			run: func(w *ws, _ *protocol.Conn) {
				w.d.theme.Store(t)            // atomic: safe under w.mu, visible to all renders
				go w.d.persistTheme()         // prefs + other sessions; never under w.mu
				w.openThemePickerLocked(x, y) // sticky: the click re-opens the picker
			}})
	}
	o := &overlay{title: "Theme", items: items}
	w.openAtLocked(o, x, y)
	o.sel = sel
}

func (w *ws) openKillConfirmLocked() {
	w.openCenteredLocked(&overlay{
		title: "Kill this session? Every shell in it dies.",
		items: []menuItem{
			{label: "Kill Session", enabled: true, danger: true, run: func(w *ws, _ *protocol.Conn) {
				root := w.root
				d := w.d
				go d.killFromUI(root) // re-enters the daemon lock; never under w.mu
			}},
			{label: "Cancel", enabled: true, run: func(w *ws, _ *protocol.Conn) {}},
		},
	})
}

func (w *ws) closeOverlayLocked() {
	if w.overlay != nil {
		w.overlay = nil
		w.markAllDirtyLocked()
	}
}

func (w *ws) runItemLocked(conn *protocol.Conn, i int) {
	o := w.overlay
	if o == nil || i < 0 || i >= len(o.items) || !o.items[i].enabled || o.items[i].separator {
		return
	}
	w.overlay = nil
	w.markAllDirtyLocked()
	o.items[i].run(w, conn)
}

// runSelectedLocked is Enter: exactly the highlighted item. When nothing
// is lit (truth rule after clamping) Enter runs nothing — never a
// remembered or implicit index the screen does not show.
func (w *ws) runSelectedLocked(conn *protocol.Conn) {
	if o := w.overlay; o != nil {
		w.runItemLocked(conn, o.sel)
	}
}
