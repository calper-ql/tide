package layout

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// step is one Split call when building a fixture tree.
type step struct {
	target string
	dir    Dir
	pane   string
}

func build(t *testing.T, first string, steps []step) *Layout {
	t.Helper()
	l := New(first)
	for _, s := range steps {
		if err := l.Split(s.target, s.dir, s.pane); err != nil {
			t.Fatalf("Split(%q, %v, %q): %v", s.target, s.dir, s.pane, err)
		}
	}
	return l
}

// treeString renders a tree compactly for table expectations: leaves as
// their pane id, splits as R[child:ratio ...] or D[child:ratio ...].
func treeString(n *Node) string {
	if n == nil {
		return "<nil>"
	}
	if n.Pane != "" {
		return n.Pane
	}
	d := "R"
	if n.Dir == SplitDown {
		d = "D"
	}
	parts := make([]string, len(n.Children))
	for i, c := range n.Children {
		r := "?"
		if i < len(n.Ratios) {
			r = strconv.FormatFloat(n.Ratios[i], 'f', 4, 64)
		}
		parts[i] = treeString(c) + ":" + r
	}
	return d + "[" + strings.Join(parts, " ") + "]"
}

func leafNodes(ids ...string) []*Node {
	out := make([]*Node, len(ids))
	for i, id := range ids {
		out[i] = &Node{Pane: id}
	}
	return out
}

// assertTiles asserts the tiling contract: every cell of area is covered
// by exactly one pane rect or vertical border, nothing escapes area, and
// every horizontal border is a one-row strip coinciding with the top row
// of its lower sibling's rect (the lower pane's bar, which doubles as the
// divider) — so it owns no cells of the tiling itself.
func assertTiles(t *testing.T, area Rect, rects map[string]Rect, borders []Border) {
	t.Helper()
	cover := make([]int, area.W*area.H)
	mark := func(r Rect, label string) {
		t.Helper()
		for y := r.Y; y < r.Y+r.H; y++ {
			for x := r.X; x < r.X+r.W; x++ {
				if x < area.X || x >= area.X+area.W || y < area.Y || y >= area.Y+area.H {
					t.Fatalf("%s rect %+v escapes area %+v", label, r, area)
				}
				cover[(y-area.Y)*area.W+(x-area.X)]++
			}
		}
	}
	for id, r := range rects {
		mark(r, "pane "+id)
	}
	for _, b := range borders {
		if b.Node == nil {
			t.Fatalf("border %+v has nil Node", b.Rect)
		}
		if b.Index < 0 || b.Index+1 >= len(b.Node.Children) {
			t.Fatalf("border %+v has invalid Index %d for %d children", b.Rect, b.Index, len(b.Node.Children))
		}
		if b.Rect.W <= 0 || b.Rect.H <= 0 {
			t.Fatalf("border %+v has no cells; empty borders must not be emitted", b.Rect)
		}
		if b.Vertical {
			if b.Rect.W != 1 {
				t.Fatalf("vertical border %+v must have W=1", b.Rect)
			}
			mark(b.Rect, "border")
		} else if b.Rect.H != 1 {
			t.Fatalf("horizontal border %+v must have H=1", b.Rect)
		}
	}
	for i, c := range cover {
		if c != 1 {
			t.Fatalf("cell (%d,%d) covered %d times, want exactly 1",
				area.X+i%area.W, area.Y+i/area.W, c)
		}
	}
	// horizontal borders overlap the tiling instead of joining it: every
	// cell of the strip must sit on the top row of the lower sibling's
	// rect, i.e. be the top-row cell of a pane under that sibling or the
	// top cell of a vertical border nested inside it.
	for _, b := range borders {
		if b.Vertical {
			continue
		}
		lower := make(map[string]bool)
		walkLeaves(b.Node.Children[b.Index+1], func(pane string) { lower[pane] = true })
		for x := b.Rect.X; x < b.Rect.X+b.Rect.W; x++ {
			ok := false
			for id, r := range rects {
				if lower[id] && r.H > 0 && b.Rect.Y == r.Y && x >= r.X && x < r.X+r.W {
					ok = true
					break
				}
			}
			if !ok {
				for _, vb := range borders {
					if vb.Vertical && vb.Rect.Y == b.Rect.Y && x == vb.Rect.X {
						ok = true
						break
					}
				}
			}
			if !ok {
				t.Fatalf("horizontal border %+v cell (%d,%d) is not on the top row of its lower sibling",
					b.Rect, x, b.Rect.Y)
			}
		}
	}
}

func TestNew(t *testing.T) {
	l := New("a")
	if len(l.Tabs) != 1 || l.Active != 0 {
		t.Fatalf("tabs = %d active = %d, want 1 tab active 0", len(l.Tabs), l.Active)
	}
	if got := l.FocusedPane(); got != "a" {
		t.Fatalf("FocusedPane = %q, want %q", got, "a")
	}
	if got := treeString(l.ActiveTab().Root); got != "a" {
		t.Fatalf("tree = %s, want a", got)
	}
	if l.CountPanes() != 1 {
		t.Fatalf("CountPanes = %d, want 1", l.CountPanes())
	}
}

func TestNewEmptyPaneIsSafe(t *testing.T) {
	l := New("")
	if len(l.Tabs) != 0 || l.ActiveTab() != nil || l.FocusedPane() != "" || l.CountPanes() != 0 {
		t.Fatalf("New(\"\") must yield an empty, safe layout, got %+v", l)
	}
}

func TestSplitShapes(t *testing.T) {
	tests := []struct {
		name        string
		steps       []step
		wantTree    string
		wantFocused string
	}{
		{
			name:        "nest right from single leaf",
			steps:       []step{{"a", SplitRight, "b"}},
			wantTree:    "R[a:0.5000 b:0.5000]",
			wantFocused: "b",
		},
		{
			name:        "nest down from single leaf",
			steps:       []step{{"a", SplitDown, "b"}},
			wantTree:    "D[a:0.5000 b:0.5000]",
			wantFocused: "b",
		},
		{
			name:        "same direction flattens, halving the target ratio",
			steps:       []step{{"a", SplitRight, "b"}, {"a", SplitRight, "c"}},
			wantTree:    "R[a:0.2500 c:0.2500 b:0.5000]",
			wantFocused: "c",
		},
		{
			name:        "flatten at a later index",
			steps:       []step{{"a", SplitRight, "b"}, {"b", SplitRight, "c"}},
			wantTree:    "R[a:0.5000 b:0.2500 c:0.2500]",
			wantFocused: "c",
		},
		{
			name:        "different direction nests in the leaf's place",
			steps:       []step{{"a", SplitRight, "b"}, {"b", SplitDown, "c"}},
			wantTree:    "R[a:0.5000 D[b:0.5000 c:0.5000]:0.5000]",
			wantFocused: "c",
		},
		{
			name:        "alternating directions nest deeply",
			steps:       []step{{"a", SplitRight, "b"}, {"b", SplitDown, "c"}, {"c", SplitRight, "d"}},
			wantTree:    "R[a:0.5000 D[b:0.5000 R[c:0.5000 d:0.5000]:0.5000]:0.5000]",
			wantFocused: "d",
		},
		{
			name:        "down runs flatten too",
			steps:       []step{{"a", SplitDown, "b"}, {"b", SplitDown, "c"}},
			wantTree:    "D[a:0.5000 b:0.2500 c:0.2500]",
			wantFocused: "c",
		},
		{
			name:        "nest in the middle of a flattened run",
			steps:       []step{{"a", SplitRight, "b"}, {"a", SplitRight, "c"}, {"c", SplitDown, "d"}},
			wantTree:    "R[a:0.2500 D[c:0.5000 d:0.5000]:0.2500 b:0.5000]",
			wantFocused: "d",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := build(t, "a", tt.steps)
			if got := treeString(l.ActiveTab().Root); got != tt.wantTree {
				t.Errorf("tree = %s, want %s", got, tt.wantTree)
			}
			if got := l.FocusedPane(); got != tt.wantFocused {
				t.Errorf("FocusedPane = %q, want %q", got, tt.wantFocused)
			}
		})
	}
}

func TestSplitErrors(t *testing.T) {
	tests := []struct {
		name   string
		target string
		dir    Dir
		pane   string
	}{
		{"empty new pane id", "a", SplitRight, ""},
		{"duplicate pane", "a", SplitRight, "b"},
		{"new pane equals target", "a", SplitRight, "a"},
		{"unknown target", "zz", SplitRight, "c"},
		{"invalid direction", "a", Dir(9), "c"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := build(t, "a", []step{{"a", SplitRight, "b"}})
			before := treeString(l.ActiveTab().Root)
			if err := l.Split(tt.target, tt.dir, tt.pane); err == nil {
				t.Fatal("expected an error")
			}
			if got := treeString(l.ActiveTab().Root); got != before {
				t.Errorf("failed split mutated the tree: %s -> %s", before, got)
			}
			if got := l.FocusedPane(); got != "b" {
				t.Errorf("failed split moved focus to %q", got)
			}
		})
	}
}

func TestSplitFocusesAndActivatesTab(t *testing.T) {
	l := New("a")
	l.NewTab("x")
	if l.Active != 1 {
		t.Fatalf("Active = %d after NewTab, want 1", l.Active)
	}
	if err := l.Split("a", SplitRight, "b"); err != nil {
		t.Fatal(err)
	}
	if l.Active != 0 {
		t.Errorf("Active = %d, want 0 (split must activate the target's tab)", l.Active)
	}
	if got := l.FocusedPane(); got != "b" {
		t.Errorf("FocusedPane = %q, want b", got)
	}
	if got := l.Tabs[1].Focused; got != "x" {
		t.Errorf("other tab's focus = %q, want x", got)
	}
}

func TestClosePane(t *testing.T) {
	tests := []struct {
		name           string
		steps          []step
		close          string
		wantTree       string
		wantFocused    string
		wantTabRemoved bool
	}{
		{
			name:        "two panes collapse to a leaf",
			steps:       []step{{"a", SplitRight, "b"}},
			close:       "b",
			wantTree:    "a",
			wantFocused: "a",
		},
		{
			name:        "siblings absorb the freed ratio proportionally",
			steps:       []step{{"a", SplitRight, "b"}, {"a", SplitRight, "c"}},
			close:       "a",
			wantTree:    "R[c:0.3333 b:0.6667]",
			wantFocused: "c",
		},
		{
			name:        "middle of three redistributes by weight",
			steps:       []step{{"a", SplitRight, "b"}, {"b", SplitRight, "c"}},
			close:       "b",
			wantTree:    "R[a:0.6667 c:0.3333]",
			wantFocused: "c",
		},
		{
			name:        "single-child split collapses into the child",
			steps:       []step{{"a", SplitRight, "b"}, {"b", SplitDown, "c"}},
			close:       "b",
			wantTree:    "R[a:0.5000 c:0.5000]",
			wantFocused: "c",
		},
		{
			name:        "collapse splices a same-direction child back in",
			steps:       []step{{"a", SplitRight, "b"}, {"b", SplitDown, "c"}, {"c", SplitRight, "d"}},
			close:       "b",
			wantTree:    "R[a:0.5000 c:0.2500 d:0.2500]",
			wantFocused: "d",
		},
		{
			name:           "sole pane removes the tab",
			steps:          nil,
			close:          "a",
			wantTabRemoved: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := build(t, "a", tt.steps)
			before := l.CountPanes()
			if got := l.ClosePane(tt.close); got != tt.wantTabRemoved {
				t.Fatalf("ClosePane = %v, want %v", got, tt.wantTabRemoved)
			}
			if l.TabOf(tt.close) != -1 {
				t.Errorf("pane %q still present after close", tt.close)
			}
			if got := l.CountPanes(); got != before-1 {
				t.Errorf("CountPanes = %d, want %d", got, before-1)
			}
			if tt.wantTabRemoved {
				if len(l.Tabs) != 0 || l.ActiveTab() != nil || l.Active != 0 {
					t.Errorf("tab not cleanly removed: tabs = %d active = %d", len(l.Tabs), l.Active)
				}
				return
			}
			if got := treeString(l.ActiveTab().Root); got != tt.wantTree {
				t.Errorf("tree = %s, want %s", got, tt.wantTree)
			}
			if got := l.FocusedPane(); got != tt.wantFocused {
				t.Errorf("FocusedPane = %q, want %q", got, tt.wantFocused)
			}
		})
	}
}

func TestClosePaneFocusTransfer(t *testing.T) {
	tests := []struct {
		name        string
		steps       []step
		focus       string
		close       string
		wantFocused string
	}{
		{
			name:        "focused middle goes to the previous sibling",
			steps:       []step{{"a", SplitRight, "b"}, {"b", SplitRight, "c"}},
			focus:       "b",
			close:       "b",
			wantFocused: "a",
		},
		{
			name:        "focused first goes to the next sibling",
			steps:       []step{{"a", SplitRight, "b"}, {"b", SplitRight, "c"}},
			focus:       "a",
			close:       "a",
			wantFocused: "b",
		},
		{
			name:        "focused last goes to the previous sibling",
			steps:       []step{{"a", SplitRight, "b"}, {"b", SplitRight, "c"}},
			focus:       "c",
			close:       "c",
			wantFocused: "b",
		},
		{
			name:        "closing a non-focused pane keeps focus",
			steps:       []step{{"a", SplitRight, "b"}, {"b", SplitRight, "c"}},
			focus:       "a",
			close:       "c",
			wantFocused: "a",
		},
		{
			name:        "next sibling descends to its first leaf",
			steps:       []step{{"a", SplitRight, "b"}, {"b", SplitDown, "c"}},
			focus:       "a",
			close:       "a",
			wantFocused: "b",
		},
		{
			name:        "previous sibling descends to its last leaf",
			steps:       []step{{"a", SplitRight, "b"}, {"a", SplitDown, "c"}},
			focus:       "b",
			close:       "b",
			wantFocused: "c",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := build(t, "a", tt.steps)
			l.Focus(tt.focus)
			l.ClosePane(tt.close)
			if got := l.FocusedPane(); got != tt.wantFocused {
				t.Errorf("FocusedPane = %q, want %q", got, tt.wantFocused)
			}
		})
	}
}

func TestClosePaneUnknown(t *testing.T) {
	l := build(t, "a", []step{{"a", SplitRight, "b"}})
	before := treeString(l.ActiveTab().Root)
	if l.ClosePane("zz") {
		t.Fatal("ClosePane on an unknown id must return false")
	}
	if got := treeString(l.ActiveTab().Root); got != before {
		t.Errorf("unknown close mutated the tree: %s -> %s", before, got)
	}
}

func TestClosePaneInBackgroundTab(t *testing.T) {
	l := build(t, "a", []step{{"a", SplitRight, "b"}})
	l.NewTab("x")
	if l.ClosePane("a") {
		t.Fatal("closing one of two panes must not remove the tab")
	}
	if l.Active != 1 {
		t.Errorf("Active = %d, want 1 (background close must not steal activation)", l.Active)
	}
	if got := treeString(l.Tabs[0].Root); got != "b" {
		t.Errorf("background tab tree = %s, want b", got)
	}
	// the background tab's sole remaining pane: closing it removes the
	// tab and Active clamps down to keep pointing at the same tab.
	if !l.ClosePane("b") {
		t.Fatal("closing the sole pane must remove the tab")
	}
	if len(l.Tabs) != 1 || l.Active != 0 || l.FocusedPane() != "x" {
		t.Errorf("tabs = %d active = %d focused = %q, want 1/0/x", len(l.Tabs), l.Active, l.FocusedPane())
	}
}

func TestCloseTab(t *testing.T) {
	tests := []struct {
		name        string
		active      int
		close       int
		wantActive  int
		wantLen     int
		wantRemoved []string
	}{
		{"out of range high", 0, 3, 0, 3, nil},
		{"negative index", 0, -1, 0, 3, nil},
		{"before active decrements it", 2, 0, 1, 2, []string{"a"}},
		{"active middle stays in place", 1, 1, 1, 2, []string{"b"}},
		{"active last clamps down", 2, 2, 1, 2, []string{"c"}},
		{"after active leaves it alone", 0, 2, 0, 2, []string{"c"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := New("a")
			l.NewTab("b")
			l.NewTab("c")
			l.SetActive(tt.active)
			removed := l.CloseTab(tt.close)
			if !reflect.DeepEqual(removed, tt.wantRemoved) {
				t.Errorf("removed = %v, want %v", removed, tt.wantRemoved)
			}
			if l.Active != tt.wantActive {
				t.Errorf("Active = %d, want %d", l.Active, tt.wantActive)
			}
			if len(l.Tabs) != tt.wantLen {
				t.Errorf("len(Tabs) = %d, want %d", len(l.Tabs), tt.wantLen)
			}
		})
	}
}

func TestCloseTabRemovedOrder(t *testing.T) {
	// removed ids come back in stable depth-first order: a右b nests a
	// down-split in a's slot, so the order is a, c, b.
	l := build(t, "a", []step{{"a", SplitRight, "b"}, {"a", SplitDown, "c"}})
	removed := l.CloseTab(0)
	want := []string{"a", "c", "b"}
	if !reflect.DeepEqual(removed, want) {
		t.Fatalf("removed = %v, want %v", removed, want)
	}
}

func TestCloseTabUntilEmpty(t *testing.T) {
	l := New("a")
	l.NewTab("b")
	l.CloseTab(0)
	l.CloseTab(0)
	if len(l.Tabs) != 0 || l.Active != 0 {
		t.Fatalf("tabs = %d active = %d, want 0/0", len(l.Tabs), l.Active)
	}
	if l.ActiveTab() != nil || l.FocusedPane() != "" || l.CountPanes() != 0 {
		t.Fatal("queries on an empty layout must be safe and empty")
	}
	if removed := l.CloseTab(0); removed != nil {
		t.Fatalf("closing in an empty layout returned %v", removed)
	}
	l.NewTab("c")
	if l.Active != 0 || l.FocusedPane() != "c" {
		t.Fatalf("layout did not recover from empty: active = %d focused = %q", l.Active, l.FocusedPane())
	}
}

func TestNewTab(t *testing.T) {
	l := New("a")
	l.NewTab("b")
	if len(l.Tabs) != 2 || l.Active != 1 || l.FocusedPane() != "b" {
		t.Fatalf("tabs = %d active = %d focused = %q, want 2/1/b", len(l.Tabs), l.Active, l.FocusedPane())
	}
	l.NewTab("")  // no-op
	l.NewTab("a") // duplicate id, no-op
	if len(l.Tabs) != 2 || l.Active != 1 {
		t.Fatalf("invalid NewTab mutated the layout: tabs = %d active = %d", len(l.Tabs), l.Active)
	}
}

func TestSetActive(t *testing.T) {
	l := New("a")
	l.NewTab("b")
	l.SetActive(0)
	if l.Active != 0 {
		t.Fatalf("Active = %d, want 0", l.Active)
	}
	l.SetActive(5)
	l.SetActive(-1)
	if l.Active != 0 {
		t.Fatalf("out-of-range SetActive mutated Active to %d", l.Active)
	}
}

func TestFocus(t *testing.T) {
	l := build(t, "a", []step{{"a", SplitRight, "b"}})
	l.NewTab("x")
	l.Focus("a")
	if l.Active != 0 || l.FocusedPane() != "a" {
		t.Fatalf("active = %d focused = %q, want 0/a", l.Active, l.FocusedPane())
	}
	l.Focus("nope")
	if l.Active != 0 || l.FocusedPane() != "a" {
		t.Fatal("unknown Focus must be a no-op")
	}
	l.Focus("x")
	if l.Active != 1 || l.FocusedPane() != "x" {
		t.Fatalf("active = %d focused = %q, want 1/x", l.Active, l.FocusedPane())
	}
}

func TestQueries(t *testing.T) {
	l := build(t, "a", []step{{"a", SplitRight, "b"}, {"a", SplitDown, "c"}})
	l.NewTab("x")
	if err := l.Split("x", SplitRight, "y"); err != nil {
		t.Fatal(err)
	}
	if got, want := l.PaneIDs(), []string{"a", "c", "b", "x", "y"}; !reflect.DeepEqual(got, want) {
		t.Errorf("PaneIDs = %v, want %v", got, want)
	}
	if got := l.CountPanes(); got != 5 {
		t.Errorf("CountPanes = %d, want 5", got)
	}
	for pane, want := range map[string]int{"a": 0, "b": 0, "c": 0, "x": 1, "y": 1, "z": -1, "": -1} {
		if got := l.TabOf(pane); got != want {
			t.Errorf("TabOf(%q) = %d, want %d", pane, got, want)
		}
	}
	if got := l.FocusedPane(); got != "y" {
		t.Errorf("FocusedPane = %q, want y", got)
	}

	var empty Layout
	if empty.ActiveTab() != nil || empty.FocusedPane() != "" || empty.CountPanes() != 0 ||
		empty.TabOf("a") != -1 || len(empty.PaneIDs()) != 0 {
		t.Error("zero-value layout queries must be safe and empty")
	}
}

// borderShape is a Border stripped of its node pointer for comparison.
type borderShape struct {
	Rect     Rect
	Vertical bool
	Index    int
}

func shapes(borders []Border) []borderShape {
	if len(borders) == 0 {
		return nil
	}
	out := make([]borderShape, len(borders))
	for i, b := range borders {
		out[i] = borderShape{b.Rect, b.Vertical, b.Index}
	}
	return out
}

func TestComputeExactRects(t *testing.T) {
	tests := []struct {
		name        string
		steps       []step
		area        Rect
		wantRects   map[string]Rect
		wantBorders []borderShape
	}{
		{
			name:      "single pane fills the area",
			area:      Rect{0, 0, 80, 24},
			wantRects: map[string]Rect{"a": {0, 0, 80, 24}},
		},
		{
			name:  "right split shares width minus the border, last absorbs the odd cell",
			steps: []step{{"a", SplitRight, "b"}},
			area:  Rect{0, 0, 80, 24},
			wantRects: map[string]Rect{
				"a": {0, 0, 39, 24},
				"b": {40, 0, 40, 24},
			},
			wantBorders: []borderShape{{Rect{39, 0, 1, 24}, true, 0}},
		},
		{
			name:  "down split tiles the full height; the divider is b's bar row",
			steps: []step{{"a", SplitDown, "b"}},
			area:  Rect{0, 0, 80, 24},
			wantRects: map[string]Rect{
				"a": {0, 0, 80, 12},
				"b": {0, 12, 80, 12},
			},
			wantBorders: []borderShape{{Rect{0, 12, 80, 1}, false, 0}},
		},
		{
			name:  "offset areas keep absolute coordinates",
			steps: []step{{"a", SplitRight, "b"}},
			area:  Rect{10, 5, 21, 7},
			wantRects: map[string]Rect{
				"a": {10, 5, 10, 7},
				"b": {21, 5, 10, 7},
			},
			wantBorders: []borderShape{{Rect{20, 5, 1, 7}, true, 0}},
		},
		{
			name:  "three-way flattened run has two indexed borders",
			steps: []step{{"a", SplitRight, "b"}, {"a", SplitRight, "c"}},
			area:  Rect{0, 0, 80, 24},
			wantRects: map[string]Rect{
				"a": {0, 0, 19, 24},
				"c": {20, 0, 19, 24},
				"b": {40, 0, 40, 24},
			},
			wantBorders: []borderShape{
				{Rect{19, 0, 1, 24}, true, 0},
				{Rect{39, 0, 1, 24}, true, 1},
			},
		},
		{
			name:  "nested split divides its own slot",
			steps: []step{{"a", SplitRight, "b"}, {"b", SplitDown, "c"}},
			area:  Rect{0, 0, 80, 24},
			wantRects: map[string]Rect{
				"a": {0, 0, 39, 24},
				"b": {40, 0, 40, 12},
				"c": {40, 12, 40, 12},
			},
			wantBorders: []borderShape{
				{Rect{39, 0, 1, 24}, true, 0},
				{Rect{40, 12, 40, 1}, false, 0},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := build(t, "a", tt.steps)
			rects, borders := l.ActiveTab().Compute(tt.area)
			if !reflect.DeepEqual(rects, tt.wantRects) {
				t.Errorf("rects = %v, want %v", rects, tt.wantRects)
			}
			if got := shapes(borders); !reflect.DeepEqual(got, tt.wantBorders) {
				t.Errorf("borders = %v, want %v", got, tt.wantBorders)
			}
			assertTiles(t, tt.area, rects, borders)
		})
	}
}

func TestComputeMinimumClamp(t *testing.T) {
	tests := []struct {
		name   string
		dir    Dir
		panes  []string
		ratios []float64
		area   Rect
		want   map[string]int // pane -> extent along the split axis
	}{
		{
			name:   "tiny width ratio clamps to MinPaneW",
			dir:    SplitRight,
			panes:  []string{"a", "b"},
			ratios: []float64{0.05, 0.95},
			area:   Rect{0, 0, 40, 10},
			want:   map[string]int{"a": 4, "b": 35},
		},
		{
			name:   "tiny height ratio clamps to MinPaneH",
			dir:    SplitDown,
			panes:  []string{"a", "b"},
			ratios: []float64{0.01, 0.99},
			area:   Rect{0, 0, 10, 20},
			want:   map[string]int{"a": 3, "b": 17},
		},
		{
			name:   "several clamped children all take from the largest",
			dir:    SplitRight,
			panes:  []string{"a", "b", "c"},
			ratios: []float64{0.02, 0.02, 0.96},
			area:   Rect{0, 0, 30, 5},
			want:   map[string]int{"a": 4, "b": 4, "c": 20},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tab := &Tab{
				Root:    &Node{Dir: tt.dir, Children: leafNodes(tt.panes...), Ratios: tt.ratios},
				Focused: tt.panes[0],
			}
			rects, borders := tab.Compute(tt.area)
			for pane, want := range tt.want {
				got := rects[pane].W
				if tt.dir == SplitDown {
					got = rects[pane].H
				}
				if got != want {
					t.Errorf("pane %s extent = %d, want %d", pane, got, want)
				}
			}
			assertTiles(t, tt.area, rects, borders)
		})
	}
}

func TestComputeDegenerate(t *testing.T) {
	t.Run("zero and negative areas return nil maps", func(t *testing.T) {
		l := build(t, "a", []step{{"a", SplitRight, "b"}})
		for _, area := range []Rect{{0, 0, 0, 0}, {0, 0, -3, 5}, {0, 0, 5, 0}, {2, 2, 5, -1}} {
			rects, borders := l.ActiveTab().Compute(area)
			if rects != nil || borders != nil {
				t.Errorf("Compute(%+v) = %v, %v; want nil, nil", area, rects, borders)
			}
		}
	})
	t.Run("nil tab and nil root are safe", func(t *testing.T) {
		var tab *Tab
		if rects, borders := tab.Compute(Rect{0, 0, 10, 10}); rects != nil || borders != nil {
			t.Error("nil tab must return nil maps")
		}
		if rects, borders := (&Tab{}).Compute(Rect{0, 0, 10, 10}); rects != nil || borders != nil {
			t.Error("nil root must return nil maps")
		}
	})
	t.Run("1x1 with a split still tiles", func(t *testing.T) {
		l := build(t, "a", []step{{"a", SplitRight, "b"}})
		area := Rect{0, 0, 1, 1}
		rects, borders := l.ActiveTab().Compute(area)
		if len(rects) != 2 {
			t.Fatalf("len(rects) = %d, want 2 (every pane gets a rect)", len(rects))
		}
		assertTiles(t, area, rects, borders)
	})
	t.Run("2x2 with a split still tiles", func(t *testing.T) {
		l := build(t, "a", []step{{"a", SplitRight, "b"}})
		area := Rect{0, 0, 2, 2}
		rects, borders := l.ActiveTab().Compute(area)
		assertTiles(t, area, rects, borders)
	})
	t.Run("huge split count in a tiny area", func(t *testing.T) {
		l := New("p0")
		for i := 1; i < 20; i++ {
			if err := l.Split("p0", SplitRight, fmt.Sprintf("p%d", i)); err != nil {
				t.Fatal(err)
			}
		}
		area := Rect{0, 0, 5, 3}
		rects, borders := l.ActiveTab().Compute(area)
		if len(rects) != 20 {
			t.Fatalf("len(rects) = %d, want 20", len(rects))
		}
		assertTiles(t, area, rects, borders)
	})
	t.Run("down run taller than the area", func(t *testing.T) {
		l := New("p0")
		for i := 1; i < 10; i++ {
			if err := l.Split("p0", SplitDown, fmt.Sprintf("p%d", i)); err != nil {
				t.Fatal(err)
			}
		}
		area := Rect{0, 0, 3, 4}
		rects, borders := l.ActiveTab().Compute(area)
		if len(rects) != 10 {
			t.Fatalf("len(rects) = %d, want 10", len(rects))
		}
		assertTiles(t, area, rects, borders)
	})
}

func TestComputeTilingProperty(t *testing.T) {
	fixtures := []struct {
		name  string
		build func(t *testing.T) *Layout
	}{
		{"single pane", func(t *testing.T) *Layout { return New("a") }},
		{"one right split", func(t *testing.T) *Layout {
			return build(t, "a", []step{{"a", SplitRight, "b"}})
		}},
		{"mixed nest", func(t *testing.T) *Layout {
			return build(t, "a", []step{
				{"a", SplitRight, "b"}, {"b", SplitDown, "c"},
				{"c", SplitRight, "d"}, {"a", SplitDown, "e"},
			})
		}},
		{"20-pane right run", func(t *testing.T) *Layout {
			l := New("p0")
			for i := 1; i < 20; i++ {
				if err := l.Split("p0", SplitRight, fmt.Sprintf("p%d", i)); err != nil {
					t.Fatal(err)
				}
			}
			return l
		}},
		{"alternating 8 panes", func(t *testing.T) *Layout {
			l := New("p0")
			for i := 1; i < 8; i++ {
				d := SplitRight
				if i%2 == 1 {
					d = SplitDown
				}
				if err := l.Split(fmt.Sprintf("p%d", i-1), d, fmt.Sprintf("p%d", i)); err != nil {
					t.Fatal(err)
				}
			}
			return l
		}},
	}
	areas := []Rect{
		{0, 0, 80, 24}, {2, 1, 33, 9}, {0, 0, 5, 5}, {0, 0, 4, 3},
		{0, 0, 2, 2}, {0, 0, 1, 1}, {0, 0, 3, 1}, {0, 0, 1, 3},
		{7, 11, 200, 50},
	}
	for _, f := range fixtures {
		for _, area := range areas {
			t.Run(fmt.Sprintf("%s in %dx%d", f.name, area.W, area.H), func(t *testing.T) {
				l := f.build(t)
				tab := l.ActiveTab()
				rects, borders := tab.Compute(area)
				if len(rects) != l.CountPanes() {
					t.Fatalf("len(rects) = %d, want %d (every pane gets a rect)", len(rects), l.CountPanes())
				}
				assertTiles(t, area, rects, borders)
				// dragging any border, either way, must preserve the tiling.
				for _, delta := range []int{7, -7} {
					_, bs := tab.Compute(area)
					for _, b := range bs {
						tab.DragBorder(b, delta, area)
						r2, b2 := tab.Compute(area)
						assertTiles(t, area, r2, b2)
					}
				}
			})
		}
	}
}

func TestDragBorder(t *testing.T) {
	tests := []struct {
		name  string
		dir   Dir
		area  Rect
		delta int
		want  [2]int // a and b extents along the drag axis after the drag
	}{
		{"right grows the left child", SplitRight, Rect{0, 0, 80, 24}, 10, [2]int{49, 30}},
		{"left clamps at MinPaneW", SplitRight, Rect{0, 0, 80, 24}, -100, [2]int{4, 75}},
		{"right clamps at MinPaneW", SplitRight, Rect{0, 0, 80, 24}, 100, [2]int{75, 4}},
		{"down grows the top child", SplitDown, Rect{0, 0, 80, 24}, 5, [2]int{17, 7}},
		{"up clamps at MinPaneH", SplitDown, Rect{0, 0, 80, 24}, -100, [2]int{3, 21}},
		{"degenerate area cannot drag", SplitRight, Rect{0, 0, 7, 3}, 3, [2]int{3, 3}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := build(t, "a", []step{{"a", tt.dir, "b"}})
			tab := l.ActiveTab()
			_, borders := tab.Compute(tt.area)
			if len(borders) != 1 {
				t.Fatalf("len(borders) = %d, want 1", len(borders))
			}
			tab.DragBorder(borders[0], tt.delta, tt.area)
			rects, bs := tab.Compute(tt.area)
			extent := func(r Rect) int {
				if tt.dir == SplitDown {
					return r.H
				}
				return r.W
			}
			if got := [2]int{extent(rects["a"]), extent(rects["b"])}; got != tt.want {
				t.Errorf("extents = %v, want %v", got, tt.want)
			}
			assertTiles(t, tt.area, rects, bs)
		})
	}
}

func TestDragBorderMiddleOfThree(t *testing.T) {
	// R[a:0.5 b:0.25 c:0.25] in 80 columns lays out as 39, 19, 20; dragging
	// the divider between b and c must not move a by even one cell.
	l := build(t, "a", []step{{"a", SplitRight, "b"}, {"b", SplitRight, "c"}})
	tab := l.ActiveTab()
	area := Rect{0, 0, 80, 24}
	rects, borders := tab.Compute(area)
	if got := [3]int{rects["a"].W, rects["b"].W, rects["c"].W}; got != [3]int{39, 19, 20} {
		t.Fatalf("initial widths = %v, want [39 19 20]", got)
	}
	if len(borders) != 2 || borders[1].Index != 1 {
		t.Fatalf("borders = %+v, want two with the second at Index 1", shapes(borders))
	}
	tab.DragBorder(borders[1], 10, area)
	rects, borders = tab.Compute(area)
	if got := [3]int{rects["a"].W, rects["b"].W, rects["c"].W}; got != [3]int{39, 29, 10} {
		t.Errorf("widths after drag = %v, want [39 29 10]", got)
	}
	assertTiles(t, area, rects, borders)
}

func TestDragBorderInvalid(t *testing.T) {
	area := Rect{0, 0, 80, 24}
	other := build(t, "x", []step{{"x", SplitRight, "y"}})
	_, otherBorders := other.ActiveTab().Compute(area)
	tests := []struct {
		name string
		drag func(tab *Tab, b Border)
	}{
		{"zero delta", func(tab *Tab, b Border) { tab.DragBorder(b, 0, area) }},
		{"index out of range", func(tab *Tab, b Border) {
			b.Index = 1
			tab.DragBorder(b, 5, area)
		}},
		{"negative index", func(tab *Tab, b Border) {
			b.Index = -1
			tab.DragBorder(b, 5, area)
		}},
		{"foreign node", func(tab *Tab, _ Border) { tab.DragBorder(otherBorders[0], 5, area) }},
		{"nil node", func(tab *Tab, _ Border) { tab.DragBorder(Border{}, 5, area) }},
		{"zero area", func(tab *Tab, b Border) { tab.DragBorder(b, 5, Rect{}) }},
		{"nil tab", func(_ *Tab, b Border) {
			var nilTab *Tab
			nilTab.DragBorder(b, 5, area)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := build(t, "a", []step{{"a", SplitRight, "b"}})
			tab := l.ActiveTab()
			_, borders := tab.Compute(area)
			before := append([]float64(nil), tab.Root.Ratios...)
			tt.drag(tab, borders[0])
			if !reflect.DeepEqual(tab.Root.Ratios, before) {
				t.Errorf("ratios mutated: %v -> %v", before, tab.Root.Ratios)
			}
		})
	}
}

func TestJSONRoundTrip(t *testing.T) {
	tests := []struct {
		name  string
		build func(t *testing.T) *Layout
	}{
		{"zero value", func(t *testing.T) *Layout { return &Layout{} }},
		{"single pane", func(t *testing.T) *Layout { return New("a") }},
		{"multi-tab with names and nested splits", func(t *testing.T) *Layout {
			l := build(t, "a", []step{
				{"a", SplitRight, "b"}, {"b", SplitDown, "c"}, {"c", SplitRight, "d"},
			})
			l.Tabs[0].Name = "work"
			l.NewTab("x")
			if err := l.Split("x", SplitDown, "y"); err != nil {
				t.Fatal(err)
			}
			l.Focus("c")
			return l
		}},
		{"dragged ratios survive", func(t *testing.T) *Layout {
			l := build(t, "a", []step{{"a", SplitRight, "b"}})
			area := Rect{0, 0, 80, 24}
			tab := l.ActiveTab()
			_, borders := tab.Compute(area)
			tab.DragBorder(borders[0], 13, area)
			return l
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := tt.build(t)
			data, err := json.Marshal(l)
			if err != nil {
				t.Fatal(err)
			}
			var got Layout
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatal(err)
			}
			if got.Active != l.Active {
				t.Errorf("Active = %d, want %d", got.Active, l.Active)
			}
			if len(got.Tabs) != len(l.Tabs) {
				t.Fatalf("len(Tabs) = %d, want %d", len(got.Tabs), len(l.Tabs))
			}
			for i := range l.Tabs {
				if got.Tabs[i].Name != l.Tabs[i].Name || got.Tabs[i].Focused != l.Tabs[i].Focused {
					t.Errorf("tab %d name/focused = %q/%q, want %q/%q",
						i, got.Tabs[i].Name, got.Tabs[i].Focused, l.Tabs[i].Name, l.Tabs[i].Focused)
				}
				if gs, ws := treeString(got.Tabs[i].Root), treeString(l.Tabs[i].Root); gs != ws {
					t.Errorf("tab %d tree = %s, want %s", i, gs, ws)
				}
			}
			again, err := json.Marshal(&got)
			if err != nil {
				t.Fatal(err)
			}
			if string(again) != string(data) {
				t.Errorf("second marshal differs:\n%s\n%s", data, again)
			}
			// the round-tripped layout computes identical geometry.
			if tab, gotTab := l.ActiveTab(), got.ActiveTab(); tab != nil {
				area := Rect{0, 0, 80, 24}
				r1, _ := tab.Compute(area)
				r2, _ := gotTab.Compute(area)
				if !reflect.DeepEqual(r1, r2) {
					t.Errorf("rects after round trip = %v, want %v", r2, r1)
				}
			}
		})
	}
}

func TestSplitLeftAndUpInsertBefore(t *testing.T) {
	l := New("a")
	if err := l.Split("a", SplitLeft, "b"); err != nil {
		t.Fatal(err)
	}
	root := l.Tabs[0].Root
	if root.Dir != SplitRight || root.Children[0].Pane != "b" || root.Children[1].Pane != "a" {
		t.Fatalf("SplitLeft must insert before on the horizontal axis: %+v", root)
	}
	// Flattening run: another left split of "b" inserts before it.
	if err := l.Split("b", SplitLeft, "c"); err != nil {
		t.Fatal(err)
	}
	if len(root.Children) != 3 || root.Children[0].Pane != "c" || root.Children[1].Pane != "b" {
		t.Fatalf("SplitLeft must flatten into the run before the target: %+v", root.Children)
	}
	// SplitUp nests on the vertical axis with the new pane first.
	if err := l.Split("a", SplitUp, "d"); err != nil {
		t.Fatal(err)
	}
	nested := root.Children[2]
	if nested.Dir != SplitDown || nested.Children[0].Pane != "d" || nested.Children[1].Pane != "a" {
		t.Fatalf("SplitUp must nest with the new pane above: %+v", nested)
	}
	if l.FocusedPane() != "d" {
		t.Fatalf("focus = %q, want the new pane", l.FocusedPane())
	}
	rects, borders := l.Tabs[0].Compute(Rect{X: 0, Y: 0, W: 90, H: 30})
	assertTiles(t, Rect{X: 0, Y: 0, W: 90, H: 30}, rects, borders)
}

func TestSplitNodeWrapsContainerFullExtent(t *testing.T) {
	// The user-ruled boundary case: a stacked container split right gets a
	// full-height neighbor; a second one flattens into the same row.
	l := New("a")
	if err := l.Split("a", SplitDown, "b"); err != nil {
		t.Fatal(err)
	}
	stack := l.Tabs[0].Root
	if err := l.SplitNode(0, stack, SplitRight, "c"); err != nil {
		t.Fatal(err)
	}
	root := l.Tabs[0].Root
	if root.Dir != SplitRight || len(root.Children) != 2 {
		t.Fatalf("root = %+v", root)
	}
	if root.Children[0].Dir != SplitDown || root.Children[1].Pane != "c" {
		t.Fatalf("expected [stack, c]: %+v", root.Children)
	}
	rects, _ := l.Tabs[0].Compute(Rect{X: 0, Y: 0, W: 90, H: 30})
	if rects["c"].H != 30 {
		t.Fatalf("c height = %d, want full 30", rects["c"].H)
	}
	// Same-axis flatten: another right split of the stack joins the row.
	if err := l.SplitNode(0, root.Children[0], SplitRight, "d"); err != nil {
		t.Fatal(err)
	}
	if len(root.Children) != 3 || root.Children[1].Pane != "d" {
		t.Fatalf("flatten failed: %+v", root.Children)
	}
	rects, borders := l.Tabs[0].Compute(Rect{X: 0, Y: 0, W: 90, H: 30})
	assertTiles(t, Rect{X: 0, Y: 0, W: 90, H: 30}, rects, borders)
}

func TestTopEdgeNodeClimbsStacks(t *testing.T) {
	l := New("a")
	_ = l.Split("a", SplitDown, "b")  // stack [a, b]
	_ = l.Split("b", SplitRight, "c") // stack [a, [b | c]]
	tab := l.Tabs[0]
	// a leads the stack: its top edge is the whole stack (the root).
	if n := tab.TopEdgeNode("a"); n != tab.Root {
		t.Fatalf("a's top edge should be the root stack, got %+v", n)
	}
	// b's bar is the interior divider's row but b itself is child 0 of
	// nothing stacked — its top edge is just b.
	if n := tab.TopEdgeNode("b"); n == nil || n.Pane != "b" {
		t.Fatalf("b's top edge should be b, got %+v", n)
	}
	// c sits beside b: its segment of the top edge is its own.
	if n := tab.TopEdgeNode("c"); n == nil || n.Pane != "c" {
		t.Fatalf("c's top edge should be c, got %+v", n)
	}
}
