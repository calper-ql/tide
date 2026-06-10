package layout

// geometry: Compute lays a tab's tree into a screen rectangle. All the
// integer math lives here; the invariant throughout is exact tiling —
// every split's children plus their one-cell borders sum to the parent
// extent, so rects never overlap and never leave gaps, even in areas too
// small for the configured minimums.

// MinPaneW and MinPaneH are the smallest useful pane in cells. Compute
// overrides ratios sooner than go below them; only degenerate areas (too
// small to fit every child at minimum) break them, and even then the
// tiling stays exact.
const (
	MinPaneW = 4
	MinPaneH = 2
)

// Border is one draggable divider strip between two split siblings.
type Border struct {
	Rect     Rect  // the 1-cell-thick divider strip (vertical: W=1; horizontal: H=1)
	Vertical bool  // true: divider between side-by-side panes (drag changes widths)
	Node     *Node // the split node owning it
	Index    int   // divider sits between Children[Index] and Children[Index+1]
}

// Compute lays the tab's panes into area, returning every pane's rect and
// the draggable borders between siblings. Rects and borders tile area
// exactly: no overlaps, no gaps. In degenerate areas panes may receive
// zero-sized rects (they still appear in the map) and borders that get no
// cell are not emitted. A zero or negative area, a nil tab, or an empty
// tree returns nil maps. Compute never mutates the tree.
func (t *Tab) Compute(area Rect) (rects map[string]Rect, borders []Border) {
	if t == nil || t.Root == nil || area.W <= 0 || area.H <= 0 {
		return nil, nil
	}
	rects = make(map[string]Rect)
	computeNode(t.Root, area, rects, &borders, nil)
	return rects, borders
}

// DragBorder moves divider b by delta cells (positive = right/down),
// adjusting the two adjacent children; delta is clamped so neither goes
// below minimum. area must be the same area Compute used. The node's
// ratios are rewritten from the realized integer sizes, so the next
// Compute reproduces the drag exactly and non-adjacent children keep
// their cells. A stale border (node no longer in this tab), a bad index,
// a degenerate area, or a fully-clamped delta is a no-op.
func (t *Tab) DragBorder(b Border, delta int, area Rect) {
	if t == nil || t.Root == nil || b.Node == nil || delta == 0 || area.W <= 0 || area.H <= 0 {
		return
	}
	n := b.Node
	if b.Index < 0 || b.Index+1 >= len(n.Children) {
		return
	}
	if !containsNode(t.Root, n) {
		return
	}
	nodeRects := make(map[*Node]Rect)
	computeNode(t.Root, area, make(map[string]Rect), new([]Border), nodeRects)
	r, ok := nodeRects[n]
	if !ok {
		return
	}
	ratios := n.Ratios
	if len(ratios) != len(n.Children) {
		ratios = equalRatios(len(n.Children))
	}
	extent, minSize := r.W, MinPaneW
	if n.Dir == SplitDown {
		extent, minSize = r.H, MinPaneH
	}
	sizes, _ := splitExtents(extent, ratios, minSize)
	i := b.Index
	lo, hi := 0, 0
	if sizes[i] > minSize {
		lo = minSize - sizes[i]
	}
	if sizes[i+1] > minSize {
		hi = sizes[i+1] - minSize
	}
	delta = max(lo, min(hi, delta))
	if delta == 0 {
		return
	}
	avail := 0
	for _, s := range sizes {
		avail += s
	}
	if avail <= 0 {
		return
	}
	sizes[i] += delta
	sizes[i+1] -= delta
	out := make([]float64, len(sizes))
	for j, s := range sizes {
		out[j] = float64(s) / float64(avail)
	}
	n.Ratios = out
}

// computeNode recurses, assigning rect r to leaves and dividing splits
// along their axis. Zero-size rects are still assigned — every pane gets
// a rect — but zero-area borders are never emitted (nothing there to
// drag). nodeRects, when non-nil, collects each split node's rect so
// DragBorder can recover a divider's coordinate space.
func computeNode(n *Node, r Rect, rects map[string]Rect, borders *[]Border, nodeRects map[*Node]Rect) {
	if n == nil {
		return
	}
	if n.Pane != "" {
		rects[n.Pane] = r
		return
	}
	if len(n.Children) == 0 {
		return
	}
	if nodeRects != nil {
		nodeRects[n] = r
	}
	ratios := n.Ratios
	if len(ratios) != len(n.Children) {
		ratios = equalRatios(len(n.Children))
	}
	vertical := n.Dir == SplitRight
	extent, minSize := r.W, MinPaneW
	if !vertical {
		extent, minSize = r.H, MinPaneH
	}
	sizes, bcells := splitExtents(extent, ratios, minSize)
	pos := r.X
	if !vertical {
		pos = r.Y
	}
	for i, c := range n.Children {
		var cr Rect
		if vertical {
			cr = Rect{X: pos, Y: r.Y, W: sizes[i], H: r.H}
		} else {
			cr = Rect{X: r.X, Y: pos, W: r.W, H: sizes[i]}
		}
		computeNode(c, cr, rects, borders, nodeRects)
		pos += sizes[i]
		if i < len(n.Children)-1 && bcells[i] == 1 {
			var br Rect
			if vertical {
				br = Rect{X: pos, Y: r.Y, W: 1, H: r.H}
			} else {
				br = Rect{X: r.X, Y: pos, W: r.W, H: 1}
			}
			if br.W > 0 && br.H > 0 {
				*borders = append(*borders, Border{Rect: br, Vertical: vertical, Node: n, Index: i})
			}
			pos++
		}
	}
}

// splitExtents divides extent cells along a split's axis: the n-1 one-cell
// borders are reserved first (clamped when even those do not fit — the
// earliest dividers win), then the children share the remainder by ratio
// with minSize enforced. When the minimums cannot fit, ratios are
// overridden: children share equally, possibly at zero size. Invariant:
// sum(sizes) + sum(bcells) == max(extent, 0), every size >= 0, so the
// caller's tiling is always exact.
func splitExtents(extent int, ratios []float64, minSize int) (sizes, bcells []int) {
	n := len(ratios)
	sizes = make([]int, n)
	if n == 0 {
		return sizes, nil
	}
	bcells = make([]int, n-1)
	if extent <= 0 {
		return sizes, bcells
	}
	nb := min(n-1, extent)
	for i := range nb {
		bcells[i] = 1
	}
	avail := extent - nb
	if avail < n*minSize {
		base, rem := avail/n, avail%n
		for i := range sizes {
			sizes[i] = base
		}
		sizes[n-1] += rem
		return sizes, bcells
	}
	total := 0.0
	for _, r := range ratios {
		if r > 0 {
			total += r
		}
	}
	if total <= 0 {
		ratios = equalRatios(n)
		total = 1
	}
	sum := 0
	for i := range n - 1 {
		// the epsilon keeps exact fractions (size/avail, as DragBorder
		// writes) from flooring one cell short on float noise.
		s := int(ratios[i]/total*float64(avail) + 1e-6)
		if s < 0 {
			s = 0
		}
		sizes[i] = s
		sum += s
	}
	sizes[n-1] = avail - sum // last child absorbs the rounding remainder
	if sizes[n-1] < 0 {
		// pathological hand-built ratios; pull the overshoot back from the
		// preceding children so the tiling stays exact.
		over := -sizes[n-1]
		sizes[n-1] = 0
		for i := n - 2; i >= 0 && over > 0; i-- {
			take := min(over, sizes[i])
			sizes[i] -= take
			over -= take
		}
	}
	enforceMin(sizes, minSize)
	return sizes, bcells
}

// enforceMin raises every child below minSize to it, taking the cells from
// the largest children, preserving the total exactly. Callers guarantee
// sum(sizes) >= len(sizes)*minSize, so this always converges: each pass
// permanently settles one child at or above minimum.
func enforceMin(sizes []int, minSize int) {
	for {
		low := -1
		for i, s := range sizes {
			if s < minSize {
				low = i
				break
			}
		}
		if low < 0 {
			return
		}
		need := minSize - sizes[low]
		sizes[low] = minSize
		for need > 0 {
			big := -1
			for i, s := range sizes {
				if s > minSize && (big < 0 || s > sizes[big]) {
					big = i
				}
			}
			if big < 0 {
				sizes[low] -= need // unreachable under the caller's guarantee; keeps the total exact
				return
			}
			take := min(need, sizes[big]-minSize)
			sizes[big] -= take
			need -= take
		}
	}
}

// containsNode reports whether n is reachable from root by pointer
// identity — the staleness check for borders handed back to DragBorder.
func containsNode(root, n *Node) bool {
	if root == nil {
		return false
	}
	if root == n {
		return true
	}
	for _, c := range root.Children {
		if containsNode(c, n) {
			return true
		}
	}
	return false
}
