// Package picker is the interactive landing for `tide -r host` with no path.
// It runs inside `tide --serve` on the host (where it can read the host
// filesystem and reach the host daemon) and renders into the daemon's
// terminal-stream idiom, so the dumb-glass client on the laptop just blits the
// frames and ships clicks/keys back. It is mouse-first AND keyboard-friendly.
//
// Two modes:
//   - sessions: a chooser listing running sessions plus "+ New session…".
//     Pick a session to attach to it; pick New to enter the folder browser.
//   - browse:   a folder browser (click/→ to enter, ".."/← to go up, ✓ to
//     open the current folder as the project root). A "‹ back" returns to the
//     chooser when one is behind us.
//
// Directory listing (dirs first, case-insensitive, .git hidden) mirrors
// teddy's browser; rendering/navigation are tide-native because teddy's
// renderer is tty-bound and its tree is expand-in-place, not descend/ascend.
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
	listTop   = 2 // rows 0 (bar) and 1 (breadcrumb/subtitle) sit above the list
	wheelStep = 3

	styReset = "\x1b[0m"
	styBar   = "\x1b[0;7m"   // reverse: bar + buttons (theme-adaptive, no truecolor)
	styDir   = "\x1b[0;1m"   // bold: directories, the New row
	styFile  = "\x1b[0;2m"   // dim: files
	styDim   = "\x1b[0;2m"   // dim: breadcrumb/subtitle
	styHover = "\x1b[0;7;1m" // reverse bold: row under the pointer / selection

	cancelLabel = " ✕ cancel "
	backLabel   = " ‹ back  "
)

// renderMode selects the picker's screen. modeSessions is the zero value.
type renderMode int

const (
	modeSessions renderMode = iota // the session chooser
	modeBrowse                     // the folder browser
)

// Session is one running session offered in the chooser.
type Session struct {
	Root    string
	Panes   int
	Clients int
}

type entry struct {
	name  string
	isDir bool
	up    bool // the ".." row
}

// Model is the picker state. All mutation happens through Handle.
type Model struct {
	mode     renderMode
	startDir string    // where the browser opens (host $HOME)
	host     string    // shown in the chooser title
	sessions []Session // chooser rows (besides New)

	dir     string  // browse: current directory
	entries []entry // browse: entries of dir
	offset  int     // scroll offset (both modes)
	hover   int     // highlighted row; -1 = none
	cols    int
	rows    int

	chosen        string
	picked        bool
	chosenSession bool // chosen came from a running session, not a browsed folder
	cancel        bool
}

// New opens a folder browser directly (no session chooser). Used where there
// is nothing to choose between.
func New(start string, cols, rows int) *Model {
	return NewChooser(start, cols, rows, "", nil)
}

// NewChooser opens the session chooser when sessions exist, else falls
// straight through to the folder browser (a one-item chooser is just a speed
// bump).
func NewChooser(start string, cols, rows int, host string, sessions []Session) *Model {
	m := &Model{cols: cols, rows: rows, hover: -1, startDir: start, host: host, sessions: sessions}
	if len(sessions) > 0 {
		m.mode = modeSessions
		m.hover = 1 // default to the first session (reattach is the common case)
	} else {
		m.mode = modeBrowse
		m.setDir(start)
	}
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

// Size reports the current viewport (the dimensions the daemon attaches at
// once a folder/session is chosen).
func (m *Model) Size() (cols, rows int) { return m.cols, m.rows }

// Chosen reports the picked root (a session's, or a browsed folder's).
func (m *Model) Chosen() (string, bool) { return m.chosen, m.picked }

// FromSession reports whether the pick was an existing session (whose root is
// already canonical and may even no longer exist on disk) rather than a
// freshly browsed folder. The caller skips re-canonicalization for sessions.
func (m *Model) FromSession() bool { return m.chosenSession }

// Cancelled reports that the user aborted without choosing.
func (m *Model) Cancelled() bool { return m.cancel }

func (m *Model) tooSmall() bool { return m.cols < 24 || m.rows < 6 }

func (m *Model) hasBack() bool { return len(m.sessions) > 0 }

// rowCount is the number of selectable list rows in the current mode.
func (m *Model) rowCount() int {
	if m.mode == modeSessions {
		return len(m.sessions) + 1 // +1 for the New row
	}
	return len(m.entries)
}

func (m *Model) visibleRows() int {
	v := m.rows - listTop - 1 // last row is the Open button / hint
	if v < 0 {
		return 0
	}
	return v
}

func (m *Model) maxOffset() int {
	if over := m.rowCount() - m.visibleRows(); over > 0 {
		return over
	}
	return 0
}

// entryAt maps a screen cell to a list row index, or -1 if the cell is not on
// a list row. The same offset arithmetic backs both renders, so a click always
// lands on the row the user sees.
func (m *Model) entryAt(x, y int) int {
	if y < listTop || y >= listTop+m.visibleRows() {
		return -1
	}
	idx := m.offset + (y - listTop)
	if idx < 0 || idx >= m.rowCount() {
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
		return false // chrome isn't drawn; ignore input until resized
	}
	switch ev.Type {
	case input.EvKey:
		return m.handleKey(ev.Key)
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

func (m *Model) handleKey(k input.Key) bool {
	if m.mode == modeSessions {
		switch k {
		case input.KeyUp:
			return m.moveSel(-1)
		case input.KeyDown:
			return m.moveSel(1)
		case input.KeyEnter, input.KeyRight:
			if m.hover >= 0 {
				return m.activate(m.hover)
			}
		}
		return false
	}
	switch k {
	case input.KeyEnter:
		m.pick() // open the current folder as the project root
		return true
	case input.KeyUp:
		return m.moveSel(-1)
	case input.KeyDown:
		return m.moveSel(1)
	case input.KeyRight:
		return m.enterSelected()
	case input.KeyLeft:
		return m.ascend()
	}
	return false
}

func (m *Model) click(x, y int) bool {
	if y == 0 { // bar
		if m.mode == modeBrowse && m.hasBack() && x < utf8.RuneCountInString(backLabel) {
			m.setMode(modeSessions)
			return true
		}
		if x >= m.cancelStart() {
			m.cancel = true
		}
		return true
	}
	if m.mode == modeBrowse && y == m.rows-1 { // Open button (browse only)
		m.pick()
		return true
	}
	idx := m.entryAt(x, y)
	if idx < 0 {
		return false
	}
	return m.activate(idx)
}

// activate acts on list row idx for the current mode.
func (m *Model) activate(idx int) bool {
	if m.mode == modeSessions {
		if idx == 0 {
			m.enterBrowse() // the New row
			return true
		}
		if s := idx - 1; s >= 0 && s < len(m.sessions) {
			m.chosen = m.sessions[s].Root
			m.picked = true
			m.chosenSession = true
			return true
		}
		return false
	}
	if idx < 0 || idx >= len(m.entries) {
		return false
	}
	switch e := m.entries[idx]; {
	case e.up:
		m.ascend()
	case e.isDir:
		m.setDir(filepath.Join(m.dir, e.name))
	default:
		return false // files are not project roots
	}
	return true
}

func (m *Model) setMode(mode renderMode) {
	m.mode = mode
	m.offset = 0
	if mode == modeSessions {
		m.hover = 1
		if len(m.sessions) == 0 {
			m.hover = 0
		}
	} else {
		m.hover = -1
	}
}

func (m *Model) enterBrowse() {
	m.mode = modeBrowse
	m.setDir(m.startDir)
	m.selectFirst()
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
	if idx < 0 || idx >= m.rowCount() {
		idx = -1
	}
	if idx == m.hover {
		return false
	}
	m.hover = idx
	return true
}

// --- keyboard navigation -------------------------------------------------

// moveSel moves the highlight by delta (Up/Down), scrolling it into view. The
// first arrow press from no selection lands on the top row.
func (m *Model) moveSel(delta int) bool {
	if m.rowCount() == 0 {
		return false
	}
	cur := m.hover
	if cur < 0 {
		cur, delta = 0, 0
	}
	return m.selectIndex(clamp(cur+delta, 0, m.rowCount()-1))
}

// selectIndex highlights idx and scrolls so it stays visible.
func (m *Model) selectIndex(idx int) bool {
	oldHover, oldOffset := m.hover, m.offset
	m.hover = idx
	if idx < m.offset {
		m.offset = idx
	} else if v := m.visibleRows(); v > 0 && idx >= m.offset+v {
		m.offset = idx - v + 1
	}
	m.offset = clamp(m.offset, 0, m.maxOffset())
	return m.hover != oldHover || m.offset != oldOffset
}

// enterSelected descends into the highlighted directory (Right arrow); ".."
// goes up, a file does nothing. Browse mode only.
func (m *Model) enterSelected() bool {
	if m.hover < 0 || m.hover >= len(m.entries) {
		return false
	}
	switch e := m.entries[m.hover]; {
	case e.up:
		return m.ascend()
	case e.isDir:
		m.setDir(filepath.Join(m.dir, e.name))
		m.selectFirst()
		return true
	}
	return false
}

// ascend goes to the parent directory (Left arrow / ".."), highlighting the
// directory we came from. At the filesystem root, it returns to the session
// chooser if one is behind us.
func (m *Model) ascend() bool {
	parent := filepath.Dir(m.dir)
	if parent == m.dir {
		if m.hasBack() {
			m.setMode(modeSessions)
			return true
		}
		return false // already at the filesystem root, no chooser behind us
	}
	came := filepath.Base(m.dir)
	m.setDir(parent)
	if !m.selectNamed(came) {
		m.selectFirst()
	}
	return true
}

func (m *Model) selectFirst() {
	for i, e := range m.entries {
		if !e.up {
			m.selectIndex(i)
			return
		}
	}
	if len(m.entries) > 0 {
		m.selectIndex(0)
	}
}

func (m *Model) selectNamed(name string) bool {
	for i, e := range m.entries {
		if !e.up && e.name == name {
			m.selectIndex(i)
			return true
		}
	}
	return false
}

// --- rendering -----------------------------------------------------------

func (m *Model) Render() []byte {
	if m.mode == modeSessions {
		return m.renderSessions()
	}
	return m.renderBrowse()
}

func (m *Model) renderSessions() []byte {
	var b bytes.Buffer
	b.WriteString("\x1b[?25l" + styReset + "\x1b[2J")
	if m.tooSmall() {
		cup(&b, 0, 0)
		b.WriteString("window too small")
		return b.Bytes()
	}

	title := " tide — pick a session"
	if m.host != "" {
		title = " tide — sessions on " + m.host
	}
	m.bar(&b, title, false)

	cup(&b, 1, 0)
	b.WriteString(styDim + truncate(" attach to one below, or start a new session", m.cols) + styReset)

	for i := 0; i < m.visibleRows(); i++ {
		idx := m.offset + i
		if idx >= m.rowCount() {
			break
		}
		cup(&b, listTop+i, 0)
		label, st := " + New session…", styDir
		if idx > 0 {
			label, st = sessionLabel(m.sessions[idx-1]), styReset
		}
		if idx == m.hover {
			b.WriteString(styHover + padRight(label, m.cols) + styReset)
		} else {
			b.WriteString(st + truncate(label, m.cols) + styReset)
		}
	}
	return b.Bytes()
}

func (m *Model) renderBrowse() []byte {
	var b bytes.Buffer
	b.WriteString("\x1b[?25l" + styReset + "\x1b[2J")
	if m.tooSmall() {
		cup(&b, 0, 0)
		b.WriteString("window too small")
		return b.Bytes()
	}

	title := " tide — choose a folder (click to enter · ✓ to open)"
	if m.hasBack() {
		title = backLabel + "tide — choose a folder"
	}
	m.bar(&b, title, m.hasBack())

	cup(&b, 1, 0)
	b.WriteString(styDim + truncate(" "+m.dir, m.cols) + styReset)

	for i := 0; i < m.visibleRows(); i++ {
		idx := m.offset + i
		if idx >= len(m.entries) {
			break
		}
		cup(&b, listTop+i, 0)
		e := m.entries[idx]
		label := " " + e.name
		if e.up {
			label = " ../"
		} else if e.isDir {
			label += "/"
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

	cup(&b, m.rows-1, 0)
	b.WriteString(styBar + padRight(truncate(" ✓ Open this folder: "+m.dir, m.cols), m.cols) + styReset)
	return b.Bytes()
}

// bar draws row 0: a title with the cancel button pinned right. The back
// button, when present, is baked into the title string at the left.
func (m *Model) bar(b *bytes.Buffer, title string, _ bool) {
	cup(b, 0, 0)
	row := []rune(padRight(title, m.cols))
	for i, r := range []rune(cancelLabel) {
		row[m.cancelStart()+i] = r
	}
	b.WriteString(styBar + string(row) + styReset)
}

func sessionLabel(s Session) string {
	meta := fmt.Sprintf("%d pane%s", s.Panes, plural(s.Panes))
	if s.Clients > 0 {
		meta += fmt.Sprintf(" · %d client%s", s.Clients, plural(s.Clients))
	}
	return fmt.Sprintf(" %s  —  %s  ·  %s", filepath.Base(s.Root), s.Root, meta)
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
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
	return string([]rune(s)[:w])
}

func padRight(s string, w int) string {
	s = truncate(s, w)
	if pad := w - utf8.RuneCountInString(s); pad > 0 {
		return s + strings.Repeat(" ", pad)
	}
	return s
}
