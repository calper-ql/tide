// Package picker is the interactive remote folder chooser for `tide -r host`
// with no path. It runs inside `tide --serve` on the host (where it can read
// the host filesystem) and renders into the daemon's terminal-stream idiom, so
// the dumb-glass client on the laptop just blits the frames and ships clicks
// back. It is mouse-first (tide's ethos): click a folder to enter, ".." to go
// up, the wheel to scroll, and the bottom button to choose where you are.
//
// The directory-listing behaviour (dirs first, case-insensitive, .git hidden)
// mirrors teddy's file browser (cmd/teddy/browser.go); the rendering and
// navigation are tide-native because teddy's renderer is bound to a real tty
// and its tree is expand-in-place, not descend/ascend.
package picker

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/calper-ql/tide/internal/input"
)

const (
	listTop   = 2 // rows 0 (bar) and 1 (breadcrumb) sit above the list
	wheelStep = 3

	styReset = "\x1b[0m"
	styBar   = "\x1b[0;7m"   // reverse: bar + buttons (theme-adaptive, no truecolor)
	styDir   = "\x1b[0;1m"   // bold: directories
	styFile  = "\x1b[0;2m"   // dim: files
	styDim   = "\x1b[0;2m"   // dim: breadcrumb
	styHover = "\x1b[0;7;1m" // reverse bold: row under the pointer
)

const cancelLabel = " ✕ cancel "

type entry struct {
	name  string
	isDir bool
	up    bool // the ".." row
}

// Model is the picker state: the current directory, its entries, scroll
// offset, and the hovered row. All mutation happens through Handle.
type Model struct {
	dir     string
	entries []entry
	offset  int
	cols    int
	rows    int
	hover   int // hovered entry index; -1 = none
	chosen  string
	picked  bool
	cancel  bool
}

// New opens the picker rooted at dir (e.g. the host's $HOME).
func New(dir string, cols, rows int) *Model {
	m := &Model{cols: cols, rows: rows, hover: -1}
	m.setDir(dir)
	return m
}

func (m *Model) setDir(dir string) {
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	m.dir = filepath.Clean(dir)
	m.entries = loadDir(m.dir)
	m.offset = 0
	m.hover = -1
}

// loadDir lists dir: a ".." row first (unless at the filesystem root), then
// directories before files, each case-insensitive; .git is hidden. An
// unreadable directory yields just the ".." row, so a permission error never
// strands the user.
func loadDir(dir string) []entry {
	var out []entry
	if parent := filepath.Dir(dir); parent != dir {
		out = append(out, entry{name: "..", isDir: true, up: true})
	}
	des, err := os.ReadDir(dir)
	if err != nil {
		return out
	}
	var es []entry
	for _, d := range des {
		if d.Name() == ".git" {
			continue
		}
		es = append(es, entry{name: d.Name(), isDir: d.IsDir()})
	}
	sort.Slice(es, func(i, j int) bool {
		if es[i].isDir != es[j].isDir {
			return es[i].isDir
		}
		return strings.ToLower(es[i].name) < strings.ToLower(es[j].name)
	})
	return append(out, es...)
}

// Resize updates the viewport and re-clamps the scroll offset.
func (m *Model) Resize(cols, rows int) {
	m.cols, m.rows = cols, rows
	m.offset = clamp(m.offset, 0, m.maxOffset())
}

// Size reports the current viewport (the dimensions the daemon should attach
// at once a folder is chosen).
func (m *Model) Size() (cols, rows int) { return m.cols, m.rows }

// Chosen reports the picked folder once the user confirms.
func (m *Model) Chosen() (string, bool) { return m.chosen, m.picked }

// Cancelled reports that the user aborted without choosing.
func (m *Model) Cancelled() bool { return m.cancel }

// tooSmall reports a viewport too small to draw the chrome. When true, Render
// shows only a notice and Handle ignores input, so the screen and the hit map
// never disagree (a click can't land on a row that isn't drawn).
func (m *Model) tooSmall() bool { return m.cols < 24 || m.rows < 6 }

func (m *Model) visibleRows() int {
	v := m.rows - listTop - 1 // last row is the Open button
	if v < 0 {
		return 0
	}
	return v
}

func (m *Model) maxOffset() int {
	if over := len(m.entries) - m.visibleRows(); over > 0 {
		return over
	}
	return 0
}

// entryAt maps a screen cell to an entry index, or -1 if the cell is not on a
// list row. The exact same offset arithmetic backs Render, so a click always
// lands on the row the user sees (the off-by-one hazard lives here).
func (m *Model) entryAt(x, y int) int {
	if y < listTop || y >= listTop+m.visibleRows() {
		return -1
	}
	idx := m.offset + (y - listTop)
	if idx < 0 || idx >= len(m.entries) {
		return -1
	}
	return idx
}

func (m *Model) cancelStart() int { return m.cols - utf8.RuneCountInString(cancelLabel) }

// Handle applies one decoded input event and reports whether the frame needs
// repainting. Motion only repaints when the hovered row changes, so a 1003
// motion stream does not flood a remote link.
func (m *Model) Handle(ev input.Event) (dirty bool) {
	if m.tooSmall() {
		return false // chrome isn't drawn; ignore clicks/keys until resized
	}
	switch ev.Type {
	case input.EvKey:
		switch ev.Key {
		case input.KeyEnter:
			m.pick()
			return true
		case input.KeyUp:
			return m.setHover(m.hover - 1)
		case input.KeyDown:
			return m.setHover(m.hover + 1)
		}
	case input.EvMouse:
		switch ev.Mouse {
		case input.MouseWheelUp:
			return m.scroll(-wheelStep)
		case input.MouseWheelDown:
			return m.scroll(wheelStep)
		case input.MousePress:
			return m.click(ev.X, ev.Y)
		case input.MouseMotion:
			return m.setHover(m.entryAt(ev.X, ev.Y))
		}
	}
	return false
}

func (m *Model) click(x, y int) bool {
	switch {
	case y == 0:
		if x >= m.cancelStart() {
			m.cancel = true
		}
		return true
	case y == m.rows-1:
		m.pick()
		return true
	}
	idx := m.entryAt(x, y)
	if idx < 0 {
		return false
	}
	e := m.entries[idx]
	switch {
	case e.up:
		m.setDir(filepath.Dir(m.dir))
	case e.isDir:
		m.setDir(filepath.Join(m.dir, e.name))
	default:
		return false // files are not project roots
	}
	return true
}

func (m *Model) pick() {
	m.chosen = m.dir
	m.picked = true
}

func (m *Model) scroll(delta int) bool {
	n := clamp(m.offset+delta, 0, m.maxOffset())
	if n == m.offset {
		return false
	}
	m.offset = n
	return true
}

func (m *Model) setHover(idx int) bool {
	if idx < 0 || idx >= len(m.entries) {
		idx = -1
	}
	if idx == m.hover {
		return false
	}
	m.hover = idx
	return true
}

// Render composes a full frame: a bar with a cancel button, the current path,
// the windowed entry list, and the "Open this folder" button. A full repaint
// each frame keeps render and hit-testing trivially in sync; repaints only
// happen on a real state change (see Handle).
func (m *Model) Render() []byte {
	var b bytes.Buffer
	b.WriteString("\x1b[?25l" + styReset + "\x1b[2J")
	if m.tooSmall() {
		cup(&b, 0, 0)
		b.WriteString("window too small")
		return b.Bytes()
	}

	// Row 0: bar with the cancel button pinned right.
	cup(&b, 0, 0)
	bar := []rune(padRight(" tide — choose a folder (click to enter · ✓ to open)", m.cols))
	for i, r := range []rune(cancelLabel) {
		bar[m.cancelStart()+i] = r
	}
	b.WriteString(styBar + string(bar) + styReset)

	// Row 1: current path.
	cup(&b, 1, 0)
	b.WriteString(styDim + truncate(" "+m.dir, m.cols) + styReset)

	// Entry list.
	for i := 0; i < m.visibleRows(); i++ {
		idx := m.offset + i
		if idx >= len(m.entries) {
			break
		}
		cup(&b, listTop+i, 0)
		e := m.entries[idx]
		label := " " + e.name
		if e.isDir && !e.up {
			label += "/"
		} else if e.up {
			label = " ../"
		}
		switch {
		case idx == m.hover:
			b.WriteString(styHover + padRight(label, m.cols) + styReset)
		case e.isDir:
			b.WriteString(styDir + truncate(label, m.cols) + styReset)
		default:
			b.WriteString(styFile + truncate(label, m.cols) + styReset)
		}
	}

	// Last row: the Open button.
	cup(&b, m.rows-1, 0)
	b.WriteString(styBar + padRight(truncate(" ✓ Open this folder: "+m.dir, m.cols), m.cols) + styReset)
	return b.Bytes()
}

func cup(b *bytes.Buffer, y, x int) { fmt.Fprintf(b, "\x1b[%d;%dH", y+1, x+1) }

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// truncate clips s to w cells (rune count as a 1-cell proxy; wide glyphs in
// names are a known v1 approximation, as elsewhere in tide).
func truncate(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= w {
		return s
	}
	r := []rune(s)
	return string(r[:w])
}

func padRight(s string, w int) string {
	s = truncate(s, w)
	if pad := w - utf8.RuneCountInString(s); pad > 0 {
		return s + strings.Repeat(" ", pad)
	}
	return s
}
