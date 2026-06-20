package daemon

// The mouse-capture toggle is the bridge to native terminal copy: releasing
// it hands the pointer to the user's terminal (so its own select-and-copy
// works, like default tmux), and F8 grabs it back. These tests pin the bar
// control, the exact mode sequences put on the render stream, the F8 re-grab,
// inheritance by late-joining clients, and that a released mouse is inert.

import (
	"testing"
	"time"
)

// TestBarMouseToggleReleasesAndF8Regrabs is the headline flow: click the bar
// button → the terminal gets the mouse-off escape and the bar states the F8
// key; press F8 → the terminal gets the mouse-on escape and capture resumes.
func TestBarMouseToggleReleasesAndF8Regrabs(t *testing.T) {
	w, conn, s := newTestWS(t)
	s.waitFor(t, "first frame", func() bool { return s.contains("1:") })
	s.waitFor(t, "mouse button on bar", func() bool { return s.contains(" mouse ") })

	// Click the bar's mouse button → release capture.
	x, y := hitCenter(t, w, hitMouseToggle)
	w.handleInput(conn, press(x, y))
	w.handleInput(conn, release(x, y))

	s.waitFor(t, "mouse-off escape reaches the terminal", func() bool { return s.contains(mouseReleaseSeq) })
	s.waitFor(t, "bar states the F8 re-grab key", func() bool { return s.contains("F8") })
	withWS(w, func() {
		if !w.mouseReleased {
			t.Fatal("bar click must release the mouse")
		}
	})

	// F8 → re-grab.
	w.handleInput(conn, []byte("\x1b[19~")) // legacy CSI for F8 (Terminal.app/GNOME)
	s.waitFor(t, "mouse-on escape reaches the terminal", func() bool { return s.contains(mouseGrabSeq) })
	withWS(w, func() {
		if w.mouseReleased {
			t.Fatal("F8 must re-grab the mouse")
		}
	})
}

// TestReleasedMouseIsInert: once released, in-flight mouse reports are ignored
// — no tide selection starts — so the terminal's native selection is the only
// thing happening (which is the whole point).
func TestReleasedMouseIsInert(t *testing.T) {
	w, conn, s := newTestWS(t)
	s.waitFor(t, "first frame", func() bool { return s.contains("1:") })
	withWS(w, func() {
		p := w.panes[w.lay.FocusedPane()]
		p.term.Write([]byte("\rINERT-LINE\r\n"))
		w.setMouseReleasedLocked(true)
	})

	w.handleInput(conn, press(1, 2))
	w.handleInput(conn, motion(9, 2))
	w.handleInput(conn, release(9, 2))
	time.Sleep(100 * time.Millisecond)

	withWS(w, func() {
		if w.sel.dragging || w.sel.exists {
			t.Fatal("a released mouse must not start a tide selection")
		}
	})
	if s.contains("\x1b]52;p;") {
		t.Fatal("a released mouse must not emit a tide primary-selection copy")
	}
}

// TestLateClientInheritsReleasedState: a client that attaches while the
// session is released must have its terminal put into the same state (its
// enterSequences just turned mouse reporting on), or the bar and reality
// would disagree.
func TestLateClientInheritsReleasedState(t *testing.T) {
	w, _, s1 := newTestWS(t)
	s1.waitFor(t, "client 1 first frame", func() bool { return s1.contains("1:") })
	withWS(w, func() { w.setMouseReleasedLocked(true) })
	s1.waitFor(t, "client 1 release escape", func() bool { return s1.contains(mouseReleaseSeq) })

	_, s2 := attachClient(t, w)
	s2.waitFor(t, "client 2 inherits release", func() bool { return s2.contains(mouseReleaseSeq) })
}

// TestF8GoesToPaneWhenMouseGrabbed: F8 is intercepted ONLY while released, so
// in normal (grabbed) operation it is not stolen from the pane's app.
func TestF8GoesToPaneWhenMouseGrabbed(t *testing.T) {
	w, conn, s := newTestWS(t)
	s.waitFor(t, "first frame", func() bool { return s.contains("1:") })
	withWS(w, func() {
		if w.mouseReleased {
			t.Fatal("a fresh session must start with the mouse grabbed")
		}
	})

	w.handleInput(conn, []byte("\x1b[19~")) // F8 while grabbed
	time.Sleep(100 * time.Millisecond)
	withWS(w, func() {
		if w.mouseReleased {
			t.Fatal("F8 must not toggle the mouse while it is grabbed (it belongs to the pane)")
		}
	})
}
