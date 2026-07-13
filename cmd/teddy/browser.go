package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/calper-ql/tide/internal/tui"
)

// treeNode is one entry in the file browser. Directories load their children
// lazily on first expand, so opening teddy on a huge tree is cheap.
type treeNode struct {
	name     string
	path     string
	isDir    bool
	expanded bool
	loaded   bool
	children []*treeNode
}

// flatEntry is one visible row: a node plus its indentation depth. The flat
// list is the render + hit-test order, rebuilt whenever the tree changes.
type flatEntry struct {
	node  *treeNode
	depth int
}

type browser struct {
	root *treeNode
	flat []flatEntry
	top  int // scroll offset (index of the first visible row)
	sel  int // selected row

	revealedPath string // last file auto-revealed (so reveal fires once per change)

	contentY int // absolute screen y of the first tree row (set on render)
	viewRows int
}

func newBrowser(root string) *browser {
	b := &browser{root: &treeNode{name: filepath.Base(root), path: root, isDir: true, expanded: true}}
	b.load(b.root)
	b.reflatten()
	return b
}

// load reads a directory's entries once, sorted dirs-first then
// case-insensitively, skipping .git (noise, never edited here).
func (b *browser) load(n *treeNode) {
	if n.loaded || !n.isDir {
		return
	}
	n.loaded = true
	entries, err := os.ReadDir(n.path)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.Name() == ".git" {
			continue
		}
		n.children = append(n.children, &treeNode{
			name:  e.Name(),
			path:  filepath.Join(n.path, e.Name()),
			isDir: e.IsDir(),
		})
	}
	sortNodes(n.children)
}

// sortNodes orders siblings dirs-first, then case-insensitively by name.
func sortNodes(nodes []*treeNode) {
	sort.Slice(nodes, func(i, j int) bool {
		a, c := nodes[i], nodes[j]
		if a.isDir != c.isDir {
			return a.isDir
		}
		return strings.ToLower(a.name) < strings.ToLower(c.name)
	})
}

// refresh re-reads every loaded directory from disk, adding entries that
// appeared and dropping ones that vanished, while preserving each folder's
// expansion state, the selection (by path), and scroll. This is what lets the
// Explorer track files created/deleted outside teddy with no manual poke.
func (b *browser) refresh() {
	selPath := ""
	if b.sel >= 0 && b.sel < len(b.flat) {
		selPath = b.flat[b.sel].node.path
	}
	b.refreshNode(b.root)
	b.reflatten()
	if selPath != "" { // keep the selection on the same file, if it survived
		for i, e := range b.flat {
			if e.node.path == selPath {
				b.sel = i
				break
			}
		}
	}
}

// refreshNode reconciles one loaded directory's children with disk, reusing
// existing nodes (so expanded subtrees stay expanded) and recursing into
// still-loaded subdirectories.
func (b *browser) refreshNode(n *treeNode) {
	if !n.isDir || !n.loaded {
		return
	}
	entries, err := os.ReadDir(n.path)
	if err != nil {
		return // unreadable (e.g. deleted out from under us) — leave as-is
	}
	existing := make(map[string]*treeNode, len(n.children))
	for _, c := range n.children {
		existing[c.name] = c
	}
	var children []*treeNode
	for _, e := range entries {
		if e.Name() == ".git" {
			continue
		}
		if c, ok := existing[e.Name()]; ok && c.isDir == e.IsDir() {
			children = append(children, c) // reuse, keeping expanded/loaded state
		} else {
			children = append(children, &treeNode{
				name:  e.Name(),
				path:  filepath.Join(n.path, e.Name()),
				isDir: e.IsDir(),
			})
		}
	}
	sortNodes(children)
	n.children = children
	for _, c := range children {
		b.refreshNode(c)
	}
}

// reflatten rebuilds the visible-row list from the expanded tree.
func (b *browser) reflatten() {
	b.flat = b.flat[:0]
	var walk func(nodes []*treeNode, depth int)
	walk = func(nodes []*treeNode, depth int) {
		for _, n := range nodes {
			b.flat = append(b.flat, flatEntry{n, depth})
			if n.isDir && n.expanded {
				walk(n.children, depth+1)
			}
		}
	}
	walk(b.root.children, 0)
	b.sel = clampInt(b.sel, 0, max(len(b.flat)-1, 0))
}

// activate handles a click on row idx: toggle a directory (loading it on
// first open), or open a file through the supplied callback.
func (b *browser) activate(idx int, open func(string) error) {
	if idx < 0 || idx >= len(b.flat) {
		return
	}
	b.sel = idx
	n := b.flat[idx].node
	if n.isDir {
		if n.expanded {
			n.expanded = false
		} else {
			b.load(n)
			n.expanded = true
		}
		b.reflatten()
		return
	}
	_ = open(n.path)
}

func (b *browser) scroll(delta int) {
	b.top = clampInt(b.top+delta, 0, max(len(b.flat)-1, 0))
}

// reveal expands the tree from the root down to target, selects it, and lets
// the next render scroll it into view. It only ever expands (never collapses),
// and no-ops when target is not under the root. Skips quietly if the path is
// not present on disk (e.g. an untracked new file).
func (b *browser) reveal(target string) {
	b.revealedPath = target
	rel, err := filepath.Rel(b.root.path, target)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return
	}
	node := b.root
	segs := strings.Split(rel, string(filepath.Separator))
	for i, seg := range segs {
		b.load(node)
		var child *treeNode
		for _, c := range node.children {
			if c.name == seg {
				child = c
				break
			}
		}
		if child == nil {
			return
		}
		if i < len(segs)-1 { // an ancestor directory: expand it
			b.load(child)
			child.expanded = true
		}
		node = child
	}
	b.reflatten()
	for i, e := range b.flat {
		if e.node == node {
			b.sel = i
			return
		}
	}
}

func (a *App) drawBrowser(buf *tui.Buffer, inner tui.Rect) {
	b := a.browser
	// Auto-reveal: keep the tree selection on the active file (only while the
	// Explorer is on screen, and only when the active file changed).
	if d := a.activeDoc(); d != nil && d.path != "" && d.path != b.revealedPath {
		b.reveal(d.path)
	}
	// row 0 is the panel title (drawn by drawSidePanel); row 1 the workspace
	// folder name; the tree starts at row 2.
	drawIn(buf, inner, 1, 1, stDim, b.root.name+"/")
	const listTop = 2
	rows := inner.H - listTop
	if rows < 1 {
		return
	}
	b.viewRows = rows
	b.contentY = inner.Y + listTop

	// Keep the selected row visible.
	if b.sel < b.top {
		b.top = b.sel
	}
	if b.sel >= b.top+rows {
		b.top = b.sel - rows + 1
	}
	b.top = clampInt(b.top, 0, max(len(b.flat)-rows, 0))

	for i := 0; i < rows; i++ {
		idx := b.top + i
		if idx >= len(b.flat) {
			break
		}
		e := b.flat[idx]
		y := listTop + i
		st := stText
		if e.node.isDir {
			st = stDir
		}
		if idx == b.sel {
			buf.Fill(tui.Rect{X: inner.X, Y: inner.Y + y, W: inner.W, H: 1}, ' ', stSelected)
			st = stSelected
		}
		marker := "  "
		if e.node.isDir {
			if e.node.expanded {
				marker = "▾ "
			} else {
				marker = "▸ "
			}
		}
		name := e.node.name
		if e.node.isDir {
			name += "/"
		}
		drawIn(buf, inner, 1+e.depth*2, y, st, marker+name)
	}
}

// autoRefreshBrowser is the poll-tick refresh for the Explorer: it re-reads
// the tree only while the panel is on screen, so teddy does no directory
// walking when the Explorer isn't visible.
func (a *App) autoRefreshBrowser() {
	if a.selected != 0 || a.sideCollapsed {
		return
	}
	a.browser.refresh()
}

// clickBrowser maps a click in the side panel to a tree row and activates it.
func (a *App) clickBrowser(y int) {
	idx := a.browser.top + (y - a.browser.contentY)
	a.browser.activate(idx, a.openFile)
}
