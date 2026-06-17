package main

import (
	"os"
	"strconv"
	"strings"

	"github.com/mattn/go-runewidth"

	"github.com/calper-ql/tide/internal/highlight"
	"github.com/calper-ql/tide/internal/input"
	"github.com/calper-ql/tide/internal/tui"
)

const (
	tabWidth = 4
	// maxHighlightLines caps re-lexing cost: bigger files render unhighlighted
	// rather than re-lex the whole buffer on every keystroke.
	maxHighlightLines = 10000
)

// editKind groups consecutive edits of the same sort into one undo step, so
// a run of typing undoes as a unit rather than a character at a time.
type editKind int

const (
	kindNone editKind = iota
	kindType
	kindDelete
)

// snapshot is a document's text + cursor at one point, for undo/redo.
type snapshot struct {
	lines  [][]rune
	cx, cy int
}

// doc is one open file: text as a slice of rune-lines (always at least one),
// a cursor, scroll offsets, and undo history. Splitting/joining on "\n" round
// trips exactly, including a file's trailing newline (which shows as a final
// empty line — where the cursor may rest).
type doc struct {
	path       string
	lines      [][]rune
	savedLines [][]rune // content as last loaded/saved; modified() diffs against it

	cx, cy  int // cursor: rune index within line cy, and line index
	top     int // first visible line
	left    int // horizontal scroll, in display columns
	goalCol int // preferred display column for vertical motion

	viewW, viewH int // editor viewport from the last render

	undo, redo []snapshot
	lastKind   editKind

	preview    bool // markdown viz vs raw (only meaningful for .md)
	previewTop int  // viz scroll offset

	hlLines [][]highlight.Span // cached per-line syntax spans
	hlReady bool               // false when hlLines needs recomputing

	modCached bool // cached modified() result
	modValid  bool // is modCached current?
}

func splitLines(data []byte) [][]rune {
	parts := strings.Split(string(data), "\n")
	lines := make([][]rune, len(parts))
	for i, p := range parts {
		lines[i] = []rune(p)
	}
	return lines
}

func newDoc(path string, data []byte) *doc {
	lines := splitLines(data)
	return &doc{path: path, lines: lines, savedLines: cloneLines(lines), preview: isMarkdown(path)}
}

// reload re-reads the file from disk, discarding unsaved changes and the undo
// history. The cursor is clamped back into range.
func (d *doc) reload() error {
	if d.path == "" {
		return os.ErrInvalid
	}
	data, err := os.ReadFile(d.path)
	if err != nil {
		return err
	}
	d.lines = splitLines(data)
	d.savedLines = cloneLines(d.lines)
	d.undo, d.redo = nil, nil
	d.lastKind = kindNone
	d.modValid = false
	d.hlReady = false
	d.previewTop = 0
	d.clamp()
	return nil
}

// modified reports whether the buffer differs from the on-disk baseline. It
// compares against the saved content (not a one-way "was edited" flag), so
// undoing back to the saved state clears the dirty marker. The result is
// cached and invalidated on each edit.
func (d *doc) modified() bool {
	if !d.modValid {
		d.modCached = !linesEqual(d.lines, d.savedLines)
		d.modValid = true
	}
	return d.modCached
}

func linesEqual(a, b [][]rune) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if len(a[i]) != len(b[i]) {
			return false
		}
		for j := range a[i] {
			if a[i][j] != b[i][j] {
				return false
			}
		}
	}
	return true
}

// isMarkdownPreview reports whether the doc should render as markdown viz
// (a .md file with preview on) rather than as an editable buffer.
func (d *doc) isMarkdownPreview() bool { return d.preview && isMarkdown(d.path) }

func (d *doc) line() []rune { return d.lines[d.cy] }

// bytes serializes the document; joining on "\n" inverts newDoc's split.
func (d *doc) bytes() []byte {
	parts := make([]string, len(d.lines))
	for i, l := range d.lines {
		parts[i] = string(l)
	}
	return []byte(strings.Join(parts, "\n"))
}

func (d *doc) save() error {
	if d.path == "" {
		return os.ErrInvalid
	}
	if err := os.WriteFile(d.path, d.bytes(), 0o644); err != nil {
		return err
	}
	d.savedLines = cloneLines(d.lines) // new on-disk baseline
	d.modValid = false
	return nil
}

// --- undo ---

func cloneLines(src [][]rune) [][]rune {
	dst := make([][]rune, len(src))
	for i, l := range src {
		dst[i] = append([]rune(nil), l...)
	}
	return dst
}

// beginEdit records an undo point when the edit kind changes, so same-kind
// runs (typing, or a stream of backspaces) collapse into one step.
func (d *doc) beginEdit(kind editKind) {
	if kind != d.lastKind {
		d.undo = append(d.undo, snapshot{cloneLines(d.lines), d.cx, d.cy})
		d.redo = nil
	}
	d.lastKind = kind
	d.modValid = false
	d.hlReady = false
}

// breakUndo ends the current run so the next edit starts a fresh undo step
// (called on cursor moves — typing after navigating is a new group).
func (d *doc) breakUndo() { d.lastKind = kindNone }

func (d *doc) Undo() {
	if len(d.undo) == 0 {
		return
	}
	d.redo = append(d.redo, snapshot{cloneLines(d.lines), d.cx, d.cy})
	s := d.undo[len(d.undo)-1]
	d.undo = d.undo[:len(d.undo)-1]
	d.lines, d.cx, d.cy = s.lines, s.cx, s.cy
	d.lastKind = kindNone
	d.modValid = false
	d.hlReady = false
	d.clamp()
}

func (d *doc) Redo() {
	if len(d.redo) == 0 {
		return
	}
	d.undo = append(d.undo, snapshot{cloneLines(d.lines), d.cx, d.cy})
	s := d.redo[len(d.redo)-1]
	d.redo = d.redo[:len(d.redo)-1]
	d.lines, d.cx, d.cy = s.lines, s.cx, s.cy
	d.lastKind = kindNone
	d.modValid = false
	d.hlReady = false
	d.clamp()
}

// --- editing ---

func (d *doc) insertRune(r rune) {
	d.beginEdit(kindType)
	l := d.line()
	l = append(l, 0)
	copy(l[d.cx+1:], l[d.cx:])
	l[d.cx] = r
	d.lines[d.cy] = l
	d.cx++
	d.setGoal()
}

func (d *doc) insertString(s string) {
	for _, r := range s {
		if r == '\n' {
			d.insertNewline()
			continue
		}
		if r == '\r' {
			continue
		}
		d.insertRune(r)
	}
}

func (d *doc) insertNewline() {
	d.beginEdit(kindType)
	l := d.line()
	tail := append([]rune(nil), l[d.cx:]...)
	d.lines[d.cy] = l[:d.cx]
	d.lines = append(d.lines, nil)
	copy(d.lines[d.cy+2:], d.lines[d.cy+1:])
	d.lines[d.cy+1] = tail
	d.cy++
	d.cx = 0
	d.setGoal()
}

func (d *doc) backspace() {
	d.beginEdit(kindDelete)
	if d.cx > 0 {
		l := d.line()
		copy(l[d.cx-1:], l[d.cx:])
		d.lines[d.cy] = l[:len(l)-1]
		d.cx--
	} else if d.cy > 0 {
		prev := d.lines[d.cy-1]
		d.cx = len(prev)
		d.lines[d.cy-1] = append(prev, d.line()...)
		d.lines = append(d.lines[:d.cy], d.lines[d.cy+1:]...)
		d.cy--
	}
	d.setGoal()
}

func (d *doc) deleteForward() {
	d.beginEdit(kindDelete)
	l := d.line()
	if d.cx < len(l) {
		copy(l[d.cx:], l[d.cx+1:])
		d.lines[d.cy] = l[:len(l)-1]
	} else if d.cy < len(d.lines)-1 {
		d.lines[d.cy] = append(l, d.lines[d.cy+1]...)
		d.lines = append(d.lines[:d.cy+1], d.lines[d.cy+2:]...)
	}
}

// --- cursor motion ---

func (d *doc) setGoal() { d.goalCol = displayCol(d.line(), d.cx) }

func (d *doc) clamp() {
	d.cy = clampInt(d.cy, 0, len(d.lines)-1)
	d.cx = clampInt(d.cx, 0, len(d.line()))
}

func (d *doc) moveLeft() {
	d.breakUndo()
	if d.cx > 0 {
		d.cx--
	} else if d.cy > 0 {
		d.cy--
		d.cx = len(d.line())
	}
	d.setGoal()
}

func (d *doc) moveRight() {
	d.breakUndo()
	if d.cx < len(d.line()) {
		d.cx++
	} else if d.cy < len(d.lines)-1 {
		d.cy++
		d.cx = 0
	}
	d.setGoal()
}

func (d *doc) moveUp() {
	d.breakUndo()
	if d.cy > 0 {
		d.cy--
		d.cx = colFromDisplay(d.line(), d.goalCol)
	}
}

func (d *doc) moveDown() {
	d.breakUndo()
	if d.cy < len(d.lines)-1 {
		d.cy++
		d.cx = colFromDisplay(d.line(), d.goalCol)
	}
}

func (d *doc) home()     { d.breakUndo(); d.cx = 0; d.setGoal() }
func (d *doc) end()      { d.breakUndo(); d.cx = len(d.line()); d.setGoal() }
func (d *doc) pageUp()   { d.breakUndo(); d.moveByPage(-1) }
func (d *doc) pageDown() { d.breakUndo(); d.moveByPage(1) }

func (d *doc) moveByPage(dir int) {
	step := max(d.viewH-1, 1)
	d.cy = clampInt(d.cy+dir*step, 0, len(d.lines)-1)
	d.cx = colFromDisplay(d.line(), d.goalCol)
}

// handleKey routes one key to the editor. Ctrl-combinations other than the
// app shortcuts (handled upstream) are ignored here.
func (d *doc) handleKey(ev input.Event) {
	switch ev.Key {
	case input.KeyRune:
		if ev.Mods&input.Ctrl != 0 {
			return
		}
		d.insertRune(ev.Rune)
	case input.KeyEnter:
		d.insertNewline()
	case input.KeyTab:
		d.insertRune('\t')
	case input.KeyBackspace:
		d.backspace()
	case input.KeyDelete:
		d.deleteForward()
	case input.KeyLeft:
		d.moveLeft()
	case input.KeyRight:
		d.moveRight()
	case input.KeyUp:
		d.moveUp()
	case input.KeyDown:
		d.moveDown()
	case input.KeyHome:
		d.home()
	case input.KeyEnd:
		d.end()
	case input.KeyPageUp:
		d.pageUp()
	case input.KeyPageDown:
		d.pageDown()
	}
}

// --- display geometry ---

// displayCol is the screen column at rune index cx, expanding tabs to the
// next tab stop and counting wide runes as two.
func displayCol(line []rune, cx int) int {
	col := 0
	for i := 0; i < cx && i < len(line); i++ {
		col += runeCells(line[i], col)
	}
	return col
}

// colFromDisplay is the inverse: the rune index whose start column is at or
// just past target (clicks/vertical motion snap to a rune boundary).
func colFromDisplay(line []rune, target int) int {
	col := 0
	for i := 0; i < len(line); i++ {
		if col >= target {
			return i
		}
		col += runeCells(line[i], col)
	}
	return len(line)
}

// runeCells is a rune's display width at column col (tabs depend on col).
func runeCells(r rune, col int) int {
	if r == '\t' {
		return tabWidth - col%tabWidth
	}
	if w := runewidth.RuneWidth(r); w > 0 {
		return w
	}
	return 1
}

// expandLine turns a logical line into display cells: tabs become spaces to
// the next stop, wide runes are followed by a 0 continuation marker, and
// control characters show as a middle dot. The result indexes by display
// column, which is what horizontal scrolling and drawing slice on.
func expandLine(line []rune) []rune {
	cells, _ := expandStyled(line, nil)
	return cells
}

// expandStyled turns a logical line into display cells and, when cats is
// non-nil (one category per source rune), a parallel per-cell style slice.
// Tabs expand to the next stop, wide runes get a 0 continuation cell, control
// chars show as a dot — and every emitted cell inherits its source rune's
// style, so highlighting survives expansion.
func expandStyled(line []rune, cats []highlight.Category) ([]rune, []tui.Style) {
	cells := make([]rune, 0, len(line))
	var styles []tui.Style
	if cats != nil {
		styles = make([]tui.Style, 0, len(line))
	}
	add := func(r rune, st tui.Style) {
		cells = append(cells, r)
		if styles != nil {
			styles = append(styles, st)
		}
	}
	col := 0
	for i, r := range line {
		st := stText
		if cats != nil && i < len(cats) {
			st = catStyle(cats[i])
		}
		switch {
		case r == '\t':
			n := tabWidth - col%tabWidth
			for k := 0; k < n; k++ {
				add(' ', st)
			}
			col += n
		case r < 0x20 || r == 0x7f:
			add('·', st)
			col++
		default:
			w := runewidth.RuneWidth(r)
			if w < 1 {
				w = 1
			}
			add(r, st)
			if w == 2 {
				add(0, st)
			}
			col += w
		}
	}
	return cells, styles
}

// ensureHighlight re-lexes the buffer when it has changed, skipping files too
// large to re-lex per keystroke (they render unhighlighted).
func (d *doc) ensureHighlight() {
	if d.hlReady {
		return
	}
	d.hlReady = true
	if len(d.lines) > maxHighlightLines {
		d.hlLines = nil
		return
	}
	d.hlLines = highlight.Lines(d.path, string(d.bytes()))
}

// lineCats returns one syntax Category per source rune of line ln, or nil
// when the line is unhighlighted. Spans for a line reconstruct that line
// exactly (highlight's round-trip invariant), so runes align by position.
func (d *doc) lineCats(ln int) []highlight.Category {
	if d.hlLines == nil || ln >= len(d.hlLines) {
		return nil
	}
	cats := make([]highlight.Category, len(d.lines[ln]))
	idx := 0
	for _, sp := range d.hlLines[ln] {
		for range sp.Text {
			if idx < len(cats) {
				cats[idx] = sp.Cat
			}
			idx++
		}
	}
	return cats
}

func catStyle(c highlight.Category) tui.Style {
	switch c {
	case highlight.CatKeyword:
		return stHlKeyword
	case highlight.CatBuiltin:
		return stHlBuiltin
	case highlight.CatType:
		return stHlType
	case highlight.CatString:
		return stHlString
	case highlight.CatNumber:
		return stHlNumber
	case highlight.CatComment:
		return stHlComment
	case highlight.CatError:
		return stHlError
	}
	return stText
}

// clampScroll bounds the scroll offsets without forcing the cursor into
// view, so wheel scrolling can move the viewport away from the cursor.
func (d *doc) clampScroll() {
	d.top = clampInt(d.top, 0, max(len(d.lines)-1, 0))
	d.left = max(d.left, 0)
}

// scrollToCursor pulls the viewport so the cursor is visible. Called from the
// input handlers after cursor motion/edits — never on a pure wheel scroll.
func (d *doc) scrollToCursor() {
	if d.viewH <= 0 || d.viewW <= 0 {
		return
	}
	if d.cy < d.top {
		d.top = d.cy
	}
	if d.cy >= d.top+d.viewH {
		d.top = d.cy - d.viewH + 1
	}
	dc := displayCol(d.line(), d.cx)
	if dc < d.left {
		d.left = dc
	}
	if dc >= d.left+d.viewW {
		d.left = dc - d.viewW + 1
	}
	d.top = max(d.top, 0)
	d.left = max(d.left, 0)
}

// --- App integration ---

func (a *App) saveActive() {
	if d := a.activeDoc(); d != nil {
		_ = d.save()
	}
}

func (a *App) reloadActive() {
	if d := a.activeDoc(); d != nil {
		_ = d.reload()
	}
}

func gutterWidth(nLines int) int {
	return len(strconv.Itoa(nLines)) + 2 // digits + a leading and trailing space
}

func (a *App) drawEditor(buf *tui.Buffer, r tui.Rect) {
	if r.W <= 0 || r.H <= 0 {
		return
	}
	d := a.activeDoc()
	if d == nil {
		msg := "teddy — open a file from the explorer"
		drawIn(buf, r, max((r.W-strWidth(msg))/2, 0), r.H/2, stHint, msg)
		return
	}
	if d.isMarkdownPreview() {
		a.drawMarkdown(buf, r, d)
		return
	}

	gw := gutterWidth(len(d.lines))
	viewW := r.W - gw
	if viewW < 1 {
		return
	}
	d.viewW, d.viewH = viewW, r.H
	d.clampScroll()
	d.ensureHighlight()

	for row := 0; row < r.H; row++ {
		ln := d.top + row
		y := r.Y + row
		if ln >= len(d.lines) {
			continue
		}
		gst := stGutter
		if ln == d.cy {
			gst = stGutterActive
		}
		num := strconv.Itoa(ln + 1)
		drawIn(buf, tui.Rect{X: r.X, Y: y, W: gw, H: 1}, gw-1-len(num), 0, gst, num)

		cells, styles := expandStyled(d.lines[ln], d.lineCats(ln))
		drawEditorLine(buf, r.X+gw, y, viewW, cells, d.left, stText, styles)
	}

	curCol := displayCol(d.line(), d.cx) - d.left
	a.screen.SetCursor(r.X+gw+curCol, r.Y+(d.cy-d.top))
	a.screen.ShowCursor()
}

// drawMarkdown renders the doc as markdown viz (read-only): no gutter, no
// cursor, its own scroll offset. Toggle back to raw to edit.
func (a *App) drawMarkdown(buf *tui.Buffer, r tui.Rect, d *doc) {
	width := r.W - 2 // a one-column margin each side
	if width < 1 {
		return
	}
	lines := renderMarkdown(string(d.bytes()), width)
	d.viewH = r.H
	d.previewTop = clampInt(d.previewTop, 0, max(len(lines)-1, 0))
	for i := 0; i < r.H; i++ {
		li := d.previewTop + i
		if li >= len(lines) {
			break
		}
		drawMdLine(buf, r.X+1, r.Y+i, width, lines[li])
	}
}

// drawEditorLine paints one line's display cells through the horizontal
// scroll window. cells indexes by display column (with 0 markers after wide
// runes); styles, when non-nil, is parallel to cells.
func drawEditorLine(buf *tui.Buffer, x0, y, width int, cells []rune, left int, base tui.Style, styles []tui.Style) {
	i, dc := 0, left
	for i < width {
		r := ' '
		st := base
		if dc < len(cells) {
			r = cells[dc]
			if styles != nil && dc < len(styles) {
				st = styles[dc]
			}
		}
		if r == 0 { // wide-rune continuation exposed at the window edge
			buf.SetCell(x0+i, y, ' ', st)
			i++
			dc++
			continue
		}
		w := runewidth.RuneWidth(r)
		if w < 1 {
			w = 1
		}
		if i+w > width { // a wide rune straddling the right edge: show a space
			buf.SetCell(x0+i, y, ' ', st)
			i++
			dc++
			continue
		}
		buf.Set(x0+i, y, r, st)
		i += w
		dc += w
	}
}

func clampInt(v, lo, hi int) int {
	if hi < lo {
		return lo
	}
	return min(max(v, lo), hi)
}
