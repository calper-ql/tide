package main

import (
	"bytes"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/calper-ql/tide/internal/input"
	"github.com/calper-ql/tide/internal/tui"
)

// gitFile is one changed path in the working tree, as reported by
// `git status --porcelain`. A file can surface twice — once staged, once
// not (an "MM" entry) — so staged is what distinguishes the two rows.
type gitFile struct {
	rel    string // path relative to the repo top (git's own spelling)
	abs    string // absolute, for opening in the editor
	code   rune   // single-letter status: M A D R C ? U
	staged bool   // shown under "Staged Changes" vs "Changes"
}

// gitRowKind tags a visible panel row as a group header or a file entry.
type gitRowKind int

const (
	grHeader gitRowKind = iota
	grFile
)

// gitRow is one rendered/hit-tested row of the source-control list.
type gitRow struct {
	kind  gitRowKind
	label string  // header text
	file  gitFile // valid when kind == grFile
}

// gitState is the Source Control activity's state: the repo it found, the
// parsed status, the commit box, and the scroll/selection + hit rects.
type gitState struct {
	loaded    bool   // refreshGit has run at least once this session
	available bool   // root sits inside a git work tree
	repoTop   string // `git rev-parse --show-toplevel`
	branch    string
	ahead     int
	behind    int
	errText   string // last git error (shown on the commit row)

	staged    []gitFile
	unstaged  []gitFile
	untracked []gitFile
	rows      []gitRow // headers + files, the render/hit-test order

	commitMsg   string
	commitFocus bool // the commit box has keyboard focus

	top int // first visible row (scroll)
	sel int // selected row

	inputHit   tui.Rect // commit box
	commitHit  tui.Rect // "Commit" button
	refreshHit tui.Rect // the ⟳ glyph
	contentY   int      // screen y of the first list row
	stageX     int      // absolute column of the per-row stage/unstage glyph
}

// gitActive reports whether keystrokes belong to the commit box (Source
// Control selected, focused, and the sidebar visible).
func (a *App) gitActive() bool {
	return a.selected == 2 && a.git.commitFocus && !a.sideCollapsed
}

// gitCmd runs git in dir and returns its combined stdout, or on failure the
// stderr text alongside the error (so callers can surface a message).
func (a *App) gitCmd(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		if errb.Len() > 0 {
			return errb.String(), err
		}
		return out.String(), err
	}
	return out.String(), nil
}

// refreshGit re-reads the repo: whether root is under a work tree, the branch
// and ahead/behind, and the staged/unstaged file lists. It is cheap on a
// local repo, so it runs synchronously after every mutation.
func (a *App) refreshGit() {
	g := &a.git
	g.loaded = true
	g.errText = ""

	top, err := a.gitCmd(a.root, "rev-parse", "--show-toplevel")
	if err != nil {
		g.available, g.repoTop = false, ""
		g.staged, g.unstaged, g.rows = nil, nil, nil
		return
	}
	g.available = true
	g.repoTop = strings.TrimSpace(top)

	out, err := a.gitCmd(g.repoTop, "status", "--porcelain=v1", "-b", "-z")
	if err != nil {
		g.errText = "git status failed"
		return
	}
	g.parseStatus(out)
	g.rebuildRows()
}

// autoRefreshGit is the poll-tick refresh: it re-reads status only when Source
// Control is actually on screen (the panel is open, or a diff tab is focused),
// so teddy does no git work when git isn't visible. A pending commit error is
// preserved so a background poll never swallows a message the user hasn't seen.
func (a *App) autoRefreshGit() {
	d := a.activeDoc()
	diffVisible := d != nil && d.diff != nil
	panelVisible := a.selected == 2 && !a.sideCollapsed
	if !panelVisible && !diffVisible {
		return
	}
	prevErr := a.git.errText
	a.refreshGit()
	if a.git.errText == "" {
		a.git.errText = prevErr
	}
	if diffVisible {
		a.rebuildDiffContent(d)
	}
}

// parseStatus fills the branch header and file lists from `git status -b -z`
// output (records NUL-separated; rename/copy entries carry a trailing source
// record that is consumed and ignored).
func (g *gitState) parseStatus(out string) {
	g.branch, g.ahead, g.behind = "", 0, 0
	g.staged, g.unstaged, g.untracked = g.staged[:0], g.unstaged[:0], g.untracked[:0]

	recs := strings.Split(out, "\x00")
	for i := 0; i < len(recs); i++ {
		rec := recs[i]
		if rec == "" {
			continue
		}
		if strings.HasPrefix(rec, "## ") {
			g.parseBranch(rec[3:])
			continue
		}
		if len(rec) < 4 {
			continue
		}
		x, y, path := rune(rec[0]), rune(rec[1]), rec[3:]
		if x == 'R' || x == 'C' || y == 'R' || y == 'C' {
			i++ // the rename/copy source is the next record
		}
		f := gitFile{rel: path, abs: filepath.Join(g.repoTop, filepath.FromSlash(path))}
		if x == '?' && y == '?' { // untracked: its own section
			f.code = '?'
			g.untracked = append(g.untracked, f)
			continue
		}
		if x != ' ' && x != '?' {
			sf := f
			sf.code, sf.staged = x, true
			g.staged = append(g.staged, sf)
		}
		if y != ' ' {
			uf := f
			uf.code, uf.staged = y, false
			g.unstaged = append(g.unstaged, uf)
		}
	}
}

// parseBranch pulls the branch name and ahead/behind counts out of the
// porcelain branch header, e.g. "main...origin/main [ahead 1, behind 2]",
// "No commits yet on main", or "HEAD (no branch)".
func (g *gitState) parseBranch(s string) {
	name := strings.TrimPrefix(s, "No commits yet on ")
	if i := strings.Index(name, "..."); i >= 0 {
		name = name[:i]
	}
	if i := strings.Index(name, " ["); i >= 0 {
		name = name[:i]
	}
	if i := strings.Index(name, " "); i >= 0 { // "HEAD (no branch)"
		name = name[:i]
	}
	g.branch = strings.TrimSpace(name)
	if i := strings.Index(s, "ahead "); i >= 0 {
		fmt.Sscanf(s[i:], "ahead %d", &g.ahead)
	}
	if i := strings.Index(s, "behind "); i >= 0 {
		fmt.Sscanf(s[i:], "behind %d", &g.behind)
	}
}

// rebuildRows rebuilds the flat header+file list from the two groups.
func (g *gitState) rebuildRows() {
	g.rows = g.rows[:0]
	if len(g.staged) > 0 {
		g.rows = append(g.rows, gitRow{kind: grHeader, label: fmt.Sprintf("Staged Changes (%d)", len(g.staged))})
		for _, f := range g.staged {
			g.rows = append(g.rows, gitRow{kind: grFile, file: f})
		}
	}
	if len(g.unstaged) > 0 {
		g.rows = append(g.rows, gitRow{kind: grHeader, label: fmt.Sprintf("Changes (%d)", len(g.unstaged))})
		for _, f := range g.unstaged {
			g.rows = append(g.rows, gitRow{kind: grFile, file: f})
		}
	}
	if len(g.untracked) > 0 {
		g.rows = append(g.rows, gitRow{kind: grHeader, label: fmt.Sprintf("Untracked (%d)", len(g.untracked))})
		for _, f := range g.untracked {
			g.rows = append(g.rows, gitRow{kind: grFile, file: f})
		}
	}
	g.sel = clampInt(g.sel, 0, max(len(g.rows)-1, 0))
}

// toggleStage stages an unstaged file or unstages a staged one, then refreshes.
func (a *App) toggleStage(f gitFile) {
	var err error
	var out string
	if f.staged {
		out, err = a.gitCmd(a.git.repoTop, "reset", "-q", "HEAD", "--", f.rel)
	} else {
		out, err = a.gitCmd(a.git.repoTop, "add", "--", f.rel)
	}
	a.refreshGit()
	if err != nil {
		a.git.errText = firstLine(out)
	}
}

// commit records the staged changes with the commit-box message. It guards the
// two common empties so git's own error never surprises the user.
func (a *App) commit() {
	g := &a.git
	if len(g.staged) == 0 {
		g.errText = "nothing staged to commit"
		return
	}
	if strings.TrimSpace(g.commitMsg) == "" {
		g.errText = "enter a commit message"
		g.commitFocus = true
		return
	}
	out, err := a.gitCmd(g.repoTop, "commit", "-m", g.commitMsg)
	a.refreshGit()
	if err != nil {
		g.errText = firstLine(out)
		return
	}
	g.commitMsg, g.commitFocus = "", false
}

// handleGitKey routes a key to the focused commit box.
func (a *App) handleGitKey(ev input.Event) {
	g := &a.git
	switch ev.Key {
	case input.KeyRune:
		if ev.Mods&input.Ctrl != 0 {
			return
		}
		g.commitMsg += string(ev.Rune)
	case input.KeyBackspace:
		if r := []rune(g.commitMsg); len(r) > 0 {
			g.commitMsg = string(r[:len(r)-1])
		}
	case input.KeyEnter:
		a.commit()
	case input.KeyEscape:
		g.commitFocus = false
	}
}

// clickGit dispatches a click in the Source Control panel: the refresh glyph,
// the commit box/button, a per-row stage toggle, or a file (opened).
func (a *App) clickGit(x, y int) {
	g := &a.git
	if !g.available {
		return
	}
	if g.refreshHit.Contains(x, y) {
		a.refreshGit()
		return
	}
	if g.commitHit.Contains(x, y) {
		a.commit()
		return
	}
	if g.inputHit.Contains(x, y) {
		g.commitFocus = true
		return
	}
	if y >= g.contentY {
		idx := g.top + (y - g.contentY)
		if idx >= 0 && idx < len(g.rows) && g.rows[idx].kind == grFile {
			g.sel = idx
			if x >= g.stageX { // the trailing +/− glyph
				a.toggleStage(g.rows[idx].file)
			} else {
				a.openDiff(g.rows[idx].file) // read-only changes view
			}
			return
		}
	}
	g.commitFocus = false
}

// gitStatusStyle colors a status letter: green added/untracked, yellow
// modified, red deleted, blue renamed/copied, accent for conflicts.
func gitStatusStyle(code rune) tui.Style {
	switch code {
	case 'A', '?':
		return tui.DefaultStyle.WithFG(tui.Green)
	case 'M', 'T':
		return tui.DefaultStyle.WithFG(tui.Yellow)
	case 'D':
		return tui.DefaultStyle.WithFG(tui.Red)
	case 'R', 'C':
		return tui.DefaultStyle.WithFG(tui.Blue)
	case 'U':
		return tui.DefaultStyle.WithFG(tui.BrightRed).Bolded()
	default:
		return stDim
	}
}

func (a *App) drawGit(buf *tui.Buffer, inner tui.Rect) {
	g := &a.git
	if !g.loaded {
		a.refreshGit() // first paint of the panel: load lazily
	}
	if !g.available {
		drawIn(buf, inner, 1, 2, stHint, "Not a git repository")
		return
	}

	// Row 1: branch, ahead/behind, and a refresh glyph pinned right.
	branch := g.branch
	if branch == "" {
		branch = "detached"
	}
	x := drawIn(buf, inner, 1, 1, stAccent, "⎇ "+branch)
	ab := ""
	if g.ahead > 0 {
		ab += fmt.Sprintf(" ↑%d", g.ahead)
	}
	if g.behind > 0 {
		ab += fmt.Sprintf(" ↓%d", g.behind)
	}
	if ab != "" {
		drawIn(buf, inner, x-inner.X, 1, stDim, ab)
	}
	g.refreshHit = tui.Rect{X: inner.X + inner.W - 1, Y: inner.Y + 1, W: 1, H: 1}
	drawIn(buf, inner, inner.W-1, 1, stDim, "⟳")

	// Row 2: the commit-message box (focusable, like the search box).
	box := tui.Rect{X: inner.X, Y: inner.Y + 2, W: inner.W, H: 1}
	g.inputHit = box
	ist := stStatusDim
	if a.gitActive() {
		ist = stStatus
	}
	buf.Fill(box, ' ', ist)
	if g.commitMsg == "" && !a.gitActive() {
		drawIn(buf, box, 1, 0, ist, "Message…")
	} else {
		shown := g.commitMsg
		if maxM := box.W - 2; maxM > 0 && strWidth(shown) > maxM {
			shown = string([]rune(shown)[strLen(shown)-maxM:]) // keep the tail near the cursor
		}
		mx := drawIn(buf, box, 1, 0, ist, shown)
		if a.gitActive() {
			a.screen.SetCursor(min(mx, box.X+box.W-1), box.Y)
			a.screen.ShowCursor()
		}
	}

	// Row 3: the Commit button (enabled once something is staged + a message
	// typed), with any git error shown to its right.
	label := "✓ Commit"
	if n := len(g.staged); n > 0 {
		label = fmt.Sprintf("✓ Commit (%d)", n)
	}
	cst := stDim
	if len(g.staged) > 0 && strings.TrimSpace(g.commitMsg) != "" {
		cst = stAccent
	}
	g.commitHit = tui.Rect{X: inner.X, Y: inner.Y + 3, W: inner.W, H: 1}
	drawIn(buf, inner, 1, 3, cst, label)
	if g.errText != "" {
		if ex := inner.W - strWidth(g.errText) - 1; ex > strWidth(label)+2 {
			drawIn(buf, inner, ex, 3, stHlError, g.errText)
		}
	}

	// Rows 4+: the file list, headers and entries.
	const top = 4
	rows := inner.H - top
	if rows < 1 {
		return
	}
	g.contentY = inner.Y + top
	g.stageX = inner.X + inner.W - 2
	if len(g.rows) == 0 {
		drawIn(buf, inner, 1, top, stHint, "No changes")
		return
	}

	if g.sel < g.top {
		g.top = g.sel
	}
	if g.sel >= g.top+rows {
		g.top = g.sel - rows + 1
	}
	g.top = clampInt(g.top, 0, max(len(g.rows)-rows, 0))

	for i := 0; i < rows; i++ {
		idx := g.top + i
		if idx >= len(g.rows) {
			break
		}
		row := g.rows[idx]
		y := top + i
		if row.kind == grHeader {
			drawIn(buf, inner, 1, y, stSideTitle, row.label)
			continue
		}
		f := row.file
		st, cs := stText, gitStatusStyle(f.code)
		if idx == g.sel {
			buf.Fill(tui.Rect{X: inner.X, Y: inner.Y + y, W: inner.W, H: 1}, ' ', stSelected)
			st, cs = stSelected, stSelected
		}
		drawIn(buf, inner, 1, y, cs, string(f.code))
		name := shortenPath(f.rel, inner.W-3-2) // status col + trailing glyph
		drawIn(buf, inner, 3, y, st, name)
		glyph := "+"
		if f.staged {
			glyph = "−"
		}
		gst := stDim
		if idx == g.sel {
			gst = stSelected
		}
		drawIn(buf, inner, inner.W-2, y, gst, glyph)
	}
}

// firstLine returns the first non-empty line of s, trimmed — for surfacing a
// git error compactly on the commit row.
func firstLine(s string) string {
	for _, ln := range strings.Split(s, "\n") {
		if ln = strings.TrimSpace(ln); ln != "" {
			return ln
		}
	}
	return "git error"
}
