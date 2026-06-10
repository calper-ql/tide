// Package layout is the session's tab/tile model: ordered tabs, each a
// split tree of pane leaves (spec: the daemon owns layout state, and crash
// survival means this structure must serialize). It is pure data — stdlib
// only, no goroutines, no locks; the daemon serializes access. Every
// operation is deterministic, mutations never panic (invalid input is an
// error or a no-op), and the whole structure round-trips through
// encoding/json losslessly.
package layout

import (
	"fmt"
	"slices"
)

// Rect is a screen rectangle in cells.
type Rect struct {
	X int `json:"x"`
	Y int `json:"y"`
	W int `json:"w"`
	H int `json:"h"`
}

// Dir is a split direction: which side of the target the new pane lands
// on, and therefore the axis the split's children share.
type Dir int

const (
	SplitRight Dir = iota // new pane to the right (side-by-side, vertical border)
	SplitDown             // new pane below (stacked, horizontal border)
)

// Layout is one session's complete arrangement: ordered tabs, one active.
type Layout struct {
	Tabs   []*Tab `json:"tabs"`
	Active int    `json:"active"`
}

// Tab is one tab: a binary-ish split tree of panes plus the focused pane.
type Tab struct {
	Name    string `json:"name,omitempty"` // optional user/title name; display falls back to index
	Root    *Node  `json:"root"`
	Focused string `json:"focused"` // pane id
}

// Node is a leaf (Pane != "") or a split (Children; same-direction runs
// are flattened into one node with N children and N ratios).
type Node struct {
	Pane     string    `json:"pane,omitempty"`
	Dir      Dir       `json:"dir,omitempty"`
	Children []*Node   `json:"children,omitempty"`
	Ratios   []float64 `json:"ratios,omitempty"` // sum to 1.0, one per child
}

// New returns a layout with one tab holding firstPane, focused.
func New(firstPane string) *Layout {
	l := &Layout{}
	l.NewTab(firstPane)
	return l
}

// ActiveTab returns the active tab, or nil when there are no tabs (or
// Active is out of range in a hand-built layout).
func (l *Layout) ActiveTab() *Tab {
	if l.Active < 0 || l.Active >= len(l.Tabs) {
		return nil
	}
	return l.Tabs[l.Active]
}

// PaneIDs returns every pane id across all tabs in stable order: tabs in
// order, leaves depth-first within each tab.
func (l *Layout) PaneIDs() []string {
	var ids []string
	for _, t := range l.Tabs {
		if t == nil {
			continue
		}
		walkLeaves(t.Root, func(pane string) { ids = append(ids, pane) })
	}
	return ids
}

// TabOf returns the index of the tab holding pane, or -1 if absent.
func (l *Layout) TabOf(pane string) int {
	if pane == "" {
		return -1
	}
	for i, t := range l.Tabs {
		if t == nil {
			continue
		}
		if _, _, _, ok := findLeaf(t.Root, pane); ok {
			return i
		}
	}
	return -1
}

// FocusedPane returns the active tab's focused pane, "" if no tabs.
func (l *Layout) FocusedPane() string {
	if t := l.ActiveTab(); t != nil {
		return t.Focused
	}
	return ""
}

// CountPanes returns the number of panes across all tabs.
func (l *Layout) CountPanes() int {
	n := 0
	for _, t := range l.Tabs {
		if t == nil {
			continue
		}
		walkLeaves(t.Root, func(string) { n++ })
	}
	return n
}

// NewTab appends a tab holding pane, activates it, and focuses the pane.
// An empty or already-present id is a no-op: pane ids identify leaves
// everywhere else, so a duplicate would corrupt routing.
func (l *Layout) NewTab(pane string) {
	if pane == "" || l.TabOf(pane) >= 0 {
		return
	}
	l.Tabs = append(l.Tabs, &Tab{Root: &Node{Pane: pane}, Focused: pane})
	l.Active = len(l.Tabs) - 1
}

// CloseTab removes tab i, returning its pane ids in stable (depth-first)
// order; out-of-range i is a no-op. Active is clamped so it keeps pointing
// at the same tab when possible, the nearest remaining one otherwise.
func (l *Layout) CloseTab(i int) (removed []string) {
	if i < 0 || i >= len(l.Tabs) {
		return nil
	}
	if t := l.Tabs[i]; t != nil {
		walkLeaves(t.Root, func(pane string) { removed = append(removed, pane) })
	}
	l.Tabs = slices.Delete(l.Tabs, i, i+1)
	if i < l.Active {
		l.Active--
	}
	if l.Active >= len(l.Tabs) {
		l.Active = len(l.Tabs) - 1
	}
	if l.Active < 0 {
		l.Active = 0
	}
	return removed
}

// SetActive activates tab i; out-of-range is a no-op.
func (l *Layout) SetActive(i int) {
	if i < 0 || i >= len(l.Tabs) {
		return
	}
	l.Active = i
}

// Split splits the target leaf 50/50, focuses newPane, and activates its
// tab. When d matches the parent split's direction the new leaf is
// inserted as a sibling right after target with half of target's ratio
// (same-direction runs stay flattened into one node); a differing
// direction nests a new {0.5, 0.5} split in the leaf's place.
func (l *Layout) Split(target string, d Dir, newPane string) error {
	if d != SplitRight && d != SplitDown {
		return fmt.Errorf("layout: invalid split direction %d", int(d))
	}
	if newPane == "" {
		return fmt.Errorf("layout: empty pane id")
	}
	if l.TabOf(newPane) >= 0 {
		return fmt.Errorf("layout: pane %q already exists", newPane)
	}
	ti := l.TabOf(target)
	if ti < 0 {
		return fmt.Errorf("layout: pane %q not found", target)
	}
	t := l.Tabs[ti]
	leaf, parent, idx, _ := findLeaf(t.Root, target)
	if parent != nil && parent.Dir == d {
		fixRatios(parent)
		half := parent.Ratios[idx] / 2
		parent.Ratios[idx] = half
		parent.Ratios = slices.Insert(parent.Ratios, idx+1, half)
		parent.Children = slices.Insert(parent.Children, idx+1, &Node{Pane: newPane})
	} else {
		*leaf = Node{
			Dir:      d,
			Children: []*Node{{Pane: target}, {Pane: newPane}},
			Ratios:   []float64{0.5, 0.5},
		}
	}
	t.Focused = newPane
	l.Active = ti
	return nil
}

// ClosePane removes the leaf holding id; an unknown id is a no-op. The
// freed ratio goes to the siblings proportionally; a split left with one
// child is replaced by that child (re-flattening any same-direction run
// that creates); a tab left empty is removed, clamping Active. If the
// closed pane was focused, focus moves to the nearest remaining leaf in
// the same tab — the previous sibling's last leaf, else the next
// sibling's first. Returns whether a tab was removed.
func (l *Layout) ClosePane(id string) (tabRemoved bool) {
	ti := l.TabOf(id)
	if ti < 0 {
		return false
	}
	t := l.Tabs[ti]
	_, parent, idx, ok := findLeaf(t.Root, id)
	if !ok {
		return false
	}
	if parent == nil {
		l.CloseTab(ti)
		return true
	}
	if t.Focused == id {
		if idx > 0 {
			t.Focused = lastLeaf(parent.Children[idx-1])
		} else {
			t.Focused = firstLeaf(parent.Children[idx+1])
		}
	}
	fixRatios(parent)
	freed := parent.Ratios[idx]
	parent.Children = slices.Delete(parent.Children, idx, idx+1)
	parent.Ratios = slices.Delete(parent.Ratios, idx, idx+1)
	rest := 0.0
	for _, r := range parent.Ratios {
		rest += r
	}
	if rest > 0 {
		// proportional redistribution: siblings keep their relative sizes
		// while absorbing the freed share, so the total is preserved.
		scale := (rest + freed) / rest
		for j := range parent.Ratios {
			parent.Ratios[j] *= scale
		}
	} else {
		parent.Ratios = equalRatios(len(parent.Children))
	}
	t.Root = normalize(t.Root)
	return false
}

// Focus focuses pane id and activates its tab; an unknown id is a no-op.
func (l *Layout) Focus(id string) {
	ti := l.TabOf(id)
	if ti < 0 {
		return
	}
	l.Active = ti
	l.Tabs[ti].Focused = id
}

// findLeaf returns the leaf holding pane plus its parent split and child
// index; parent is nil (idx -1) when the leaf is the root.
func findLeaf(n *Node, pane string) (leaf, parent *Node, idx int, ok bool) {
	if n == nil || pane == "" {
		return nil, nil, -1, false
	}
	if n.Pane == pane {
		return n, nil, -1, true
	}
	for i, c := range n.Children {
		if l, p, j, found := findLeaf(c, pane); found {
			if p == nil {
				return l, n, i, true
			}
			return l, p, j, true
		}
	}
	return nil, nil, -1, false
}

// walkLeaves visits every pane id under n in depth-first child order — the
// stable order every "all panes" query promises.
func walkLeaves(n *Node, visit func(pane string)) {
	if n == nil {
		return
	}
	if n.Pane != "" {
		visit(n.Pane)
		return
	}
	for _, c := range n.Children {
		walkLeaves(c, visit)
	}
}

// firstLeaf and lastLeaf pick the leaf of a subtree nearest to a removed
// neighbor: the last leaf of the previous sibling or the first of the next.
func firstLeaf(n *Node) string {
	for n != nil && n.Pane == "" {
		if len(n.Children) == 0 {
			return ""
		}
		n = n.Children[0]
	}
	if n == nil {
		return ""
	}
	return n.Pane
}

func lastLeaf(n *Node) string {
	for n != nil && n.Pane == "" {
		if len(n.Children) == 0 {
			return ""
		}
		n = n.Children[len(n.Children)-1]
	}
	if n == nil {
		return ""
	}
	return n.Pane
}

// fixRatios repairs a missing or mismatched ratio slice so indexed access
// is safe; mutating paths call it before touching Ratios.
func fixRatios(n *Node) {
	if len(n.Ratios) != len(n.Children) {
		n.Ratios = equalRatios(len(n.Children))
	}
}

func equalRatios(n int) []float64 {
	r := make([]float64, n)
	for i := range r {
		r[i] = 1 / float64(n)
	}
	return r
}

// normalize restores the structural invariants after a removal: ratios
// match children, a single-child split collapses into that child, and a
// child split sharing its parent's direction is spliced into the parent
// (its ratios scaled by the slot's ratio), so same-direction runs stay one
// node. Returns the node that takes n's place.
func normalize(n *Node) *Node {
	if n == nil || n.Pane != "" || len(n.Children) == 0 {
		return n
	}
	fixRatios(n)
	children := make([]*Node, 0, len(n.Children))
	ratios := make([]float64, 0, len(n.Children))
	for i, c := range n.Children {
		c = normalize(c)
		if c.Pane == "" && c.Dir == n.Dir && len(c.Children) > 0 {
			for j, gc := range c.Children {
				children = append(children, gc)
				ratios = append(ratios, n.Ratios[i]*c.Ratios[j])
			}
			continue
		}
		children = append(children, c)
		ratios = append(ratios, n.Ratios[i])
	}
	n.Children, n.Ratios = children, ratios
	if len(n.Children) == 1 {
		return n.Children[0]
	}
	return n
}
