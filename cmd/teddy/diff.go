package main

import (
	"bytes"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/calper-ql/tide/internal/input"
	"github.com/calper-ql/tide/internal/tui"
)

// diffMode selects how a diff renders. The default is set once here — flip
// this line to change teddy's out-of-the-box layout; Ctrl+D (or the status-bar
// pill) toggles it live for every open diff.
type diffMode int

const (
	diffInline diffMode = iota
	diffSideBySide
)

const defaultDiffMode = diffSideBySide

// diffLineKind tags one parsed unified-diff line.
type diffLineKind int

const (
	dlContext diffLineKind = iota
	dlAdd
	dlDel
	dlHunk // an @@ hunk header
)

// diffLine is one line of a parsed unified diff. text is the payload with the
// leading +/-/space stripped; oldNum/newNum are 1-based line numbers (0 when
// the line doesn't exist on that side).
type diffLine struct {
	kind           diffLineKind
	oldNum, newNum int
	text           string
}

// diffDoc is a read-only changes view for one file: the parsed diff plus its
// own scroll offset. It hangs off a doc (doc.diff) so it rides the tab strip.
type diffDoc struct {
	path   string // real absolute path of the file
	rel    string // repo-relative spelling (git's)
	code   rune   // status letter, so a reload knows untracked from tracked
	staged bool   // staged diff (--cached) vs working-tree diff
	lines  []diffLine
	binary bool
	top    int // scroll offset, in rendered rows
}

// newDiffDoc wraps a diffDoc in a minimal read-only doc so it can live as a
// tab. The single empty line keeps doc invariants (always ≥1 line) without
// ever being shown — drawDiff renders dd instead.
func newDiffDoc(dd *diffDoc) *doc {
	return &doc{path: dd.path, lines: [][]rune{{}}, savedLines: [][]rune{{}}, diff: dd}
}

// readOnly reports whether the doc is a non-editable view (a diff).
func (d *doc) readOnly() bool { return d.diff != nil }

// buildDiff produces the diff for a source-control entry: `git diff` (working
// tree), `git diff --cached` (staged), or — for an untracked file — the whole
// file synthesized as additions (git has no diff for it).
func (a *App) buildDiff(f gitFile) (*diffDoc, error) {
	dd := &diffDoc{path: f.abs, rel: f.rel, code: f.code, staged: f.staged}
	if f.code == '?' {
		data, err := os.ReadFile(f.abs)
		if err != nil {
			return nil, err
		}
		if bytes.IndexByte(data[:min(len(data), 1024)], 0) >= 0 {
			dd.binary = true
			return dd, nil
		}
		dd.lines = untrackedDiff(data)
		return dd, nil
	}
	args := []string{"diff", "--no-color"}
	if f.staged {
		args = append(args, "--cached")
	}
	args = append(args, "--", f.rel)
	out, err := a.gitCmd(a.git.repoTop, args...)
	if err != nil {
		return nil, err
	}
	if strings.Contains(out, "\nBinary files ") || strings.HasPrefix(out, "Binary files ") {
		dd.binary = true
		return dd, nil
	}
	dd.lines = parseDiff(out)
	return dd, nil
}

// untrackedDiff turns a brand-new file's contents into an all-additions diff,
// with a synthetic hunk header covering the whole file.
func untrackedDiff(data []byte) []diffLine {
	raw := strings.Split(string(data), "\n")
	if n := len(raw); n > 0 && raw[n-1] == "" {
		raw = raw[:n-1] // drop the trailing-newline artifact
	}
	out := make([]diffLine, 0, len(raw)+1)
	out = append(out, diffLine{kind: dlHunk, text: fmt.Sprintf("@@ -0,0 +1,%d @@", len(raw))})
	for i, ln := range raw {
		out = append(out, diffLine{kind: dlAdd, newNum: i + 1, text: ln})
	}
	return out
}

// parseDiff turns `git diff` unified output into diffLines, dropping the file
// headers (diff --git, index, ---/+++, mode/rename lines, "no newline").
func parseDiff(out string) []diffLine {
	var lines []diffLine
	oldN, newN := 0, 0
	inHunk := false // pre-hunk lines (diff --git, ---/+++, mode, ...) are headers
	for _, raw := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(raw, "diff --git"):
			inHunk = false // a new file's header block begins
		case strings.HasPrefix(raw, "@@"):
			oldN, newN = parseHunkHeader(raw)
			lines = append(lines, diffLine{kind: dlHunk, text: raw})
			inHunk = true
		case !inHunk:
			// index / --- / +++ / new file / deleted / mode / rename — skip
		case raw == "":
			// trailing split artifact (a real blank context line is " ")
		case raw[0] == '+':
			lines = append(lines, diffLine{kind: dlAdd, newNum: newN, text: raw[1:]})
			newN++
		case raw[0] == '-':
			lines = append(lines, diffLine{kind: dlDel, oldNum: oldN, text: raw[1:]})
			oldN++
		case raw[0] == ' ':
			lines = append(lines, diffLine{kind: dlContext, oldNum: oldN, newNum: newN, text: raw[1:]})
			oldN++
			newN++
		case raw[0] == '\\':
			// "\ No newline at end of file" — skip
		}
	}
	return lines
}

// parseHunkHeader reads the starting old/new line numbers from "@@ -a,b +c,d @@".
func parseHunkHeader(s string) (old, new int) {
	if i := strings.IndexByte(s, '-'); i >= 0 {
		fmt.Sscanf(s[i:], "-%d", &old)
	}
	if i := strings.IndexByte(s, '+'); i >= 0 {
		fmt.Sscanf(s[i:], "+%d", &new)
	}
	return old, new
}

// --- app integration ---

// openDiff opens (or refocuses + refreshes) a read-only diff tab for a
// source-control entry.
func (a *App) openDiff(f gitFile) {
	if !a.git.available {
		return
	}
	dd, err := a.buildDiff(f)
	if err != nil {
		a.git.errText = firstLine(err.Error())
		return
	}
	a.tabPinActive = true
	for i, d := range a.tabs {
		if d.diff != nil && d.diff.path == f.abs && d.diff.staged == f.staged {
			d.diff = dd // refresh in place
			a.active = i
			return
		}
	}
	a.tabs = append(a.tabs, newDiffDoc(dd))
	a.active = len(a.tabs) - 1
}

// rebuildDiff re-runs the status and the diff behind an open diff tab
// (Ctrl+R / reload).
func (a *App) rebuildDiff(d *doc) {
	if d.diff == nil {
		return
	}
	a.refreshGit()
	a.rebuildDiffContent(d)
}

// rebuildDiffContent re-runs just this file's diff (no status read), keeping
// the current scroll position. Used by the poll after it already refreshed.
func (a *App) rebuildDiffContent(d *doc) {
	if d.diff == nil {
		return
	}
	top := d.diff.top
	dd, err := a.buildDiff(gitFile{abs: d.diff.path, rel: d.diff.rel, code: d.diff.code, staged: d.diff.staged})
	if err != nil {
		return
	}
	dd.top = top
	d.diff = dd
}

func (a *App) toggleDiffMode() {
	if a.diffMode == diffInline {
		a.diffMode = diffSideBySide
	} else {
		a.diffMode = diffInline
	}
}

// diffKey handles keys while a diff tab is focused: only scrolling; the view
// is read-only.
func (a *App) diffKey(d *doc, ev input.Event) {
	dd := d.diff
	page := max(d.viewH-1, 1)
	switch ev.Key {
	case input.KeyDown:
		dd.top++
	case input.KeyUp:
		dd.top = max(dd.top-1, 0)
	case input.KeyPageDown:
		dd.top += page
	case input.KeyPageUp:
		dd.top = max(dd.top-page, 0)
	case input.KeyHome:
		dd.top = 0
	}
}

// --- rendering ---

func (a *App) drawDiff(buf *tui.Buffer, r tui.Rect, d *doc) {
	dd := d.diff
	if r.W < 1 || r.H < 1 {
		return
	}
	d.viewH = r.H
	if dd.binary {
		drawIn(buf, r, 1, 0, stHint, "Binary file — no textual diff")
		return
	}
	if len(dd.lines) == 0 {
		drawIn(buf, r, 1, 0, stHint, "No changes")
		return
	}
	if a.diffMode == diffSideBySide {
		a.drawDiffSide(buf, r, dd)
	} else {
		a.drawDiffInline(buf, r, dd)
	}
}

// drawDiffInline renders the unified diff: a line-number gutter, a colored
// +/-/space sign, then the text. Hunk headers span the width.
func (a *App) drawDiffInline(buf *tui.Buffer, r tui.Rect, dd *diffDoc) {
	maxNum := 1
	for _, l := range dd.lines {
		maxNum = max(maxNum, max(l.oldNum, l.newNum))
	}
	gw := len(strconv.Itoa(maxNum)) + 1
	dd.top = clampInt(dd.top, 0, max(len(dd.lines)-1, 0))

	for i := 0; i < r.H; i++ {
		idx := dd.top + i
		if idx >= len(dd.lines) {
			break
		}
		l := dd.lines[idx]
		y := r.Y + i
		if l.kind == dlHunk {
			drawIn(buf, r, 0, i, stDiffHunk, l.text)
			continue
		}
		num, sign, st := l.newNum, ' ', stDiffCtx
		switch l.kind {
		case dlAdd:
			sign, st = '+', stDiffAdd
		case dlDel:
			num, sign, st = l.oldNum, '-', stDiffDel
		}
		ns := strconv.Itoa(num)
		drawIn(buf, r, gw-1-len(ns), i, stDiffNum, ns)
		buf.Set(r.X+gw, y, rune(sign), st)
		cells := expandLine([]rune(l.text))
		drawEditorLine(buf, r.X+gw+1, y, max(r.W-gw-1, 0), cells, 0, st, nil)
	}
}

// sideRow is one rendered row of the side-by-side view: an old-side cell, a
// new-side cell, or a full-width hunk header. A kind of -1 means "no cell".
type sideRow struct {
	hunk  bool
	htext string
	lkind diffLineKind
	lnum  int
	ltext string
	rkind diffLineKind
	rnum  int
	rtext string
}

const noCell diffLineKind = -1

// buildSideRows pairs each run of deletions with the following additions so
// they sit across from each other; context lines occupy both sides.
func buildSideRows(lines []diffLine) []sideRow {
	var rows []sideRow
	var dels, adds []diffLine
	flush := func() {
		n := max(len(dels), len(adds))
		for i := 0; i < n; i++ {
			row := sideRow{lkind: noCell, rkind: noCell}
			if i < len(dels) {
				row.lkind, row.lnum, row.ltext = dlDel, dels[i].oldNum, dels[i].text
			}
			if i < len(adds) {
				row.rkind, row.rnum, row.rtext = dlAdd, adds[i].newNum, adds[i].text
			}
			rows = append(rows, row)
		}
		dels, adds = dels[:0], adds[:0]
	}
	for _, l := range lines {
		switch l.kind {
		case dlDel:
			dels = append(dels, l)
		case dlAdd:
			adds = append(adds, l)
		case dlContext:
			flush()
			rows = append(rows, sideRow{
				lkind: dlContext, lnum: l.oldNum, ltext: l.text,
				rkind: dlContext, rnum: l.newNum, rtext: l.text,
			})
		case dlHunk:
			flush()
			rows = append(rows, sideRow{hunk: true, htext: l.text})
		}
	}
	flush()
	return rows
}

// drawDiffSide renders the old file on the left and the new file on the right,
// split down the middle with a border column between them.
func (a *App) drawDiffSide(buf *tui.Buffer, r tui.Rect, dd *diffDoc) {
	rows := buildSideRows(dd.lines)
	dd.top = clampInt(dd.top, 0, max(len(rows)-1, 0))

	sep := r.X + r.W/2
	leftW := sep - r.X
	rightX := sep + 1
	rightW := r.X + r.W - rightX

	maxOld, maxNew := 1, 1
	for _, l := range dd.lines {
		maxOld = max(maxOld, l.oldNum)
		maxNew = max(maxNew, l.newNum)
	}
	lnW := len(strconv.Itoa(maxOld)) + 1
	rnW := len(strconv.Itoa(maxNew)) + 1

	for i := 0; i < r.H; i++ {
		idx := dd.top + i
		if idx >= len(rows) {
			break
		}
		row := rows[idx]
		y := r.Y + i
		if row.hunk {
			drawIn(buf, r, 0, i, stDiffHunk, row.htext)
			continue
		}
		buf.Set(sep, y, '│', stBorder)
		if row.lkind != noCell {
			drawSideCell(buf, r.X, y, leftW, lnW, row.lnum, row.lkind, row.ltext)
		}
		if row.rkind != noCell {
			drawSideCell(buf, rightX, y, rightW, rnW, row.rnum, row.rkind, row.rtext)
		}
	}
}

// drawSideCell paints one half of a side-by-side row: a right-aligned line
// number then the colored text.
func drawSideCell(buf *tui.Buffer, x0, y, w, numW, num int, kind diffLineKind, text string) {
	if w < 1 {
		return
	}
	ns := strconv.Itoa(num)
	drawIn(buf, tui.Rect{X: x0, Y: y, W: numW, H: 1}, numW-1-len(ns), 0, stDiffNum, ns)
	st := stDiffCtx
	switch kind {
	case dlAdd:
		st = stDiffAdd
	case dlDel:
		st = stDiffDel
	}
	cells := expandLine([]rune(text))
	drawEditorLine(buf, x0+numW, y, max(w-numW, 0), cells, 0, st, nil)
}
