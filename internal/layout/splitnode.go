package layout

import "fmt"

// SplitNode inserts newPane beside an arbitrary node — the boundary-click
// primitive. Splitting beside a whole container is what makes "new pane
// right of this stack, full height" mean the stack, not one of its panes:
// the new pane spans target's entire extent on the cross axis. Same-axis
// runs flatten exactly like Split; Left/Up insert before. The new pane is
// focused and its tab activated.
func (l *Layout) SplitNode(tabIdx int, target *Node, d Dir, newPane string) error {
	axis, before := d, false
	switch d {
	case SplitLeft:
		axis, before = SplitRight, true
	case SplitUp:
		axis, before = SplitDown, true
	case SplitRight, SplitDown:
	default:
		return fmt.Errorf("layout: invalid split direction %d", int(d))
	}
	if newPane == "" {
		return fmt.Errorf("layout: empty pane id")
	}
	if l.TabOf(newPane) >= 0 {
		return fmt.Errorf("layout: pane %q already exists", newPane)
	}
	if tabIdx < 0 || tabIdx >= len(l.Tabs) || target == nil {
		return fmt.Errorf("layout: bad split target")
	}
	t := l.Tabs[tabIdx]
	parent, idx := findParent(t.Root, target)
	if parent == nil && t.Root != target {
		return fmt.Errorf("layout: node not in tab")
	}
	if parent != nil && parent.Dir == axis {
		// Same-axis context: the new pane joins the run beside the target,
		// taking half the target's share.
		fixRatios(parent)
		half := parent.Ratios[idx] / 2
		parent.Ratios[idx] = half
		at := idx + 1
		if before {
			at = idx
		}
		parent.Ratios = insertFloat(parent.Ratios, at, half)
		parent.Children = insertNode(parent.Children, at, &Node{Pane: newPane})
	} else {
		// Wrap: target keeps its subtree, the new pane takes half its
		// extent on the cross axis.
		old := *target
		children := []*Node{&old, {Pane: newPane}}
		if before {
			children = []*Node{{Pane: newPane}, &old}
		}
		*target = Node{
			Dir:      axis,
			Children: children,
			Ratios:   []float64{0.5, 0.5},
		}
	}
	t.Focused = newPane
	l.Active = tabIdx
	return nil
}

// TopEdgeNode climbs from a pane while it forms the leading (top) edge of
// stacked containers, returning the outermost node whose top edge the
// pane's bar is. A bar that is not an interior divider is exactly this
// edge: "new pane above" inserted there spans that node's full width.
func (t *Tab) TopEdgeNode(pane string) *Node {
	if t == nil || t.Root == nil {
		return nil
	}
	n := findLeafNode(t.Root, pane)
	if n == nil {
		return nil
	}
	for {
		parent, idx := findParent(t.Root, n)
		if parent == nil {
			return n
		}
		if parent.Dir == SplitDown && idx == 0 {
			n = parent // the pane's bar is also this stack's top edge
			continue
		}
		// Side-by-side siblings each own only their segment of the
		// container's top edge: "above" means above this column, not the
		// whole row.
		return n
	}
}

// findParent locates target's parent split and child index; (nil, -1) when
// target is the root or absent.
func findParent(root, target *Node) (*Node, int) {
	if root == nil || root == target {
		return nil, -1
	}
	for i, c := range root.Children {
		if c == target {
			return root, i
		}
		if p, idx := findParent(c, target); p != nil {
			return p, idx
		}
	}
	return nil, -1
}

func findLeafNode(n *Node, pane string) *Node {
	if n == nil {
		return nil
	}
	if n.Pane == pane {
		return n
	}
	for _, c := range n.Children {
		if f := findLeafNode(c, pane); f != nil {
			return f
		}
	}
	return nil
}

func insertFloat(s []float64, i int, v float64) []float64 {
	s = append(s, 0)
	copy(s[i+1:], s[i:])
	s[i] = v
	return s
}

func insertNode(s []*Node, i int, v *Node) []*Node {
	s = append(s, nil)
	copy(s[i+1:], s[i:])
	s[i] = v
	return s
}
