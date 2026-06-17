package main

import (
	"testing"

	"github.com/calper-ql/tide/internal/tui"
)

func TestComputeLayoutNormal(t *testing.T) {
	r := computeLayout(100, 30, 28, false)
	if r.activity.W != activityW {
		t.Errorf("activity width = %d, want %d", r.activity.W, activityW)
	}
	if r.side.W != 28 {
		t.Errorf("side width = %d, want 28", r.side.W)
	}
	if r.editor.X != activityW+28 {
		t.Errorf("editor x = %d, want %d", r.editor.X, activityW+28)
	}
	if r.editor.W != 100-(activityW+28) {
		t.Errorf("editor width = %d, want %d", r.editor.W, 100-(activityW+28))
	}
	if r.editor.Y != 1 || r.tabs.Y != 0 || r.tabs.H != 1 {
		t.Errorf("tab strip / editor stacking wrong: tabs=%+v editor=%+v", r.tabs, r.editor)
	}
	if r.status.Y != 29 || r.status.W != 100 {
		t.Errorf("status bar = %+v, want full-width row 29", r.status)
	}
	if r.editor.H != 28 { // workH 29 minus the 1-row tab strip
		t.Errorf("editor height = %d, want 28", r.editor.H)
	}
}

func TestComputeLayoutCollapsed(t *testing.T) {
	r := computeLayout(100, 30, 28, true)
	if r.side.W != 0 {
		t.Errorf("collapsed side width = %d, want 0", r.side.W)
	}
	if r.editor.X != activityW {
		t.Errorf("collapsed editor x = %d, want %d", r.editor.X, activityW)
	}
}

func TestComputeLayoutTinyStaysSane(t *testing.T) {
	for _, d := range [][2]int{{0, 0}, {1, 1}, {2, 2}, {3, 3}, {5, 1}, {4, 4}} {
		r := computeLayout(d[0], d[1], 28, false)
		cols := max(d[0], 1)
		for _, rect := range []tui.Rect{r.activity, r.side, r.tabs, r.editor, r.status} {
			if rect.W < 0 || rect.H < 0 {
				t.Errorf("cols=%d rows=%d: negative rect %+v", d[0], d[1], rect)
			}
		}
		if r.activity.W+r.side.W > cols {
			t.Errorf("cols=%d: activity+side (%d) exceeds width", cols, r.activity.W+r.side.W)
		}
	}
}
