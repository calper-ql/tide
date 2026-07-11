// Ported from vt10x (github.com/hinshun/vt10x), MIT licensed — see LICENSE-vt10x.
// Local changes are marked with "tide:" comments.

package vt

import (
	"io"
	"log"
	"reflect"
	"sync"
	"unicode/utf8"
)

const (
	tabspaces = 8
)

const (
	attrReverse = 1 << iota
	attrUnderline
	attrBold
	attrGfx
	attrItalic
	attrBlink
	attrWrap
	attrWide      // tide: lead cell of a double-width glyph
	attrWideDummy // tide: right half of a double-width glyph (Char 0)
	attrFaint     // tide: SGR 2 (dim); preserved and re-emitted, the client renders it
	attrStrike    // tide: SGR 9 (crossed-out)
	attrConceal   // tide: SGR 8 (invisible)
	attrOverline  // tide: SGR 53 (overlined)
)

const (
	cursorDefault = 1 << iota
	cursorWrapNext
	cursorOrigin
)

// ModeFlag represents various terminal mode states.
type ModeFlag uint32

// Terminal modes
const (
	ModeWrap ModeFlag = 1 << iota
	ModeInsert
	ModeAppKeypad
	ModeAltScreen
	ModeCRLF
	ModeMouseButton
	ModeMouseMotion
	ModeReverse
	ModeKeyboardLock
	ModeHide
	ModeEcho
	ModeAppCursor
	ModeMouseSgr
	Mode8bit
	ModeBlink
	ModeFBlink
	ModeFocus
	ModeMouseX10
	ModeMouseMany
	ModeBracketedPaste // tide: DECSET 2004, needed for exact reattach
	ModeMouseMask      = ModeMouseButton | ModeMouseMotion | ModeMouseX10 | ModeMouseMany
)

// ChangeFlag represents possible state changes of the terminal.
type ChangeFlag uint32

// Terminal changes to occur in VT.ReadState
const (
	ChangedScreen ChangeFlag = 1 << iota
	ChangedTitle
)

type Glyph struct {
	Char   rune
	Mode   int16
	FG, BG Color
}

type line []Glyph

type Cursor struct {
	Attr  Glyph
	X, Y  int
	State uint8
}

type parseState func(c rune)

// State represents the terminal emulation state. Use Lock/Unlock
// methods to synchronize data access with VT.
type State struct {
	DebugLogger *log.Logger

	w             io.Writer
	mu            sync.Mutex
	changed       ChangeFlag
	cols, rows    int
	lines         []line
	altLines      []line
	dirty         []bool // line dirtiness
	anydirty      bool
	cur, curSaved Cursor
	top, bottom   int // scroll limits
	mode          ModeFlag
	state         parseState
	str           strEscape
	csi           csiEscape
	numlock       bool
	tabs          []bool
	title         string
	colorOverride map[Color]Color

	// tide: last printed graphic rune, for REP (CSI b).
	lastChar rune
	// tide: a 1049 alt-screen entry has saved the cursor at least once, so
	// a (possibly unpaired) 1049 exit may restore it (see setMode).
	altSaved bool
	// tide: DECSCUSR cursor style (CSI Ps SP q). 0 or 1 = blinking block
	// (default); 2 steady block, 3/4 underline, 5/6 bar.
	cursorShape int

	// tide: keyboard enhancement protocols the inner app has requested, so
	// the router can re-encode the modified keys a legacy terminal would have
	// to drop (shift+enter chief among them). kittyFlags is the active Kitty
	// keyboard protocol flag set (0 = disabled) and kittyStack the entries
	// saved by the CSI > / CSI < push/pop pair; modifyOtherKeys is the xterm
	// XTMODKEYS level (0/1/2) from CSI > 4 ; Pv m. See input.EncodeKey.
	kittyFlags      int
	kittyStack      []int
	modifyOtherKeys int

	// tide: fixed-capacity ring of lines scrolled off the top of the main
	// screen; see scrollback.go. histScrolled counts lines scrolled into
	// history since the last full clear — the resize pull budget (tmux's
	// hscrolled): growth re-exposes only content that actually scrolled
	// away, never scrollback from before an ED 2.
	history      []line
	historyStart int
	historyCount int
	histScrolled int

	// tide: bytes of the in-flight (incomplete) escape sequence, so a
	// snapshot taken mid-sequence can hand them to the continuation stream.
	// groundPC identifies the ground parser state; see put.
	seq         []byte
	seqOverflow bool
	groundPC    uintptr

	// tide: clipboard events emitted by inner programs via OSC 52, drained
	// by Term.DrainClips after each Write so the pane can forward them to
	// clients without holding the State lock.
	pendingClips []ClipEvent
}

// ClipEvent holds one clipboard write request from an OSC 52 sequence.
type ClipEvent struct {
	Target string // "c" (clipboard) or "p" (primary)
	Text   string
}

// tide: bound on in-flight sequence capture; a pathological never-ending
// DCS/OSC must not grow memory, and a sequence this large cannot usefully
// be replayed anyway.
const maxInflight = 1 << 16

func newState(w io.Writer) *State {
	return &State{
		w:             w,
		colorOverride: make(map[Color]Color),
	}
}

func (t *State) logf(format string, args ...interface{}) {
	if t.DebugLogger != nil {
		t.DebugLogger.Printf(format, args...)
	}
}

func (t *State) logln(s string) {
	if t.DebugLogger != nil {
		t.DebugLogger.Println(s)
	}
}

func (t *State) lock() {
	t.mu.Lock()
}

func (t *State) unlock() {
	t.mu.Unlock()
}

// Lock locks the state object's mutex.
func (t *State) Lock() {
	t.mu.Lock()
}

// Unlock resets change flags and unlocks the state object's mutex.
func (t *State) Unlock() {
	t.resetChanges()
	t.mu.Unlock()
}

// Cell returns the glyph containing the character code, foreground color, and
// background color at position (x, y) relative to the top left of the terminal.
func (t *State) Cell(x, y int) Glyph {
	cell := t.lines[y][x]
	fg, ok := t.colorOverride[cell.FG]
	if ok {
		cell.FG = fg
	}
	bg, ok := t.colorOverride[cell.BG]
	if ok {
		cell.BG = bg
	}
	return cell
}

// Cursor returns the current position of the cursor.
func (t *State) Cursor() Cursor {
	return t.cur
}

// CursorVisible returns the visible state of the cursor.
func (t *State) CursorVisible() bool {
	return t.mode&ModeHide == 0
}

// Mode returns the current terminal mode.
func (t *State) Mode() ModeFlag {
	return t.mode
}

// Title returns the current title set via the tty.
func (t *State) Title() string {
	return t.title
}

/*
// ChangeMask returns a bitfield of changes that have occured by VT.
func (t *State) ChangeMask() ChangeFlag {
	return t.changed
}
*/

// Changed returns true if change has occured.
func (t *State) Changed(change ChangeFlag) bool {
	return t.changed&change != 0
}

// resetChanges resets the change mask and dirtiness.
func (t *State) resetChanges() {
	for i := range t.dirty {
		t.dirty[i] = false
	}
	t.anydirty = false
	t.changed = 0
}

func (t *State) saveCursor() {
	t.curSaved = t.cur
}

func (t *State) restoreCursor() {
	t.cur = t.curSaved
	t.moveTo(t.cur.X, t.cur.Y)
}

// tide: put records the bytes of any in-flight escape sequence (cleared the
// moment the parser returns to ground) for Snapshot to replay.
func (t *State) put(c rune) {
	if !t.seqOverflow {
		var enc [utf8.UTFMax]byte
		n := utf8.EncodeRune(enc[:], c)
		t.seq = append(t.seq, enc[:n]...)
		if len(t.seq) > maxInflight {
			t.seq, t.seqOverflow = t.seq[:0], true
		}
	}
	t.state(c)
	if reflect.ValueOf(t.state).Pointer() == t.groundPC {
		t.seq, t.seqOverflow = t.seq[:0], false
	}
}

func (t *State) putTab(forward bool) {
	x := t.cur.X
	if forward {
		if x == t.cols {
			return
		}
		for x++; x < t.cols && !t.tabs[x]; x++ {
		}
	} else {
		if x == 0 {
			return
		}
		for x--; x > 0 && !t.tabs[x]; x-- {
		}
	}
	t.moveTo(x, t.cur.Y)
}

func (t *State) newline(firstCol bool) {
	y := t.cur.Y
	if y == t.bottom {
		// tide: scroll with the live pen so the newly exposed bottom line is
		// filled with the current background (BCE), matching IND/SU and st's
		// tnewline. Upstream reset the pen to default here, breaking BCE on
		// LF/auto-wrap scrolls.
		t.scrollUp(t.top, 1)
	} else {
		y++
	}
	if firstCol {
		t.moveTo(0, y)
	} else {
		t.moveTo(t.cur.X, y)
	}
}

// table from st, which in turn is from rxvt :)
var gfxCharTable = [62]rune{
	'↑', '↓', '→', '←', '█', '▚', '☃', // A - G
	0, 0, 0, 0, 0, 0, 0, 0, // H - O
	0, 0, 0, 0, 0, 0, 0, 0, // P - W
	0, 0, 0, 0, 0, 0, 0, ' ', // X - _
	'◆', '▒', '␉', '␌', '␍', '␊', '°', '±', // ` - g
	'␤', '␋', '┘', '┐', '┌', '└', '┼', '⎺', // h - o
	'⎻', '─', '⎼', '⎽', '├', '┤', '┴', '┬', // p - w
	'│', '≤', '≥', 'π', '≠', '£', '·', // x - ~
}

func (t *State) setChar(c rune, attr *Glyph, x, y int) {
	if attr.Mode&attrGfx != 0 {
		if c >= 0x41 && c <= 0x7e && gfxCharTable[c-0x41] != 0 {
			c = gfxCharTable[c-0x41]
		}
	}
	// tide: overwriting half of a double-width pair blanks the partner, so
	// the grid can never hold a torn pair (which no byte stream could
	// faithfully re-render).
	if t.lines[y][x].Mode&attrWideDummy != 0 && x > 0 && t.lines[y][x-1].Mode&attrWide != 0 {
		t.lines[y][x-1].Char = ' '
		t.lines[y][x-1].Mode &^= attrWide
	}
	if t.lines[y][x].Mode&attrWide != 0 && x+1 < t.cols && t.lines[y][x+1].Mode&attrWideDummy != 0 {
		t.lines[y][x+1].Char = ' '
		t.lines[y][x+1].Mode &^= attrWideDummy
	}
	t.changed |= ChangedScreen
	t.dirty[y] = true
	t.lines[y][x] = *attr
	t.lines[y][x].Char = c
	//if t.options.BrightBold && attr.Mode&attrBold != 0 && attr.FG < 8 {
	if attr.Mode&attrBold != 0 && attr.FG < 8 {
		t.lines[y][x].FG = attr.FG + 8
	}
	if attr.Mode&attrReverse != 0 {
		t.lines[y][x].FG = attr.BG
		t.lines[y][x].BG = attr.FG
	}
}

func (t *State) defaultCursor() Cursor {
	c := Cursor{}
	c.Attr.FG = DefaultFG
	c.Attr.BG = DefaultBG
	return c
}

// softReset implements DECSTR (CSI ! p): reset the pen, origin mode, IRM,
// cursor visibility, the scroll region, and the saved cursor — WITHOUT
// clearing the screen or moving the cursor (apps use it to get a clean slate
// mid-session, e.g. before drawing a fresh UI).
func (t *State) softReset() {
	def := t.defaultCursor()
	t.cur.Attr = def.Attr
	t.cur.State &^= cursorOrigin
	t.curSaved = def
	t.modMode(false, ModeInsert)
	t.modMode(false, ModeHide)
	t.setScroll(0, t.rows-1)
}

func (t *State) reset() {
	// tide: RIS returns to the main buffer; without the swap the alt bit is
	// cleared but t.lines still points at the alt buffer, orphaning content.
	if t.mode&ModeAltScreen != 0 {
		t.swapScreen()
	}
	t.cur = t.defaultCursor()
	t.saveCursor()
	for i := range t.tabs {
		t.tabs[i] = false
	}
	for i := tabspaces; i < len(t.tabs); i += tabspaces {
		t.tabs[i] = true
	}
	t.top = 0
	t.bottom = t.rows - 1
	t.mode = ModeWrap
	t.colorOverride = make(map[Color]Color) // RIS resets palette overrides
	t.cursorShape = 0
	// tide: a hard reset drops any keyboard-protocol enhancement the app had
	// negotiated, back to the legacy encoding.
	t.kittyFlags = 0
	t.kittyStack = nil
	t.modifyOtherKeys = 0
	t.histScrolled = 0
	t.altSaved = false
	// tide: clear the WHOLE screen — upstream transposed the args
	// (rows-1,cols-1), wiping only a square corner.
	t.clearAll()
	t.moveTo(0, 0)
}

// resize reshapes both grids to cols x rows, matching tmux (verified
// head-to-head): each grid slides up just enough to keep ITS OWN cursor
// visible — the live cursor for the active grid, the 1049-saved cursor for
// a main grid parked behind an alt screen. The main grid's slid-away top
// rows leave through the history ring like scrolled lines; rows cut off
// below the cursor are dropped (pushing them would put below-cursor content
// ABOVE the screen in scrollback order). Growth pulls main-grid rows back
// out of history — but only past the blank slack under the content, so a
// shrink+grow cycle round-trips while a just-cleared screen stays
// top-anchored. The alt grid has no history: it neither pushes nor pulls.
// TODO: definitely can improve allocs (rows-only changes realloc both grids).
func (t *State) resize(cols, rows int) bool {
	if cols == t.cols && rows == t.rows {
		return false
	}
	if cols < 1 || rows < 1 {
		return false
	}
	alt := t.mode&ModeAltScreen != 0
	oldRows, oldCols := t.rows, t.cols
	mincols := min(cols, oldCols)
	oldMain, oldAlt := t.lines, t.altLines
	mainY, altY := t.cur.Y, t.cur.Y
	if alt {
		oldMain, oldAlt = t.altLines, t.lines
		mainY = t.curSaved.Y
	}

	mainSlide := max(mainY-rows+1, 0)
	altSlide := max(altY-rows+1, 0)
	for i := 0; i < mainSlide; i++ {
		t.pushHistory(oldMain[i])
	}
	// Growth pulls back at most the lines that have SCROLLED into history
	// since the last full clear (tmux's hscrolled): a screen that filled by
	// scrolling stays glued to its bottom, a just-cleared one stays at the
	// top instead of having old scrollback shoved back over the prompt.
	pull := 0
	if rows > oldRows {
		pull = clamp(rows-oldRows, 0, min(t.histScrolled, t.historyCount))
	}

	t.cols = cols
	t.rows = rows
	t.changed |= ChangedScreen
	t.dirty = make([]bool, rows)
	for y := range t.dirty {
		t.dirty[y] = true
	}
	newMain := t.reshapeGrid(oldMain, oldRows, mincols, mainSlide, pull)
	for k := 0; k < pull; k++ {
		h := t.historyLine(t.historyCount - pull + k)
		n := copy(newMain[k], h)
		t.eraseCells(newMain[k], n, cols)
	}
	t.popHistory(pull)
	t.histScrolled -= pull
	newAlt := t.reshapeGrid(oldAlt, oldRows, mincols, altSlide, 0)
	if alt {
		t.lines, t.altLines = newAlt, newMain
	} else {
		t.lines, t.altLines = newMain, newAlt
	}

	oldTabs := t.tabs
	t.tabs = make([]bool, cols)
	copy(t.tabs, oldTabs)
	if cols > len(oldTabs) {
		i := len(oldTabs) - 1
		for i > 0 && !oldTabs[i] {
			i--
		}
		for i += tabspaces; i < cols; i += tabspaces {
			t.tabs[i] = true
		}
	}

	// tide: only column truncation and pulled rows (checkpointed at another
	// width) can cut a double-width pair at the new edge.
	if cols < oldCols || pull > 0 {
		for i := 0; i < rows; i++ {
			t.normalizeWideRow(t.lines[i])
			t.normalizeWideRow(t.altLines[i])
		}
	}
	t.setScroll(0, rows-1)
	// Each cursor rides its own grid's transform, so DECRC / 1049l restore
	// land on the same text.
	t.curSaved.Y = clamp(t.curSaved.Y-mainSlide+pull, 0, rows-1)
	if alt {
		t.moveTo(t.cur.X, t.cur.Y-altSlide)
	} else {
		t.moveTo(t.cur.X, t.cur.Y-mainSlide+pull)
	}
	return mainSlide > 0 || altSlide > 0
}

// reshapeGrid builds the rows x t.cols version of a grid: old rows starting
// at slide land shifted down by shift, and only the exposed cells — pulled
// rows (filled by the caller), widened column tails, the bottom remainder —
// are erased. t.rows/t.cols already hold the new size.
func (t *State) reshapeGrid(old []line, oldRows, mincols, slide, shift int) []line {
	g := make([]line, t.rows)
	for y := range g {
		g[y] = make(line, t.cols)
	}
	kept := min(oldRows-slide, t.rows-shift)
	for i := 0; i < kept; i++ {
		row := g[shift+i]
		copy(row[:mincols], old[slide+i][:mincols])
		t.eraseCells(row, mincols, t.cols)
	}
	for y := 0; y < shift; y++ {
		t.eraseCells(g[y], 0, t.cols)
	}
	for y := shift + kept; y < t.rows; y++ {
		t.eraseCells(g[y], 0, t.cols)
	}
	return g
}

// eraseCells blanks l[x0:x1] — the one erase rule for clears and
// resize-exposed cells alike. Erase is background-color-erase: the cell
// keeps the pen's fg/bg but drops every other rendition (underline, bold,
// ...). Carrying underline here is what painted a continuous underscore
// across blank space under apps that set underline then erase to end of
// line (lazygit). Matches st (gp->mode = 0).
func (t *State) eraseCells(l line, x0, x1 int) {
	for x := x0; x < x1; x++ {
		l[x] = t.cur.Attr
		l[x].Char = ' '
		l[x].Mode = 0
	}
}

func (t *State) clear(x0, y0, x1, y1 int) {
	if x0 > x1 {
		x0, x1 = x1, x0
	}
	if y0 > y1 {
		y0, y1 = y1, y0
	}
	x0 = clamp(x0, 0, t.cols-1)
	x1 = clamp(x1, 0, t.cols-1)
	y0 = clamp(y0, 0, t.rows-1)
	y1 = clamp(y1, 0, t.rows-1)
	t.changed |= ChangedScreen
	for y := y0; y <= y1; y++ {
		t.dirty[y] = true
		t.eraseCells(t.lines[y], x0, x1+1)
		// tide: a clear that cuts a double-width pair leaves no torn halves.
		if x0 > 0 && t.lines[y][x0-1].Mode&attrWide != 0 {
			t.lines[y][x0-1].Char = ' '
			t.lines[y][x0-1].Mode &^= attrWide
		}
		if x1+1 < t.cols && t.lines[y][x1+1].Mode&attrWideDummy != 0 {
			t.lines[y][x1+1].Char = ' '
			t.lines[y][x1+1].Mode &^= attrWideDummy
		}
	}
}

// tide: normalizeWideRow repairs double-width pairs torn by row shifts
// (ICH/DCH) or resize truncation.
func (t *State) normalizeWideRow(row line) {
	n := len(row)
	for x := 0; x < n; x++ {
		if row[x].Mode&attrWide != 0 && (x+1 >= n || row[x+1].Mode&attrWideDummy == 0) {
			row[x].Char = ' '
			row[x].Mode &^= attrWide
		}
		if row[x].Mode&attrWideDummy != 0 && (x == 0 || row[x-1].Mode&attrWide == 0) {
			row[x].Char = ' '
			row[x].Mode &^= attrWideDummy
		}
	}
}

func (t *State) clearAll() {
	t.clear(0, 0, t.cols-1, t.rows-1)
}

func (t *State) moveAbsTo(x, y int) {
	if t.cur.State&cursorOrigin != 0 {
		y += t.top
	}
	t.moveTo(x, y)
}

func (t *State) moveTo(x, y int) {
	var miny, maxy int
	if t.cur.State&cursorOrigin != 0 {
		miny = t.top
		maxy = t.bottom
	} else {
		miny = 0
		maxy = t.rows - 1
	}
	x = clamp(x, 0, t.cols-1)
	y = clamp(y, miny, maxy)
	t.changed |= ChangedScreen
	t.cur.State &^= cursorWrapNext
	t.cur.X = x
	t.cur.Y = y
}

func (t *State) swapScreen() {
	t.lines, t.altLines = t.altLines, t.lines
	t.mode ^= ModeAltScreen
	t.dirtyAll()
}

func (t *State) dirtyAll() {
	t.changed |= ChangedScreen
	for y := 0; y < t.rows; y++ {
		t.dirty[y] = true
	}
}

func (t *State) setScroll(top, bottom int) {
	top = clamp(top, 0, t.rows-1)
	bottom = clamp(bottom, 0, t.rows-1)
	if top > bottom {
		top, bottom = bottom, top
	}
	t.top = top
	t.bottom = bottom
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func clamp(val, min, max int) int {
	if val < min {
		return min
	} else if val > max {
		return max
	}
	return val
}

func between(val, min, max int) bool {
	if val < min || val > max {
		return false
	}
	return true
}

func (t *State) scrollDown(orig, n int) {
	n = clamp(n, 0, t.bottom-orig+1)
	t.clear(0, t.bottom-n+1, t.cols-1, t.bottom)
	t.changed |= ChangedScreen
	for i := t.bottom; i >= orig+n; i-- {
		t.lines[i], t.lines[i-n] = t.lines[i-n], t.lines[i]
		t.dirty[i] = true
		t.dirty[i-n] = true
	}

	// TODO: selection scroll
}

func (t *State) scrollUp(orig, n int) {
	n = clamp(n, 0, t.bottom-orig+1)
	// tide: full-screen scrolls on the main screen feed the history ring;
	// partial scroll regions (status-line setups) are not scrollback.
	if t.mode&ModeAltScreen == 0 && orig == 0 && t.top == 0 && t.bottom == t.rows-1 {
		for i := orig; i < orig+n; i++ {
			t.pushHistory(t.lines[i])
		}
	}
	t.clear(0, orig, t.cols-1, orig+n-1)
	t.changed |= ChangedScreen
	for i := orig; i <= t.bottom-n; i++ {
		t.lines[i], t.lines[i+n] = t.lines[i+n], t.lines[i]
		t.dirty[i] = true
		t.dirty[i+n] = true
	}

	// TODO: selection scroll
}

func (t *State) modMode(set bool, bit ModeFlag) {
	if set {
		t.mode |= bit
	} else {
		t.mode &^= bit
	}
}

func (t *State) setMode(priv bool, set bool, args []int) {
	if priv {
		for _, a := range args {
			switch a {
			case 1: // DECCKM - cursor key
				t.modMode(set, ModeAppCursor)
			case 5: // DECSCNM - reverse video
				mode := t.mode
				t.modMode(set, ModeReverse)
				if mode != t.mode {
					// TODO: redraw
				}
			case 6: // DECOM - origin
				if set {
					t.cur.State |= cursorOrigin
				} else {
					t.cur.State &^= cursorOrigin
				}
				t.moveAbsTo(0, 0)
			case 7: // DECAWM - auto wrap
				t.modMode(set, ModeWrap)
			// IGNORED:
			case 0, // error
				2,  // DECANM - ANSI/VT52
				3,  // DECCOLM - column
				4,  // DECSCLM - scroll
				8,  // DECARM - auto repeat
				18, // DECPFF - printer feed
				19, // DECPEX - printer extent
				42, // DECNRCM - national characters
				12: // att610 - start blinking cursor
				break
			case 25: // DECTCEM - text cursor enable mode
				t.modMode(!set, ModeHide)
			case 9: // X10 mouse compatibility mode
				t.modMode(false, ModeMouseMask)
				t.modMode(set, ModeMouseX10)
			case 1000: // report button press
				t.modMode(false, ModeMouseMask)
				t.modMode(set, ModeMouseButton)
			case 1002: // report motion on button press
				t.modMode(false, ModeMouseMask)
				t.modMode(set, ModeMouseMotion)
			case 1003: // enable all mouse motions
				t.modMode(false, ModeMouseMask)
				t.modMode(set, ModeMouseMany)
			case 1004: // send focus events to tty
				t.modMode(set, ModeFocus)
			case 1006: // extended reporting mode
				t.modMode(set, ModeMouseSgr)
			case 2004: // tide: bracketed paste reporting
				t.modMode(set, ModeBracketedPaste)
			case 1034:
				t.modMode(set, Mode8bit)
			case 1049, // = 1047 and 1048
				47, 1047:
				alt := t.mode&ModeAltScreen != 0
				if alt {
					t.clear(0, 0, t.cols-1, t.rows-1)
				}
				// tide: upstream swapped on `!set || !alt`, which flips a
				// main-screen terminal INTO the alt screen on a reset
				// (1049l). Only a real transition swaps.
				if set != alt {
					t.swapScreen()
				}
				if a != 1049 {
					break
				}
				// tide: 1049 saves the cursor on a real entry and restores
				// on ANY exit once a save exists — even an unpaired one
				// (tmux restores "even if the alternate screen is not in
				// use"); an exit before any save must not yank the cursor
				// to a never-saved home. Matches tmux, fuzz-verified.
				if set && !alt {
					t.saveCursor()
					t.altSaved = true
				} else if !set && t.altSaved {
					t.restoreCursor()
				}
			case 1048:
				if set {
					t.saveCursor()
				} else {
					t.restoreCursor()
				}
			case 1001:
				// mouse highlight mode; can hang the terminal by design when
				// implemented
			case 1005:
				// utf8 mouse mode; will confuse applications not supporting
				// utf8 and luit
			case 1015:
				// urxvt mangled mouse mode; incompatiblt and can be mistaken
				// for other control codes
			default:
				t.logf("unknown private set/reset mode %d\n", a)
			}
		}
	} else {
		for _, a := range args {
			switch a {
			case 0: // Error (ignored)
			case 2: // KAM - keyboard action
				t.modMode(set, ModeKeyboardLock)
			case 4: // IRM - insertion-replacement
				t.modMode(set, ModeInsert)
				t.logln("insert mode not implemented")
			case 12: // SRM - send/receive
				t.modMode(set, ModeEcho)
			case 20: // LNM - linefeed/newline
				t.modMode(set, ModeCRLF)
			case 34:
				t.logln("right-to-left mode not implemented")
			case 96:
				t.logln("right-to-left copy mode not implemented")
			default:
				t.logf("unknown set/reset mode %d\n", a)
			}
		}
	}
}

// setAttr applies an SGR parameter list. sub[i] marks args[i] as a colon
// sub-parameter (see csiEscape), letting ESC[4:3m and ESC[38:2:r:g:bm be
// told apart from their ';' forms.
func (t *State) setAttr(attr []int, sub []bool) {
	if len(attr) == 0 {
		attr = []int{0}
	}
	isSub := func(k int) bool { return k >= 0 && k < len(sub) && sub[k] }
	for i := 0; i < len(attr); i++ {
		a := attr[i]
		switch a {
		case 0:
			t.cur.Attr.Mode &^= attrReverse | attrUnderline | attrBold | attrFaint |
				attrItalic | attrBlink | attrStrike | attrConceal | attrOverline
			t.cur.Attr.FG = DefaultFG
			t.cur.Attr.BG = DefaultBG
		case 1:
			t.cur.Attr.Mode |= attrBold
		case 2: // tide: faint/dim
			t.cur.Attr.Mode |= attrFaint
		case 3:
			t.cur.Attr.Mode |= attrItalic
		case 4:
			// tide: ESC[4:Nm is a single styled underline (N=0 off, else on);
			// ESC[4;Nm would be two separate SGRs. Without sub-params, plain on.
			if isSub(i + 1) {
				if attr[i+1] == 0 {
					t.cur.Attr.Mode &^= attrUnderline
				} else {
					t.cur.Attr.Mode |= attrUnderline
				}
				i++
			} else {
				t.cur.Attr.Mode |= attrUnderline
			}
		case 5, 6: // slow, rapid blink
			t.cur.Attr.Mode |= attrBlink
		case 7:
			t.cur.Attr.Mode |= attrReverse
		case 8: // tide: conceal/invisible
			t.cur.Attr.Mode |= attrConceal
		case 9: // tide: crossed-out
			t.cur.Attr.Mode |= attrStrike
		case 21, 22: // tide: 22 is normal intensity — clears both bold and faint
			t.cur.Attr.Mode &^= attrBold | attrFaint
		case 23:
			t.cur.Attr.Mode &^= attrItalic
		case 24:
			t.cur.Attr.Mode &^= attrUnderline
		case 25, 26:
			t.cur.Attr.Mode &^= attrBlink
		case 27:
			t.cur.Attr.Mode &^= attrReverse
		case 28: // tide: reveal (conceal off)
			t.cur.Attr.Mode &^= attrConceal
		case 29: // tide: not crossed-out
			t.cur.Attr.Mode &^= attrStrike
		case 38:
			col, ok, ni := sgrColor(attr, sub, i)
			if ok {
				t.cur.Attr.FG = col
			}
			i = ni
		case 39:
			t.cur.Attr.FG = DefaultFG
		case 48:
			col, ok, ni := sgrColor(attr, sub, i)
			if ok {
				t.cur.Attr.BG = col
			}
			i = ni
		case 49:
			t.cur.Attr.BG = DefaultBG
		case 53: // tide: overline
			t.cur.Attr.Mode |= attrOverline
		case 55: // tide: not overlined
			t.cur.Attr.Mode &^= attrOverline
		case 58: // tide: underline color — consumed so it can't poison the
			// following args; the value is not stored (underline still shows).
			_, _, ni := sgrColor(attr, sub, i)
			i = ni
		case 59: // default underline color — no-op
		default:
			if between(a, 30, 37) {
				t.cur.Attr.FG = Color(a - 30)
			} else if between(a, 40, 47) {
				t.cur.Attr.BG = Color(a - 40)
			} else if between(a, 90, 97) {
				t.cur.Attr.FG = Color(a - 90 + 8)
			} else if between(a, 100, 107) {
				t.cur.Attr.BG = Color(a - 100 + 8)
			} else {
				t.logf("gfx attr %d unknown\n", a)
			}
		}
	}
}

// sgrColor parses an extended color whose selector (38/48/58) is at attr[i].
// It handles both the legacy ';' form (38;5;n, 38;2;r;g;b) and the ITU ':'
// sub-parameter form (38:5:n, 38:2:r:g:b, 38:2:CS:r:g:b with an optional
// colorspace id), matching xterm and st. It returns the color, whether it
// parsed, and the index of the last argument it consumed.
func sgrColor(attr []int, sub []bool, i int) (Color, bool, int) {
	subAt := func(k int) bool { return k >= 0 && k < len(sub) && sub[k] }
	if i+1 >= len(attr) {
		return 0, false, i
	}
	if subAt(i + 1) {
		j := i + 1
		for j+1 < len(attr) && subAt(j+1) {
			j++
		}
		parts := attr[i+1 : j+1] // [5,n] or [2,r,g,b] or [2,CS,r,g,b]
		switch {
		case len(parts) >= 2 && parts[0] == 5:
			if between(parts[1], 0, 255) {
				return Color(parts[1]), true, j
			}
		case len(parts) >= 4 && parts[0] == 2:
			r, g, b := parts[len(parts)-3], parts[len(parts)-2], parts[len(parts)-1]
			if between(r, 0, 255) && between(g, 0, 255) && between(b, 0, 255) {
				return Color(r<<16 | g<<8 | b), true, j
			}
		}
		return 0, false, j
	}
	switch {
	case i+2 < len(attr) && attr[i+1] == 5:
		if between(attr[i+2], 0, 255) {
			return Color(attr[i+2]), true, i + 2
		}
		return 0, false, i + 2
	case i+4 < len(attr) && attr[i+1] == 2:
		r, g, b := attr[i+2], attr[i+3], attr[i+4]
		if between(r, 0, 255) && between(g, 0, 255) && between(b, 0, 255) {
			return Color(r<<16 | g<<8 | b), true, i + 4
		}
		return 0, false, i + 4
	}
	return 0, false, i
}

func (t *State) insertBlanks(n int) {
	src := t.cur.X
	dst := src + n
	size := t.cols - dst
	t.changed |= ChangedScreen
	t.dirty[t.cur.Y] = true

	if dst >= t.cols {
		t.clear(t.cur.X, t.cur.Y, t.cols-1, t.cur.Y)
	} else {
		copy(t.lines[t.cur.Y][dst:dst+size], t.lines[t.cur.Y][src:src+size])
		t.clear(src, t.cur.Y, dst-1, t.cur.Y)
	}
	t.normalizeWideRow(t.lines[t.cur.Y]) // tide
}

func (t *State) insertBlankLines(n int) {
	if t.cur.Y < t.top || t.cur.Y > t.bottom {
		return
	}
	t.scrollDown(t.cur.Y, n)
}

func (t *State) deleteLines(n int) {
	if t.cur.Y < t.top || t.cur.Y > t.bottom {
		return
	}
	t.scrollUp(t.cur.Y, n)
}

func (t *State) deleteChars(n int) {
	src := t.cur.X + n
	dst := t.cur.X
	size := t.cols - src
	t.changed |= ChangedScreen
	t.dirty[t.cur.Y] = true

	if src >= t.cols {
		t.clear(t.cur.X, t.cur.Y, t.cols-1, t.cur.Y)
	} else {
		copy(t.lines[t.cur.Y][dst:dst+size], t.lines[t.cur.Y][src:src+size])
		t.clear(t.cols-n, t.cur.Y, t.cols-1, t.cur.Y)
	}
	t.normalizeWideRow(t.lines[t.cur.Y]) // tide
}

func (t *State) setTitle(title string) {
	t.changed |= ChangedTitle
	t.title = title
}

func (t *State) Size() (cols, rows int) {
	return t.cols, t.rows
}

func (t *State) String() string {
	t.Lock()
	defer t.Unlock()

	var view []rune
	for y := 0; y < t.rows; y++ {
		for x := 0; x < t.cols; x++ {
			attr := t.Cell(x, y)
			view = append(view, attr.Char)
		}
		view = append(view, '\n')
	}

	return string(view)
}
