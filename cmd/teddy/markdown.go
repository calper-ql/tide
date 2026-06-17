package main

import (
	"path/filepath"
	"strings"

	"github.com/mattn/go-runewidth"

	"github.com/calper-ql/tide/internal/tui"
)

// isMarkdown reports whether a path is a markdown file (gets the viz toggle).
func isMarkdown(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".md", ".markdown":
		return true
	}
	return false
}

// mdCell is one styled display cell in rendered markdown.
type mdCell struct {
	r  rune
	st tui.Style
}

// togglePreview flips the active markdown doc between viz and raw.
func (a *App) togglePreview() {
	if d := a.activeDoc(); d != nil && isMarkdown(d.path) {
		d.preview = !d.preview
	}
}

// renderMarkdown turns markdown source into styled, width-wrapped display
// lines using the 16-color palette. It is a pragmatic line-oriented renderer
// — headings, emphasis, inline code, links, lists, block quotes, rules, and
// fenced code — not a full CommonMark parser; that is enough for a viz toggle.
func renderMarkdown(src string, width int) [][]mdCell {
	if width < 1 {
		width = 1
	}
	var out [][]mdCell
	inFence := false
	for _, line := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(line)

		if strings.HasPrefix(trimmed, "```") {
			inFence = !inFence
			out = append(out, mdRule(width))
			continue
		}
		if inFence {
			out = append(out, codeLine(line, width))
			continue
		}
		switch {
		case trimmed == "":
			out = append(out, nil)
		case isRule(trimmed):
			out = append(out, mdRule(width))
		case headingLevel(trimmed) > 0:
			text := strings.TrimLeft(trimmed, "# ")
			out = append(out, wrapCells(inline(text, stMdHeading), width)...)
		case strings.HasPrefix(trimmed, ">"):
			body := inline(strings.TrimSpace(strings.TrimPrefix(trimmed, ">")), stMdQuote)
			prefix := []mdCell{{'▎', stMdBullet}, {' ', stMdQuote}}
			out = append(out, renderPrefixed(prefix, body, width)...)
		default:
			if marker, rest := listItem(trimmed); marker != "" {
				prefix := cellsOf(marker, stMdBullet)
				out = append(out, renderPrefixed(prefix, inline(rest, stText), width)...)
			} else {
				out = append(out, wrapCells(inline(line, stText), width)...)
			}
		}
	}
	return out
}

// inline parses inline markup into styled cells: `code`, **bold**, *italic*
// / _italic_, and [text](url) (the url is dropped). Unbalanced markers render
// literally — viz is forgiving, never an error.
func inline(s string, base tui.Style) []mdCell {
	rs := []rune(s)
	var out []mdCell
	bold, italic := false, false
	emit := func(r rune) {
		st := base
		if bold {
			st = st.Bolded()
		}
		if italic {
			st = st.Italicized()
		}
		out = append(out, mdCell{r, st})
	}
	for i := 0; i < len(rs); {
		switch {
		case rs[i] == '`':
			if j := indexRune(rs, '`', i+1); j > 0 {
				for k := i + 1; k < j; k++ {
					out = append(out, mdCell{rs[k], stMdCode})
				}
				i = j + 1
				continue
			}
		case rs[i] == '*' && i+1 < len(rs) && rs[i+1] == '*':
			bold = !bold
			i += 2
			continue
		case rs[i] == '*' || rs[i] == '_':
			italic = !italic
			i++
			continue
		case rs[i] == '[':
			if close := indexRune(rs, ']', i+1); close > 0 && close+1 < len(rs) && rs[close+1] == '(' {
				if end := indexRune(rs, ')', close+2); end > 0 {
					for k := i + 1; k < close; k++ {
						out = append(out, mdCell{rs[k], stMdLink})
					}
					i = end + 1
					continue
				}
			}
		}
		emit(rs[i])
		i++
	}
	return out
}

// wrapCells word-wraps styled cells to width: it splits on spaces (runs of
// whitespace collapse to one, as markdown prose does) and greedily packs
// words, keeping each word's per-rune styles. A word longer than width sits
// alone and is clipped by the drawer.
func wrapCells(cells []mdCell, width int) [][]mdCell {
	if width < 1 {
		width = 1
	}
	var words [][]mdCell
	var w []mdCell
	for _, c := range cells {
		if c.r == ' ' {
			if len(w) > 0 {
				words = append(words, w)
				w = nil
			}
			continue
		}
		w = append(w, c)
	}
	if len(w) > 0 {
		words = append(words, w)
	}
	if len(words) == 0 {
		return [][]mdCell{nil} // a blank line
	}

	space := mdCell{' ', tui.DefaultStyle}
	var out [][]mdCell
	var line []mdCell
	lineW := 0
	for _, word := range words {
		ww := cellsWidth(word)
		switch {
		case len(line) == 0:
			line = append([]mdCell(nil), word...)
			lineW = ww
		case lineW+1+ww <= width:
			line = append(line, space)
			line = append(line, word...)
			lineW += 1 + ww
		default:
			out = append(out, line)
			line = append([]mdCell(nil), word...)
			lineW = ww
		}
	}
	return append(out, line)
}

// renderPrefixed wraps body into the width left of prefix, putting prefix on
// the first line and an equal indent on continuations (hanging indent).
func renderPrefixed(prefix, body []mdCell, width int) [][]mdCell {
	pw := cellsWidth(prefix)
	wrapped := wrapCells(body, max(width-pw, 1))
	out := make([][]mdCell, 0, len(wrapped))
	for i, wl := range wrapped {
		var line []mdCell
		if i == 0 {
			line = append(line, prefix...)
		} else {
			line = append(line, indentCells(pw)...)
		}
		out = append(out, append(line, wl...))
	}
	return out
}

func drawMdLine(buf *tui.Buffer, x0, y, width int, cells []mdCell) {
	x := x0
	for _, c := range cells {
		if x >= x0+width {
			break
		}
		x += buf.Set(x, y, c.r, c.st)
	}
}

// --- small helpers ---

func headingLevel(trimmed string) int {
	n := 0
	for n < len(trimmed) && trimmed[n] == '#' {
		n++
	}
	if n >= 1 && n <= 6 && n < len(trimmed) && trimmed[n] == ' ' {
		return n
	}
	return 0
}

func isRule(trimmed string) bool {
	s := strings.ReplaceAll(trimmed, " ", "")
	if len(s) < 3 {
		return false
	}
	for _, ch := range []byte{'-', '*', '_'} {
		if strings.Trim(s, string(ch)) == "" {
			return true
		}
	}
	return false
}

// listItem returns a render marker and the remaining text for a bullet or
// ordered item, or "" if the line is not a list item.
func listItem(trimmed string) (marker, rest string) {
	for _, b := range []string{"- ", "* ", "+ "} {
		if strings.HasPrefix(trimmed, b) {
			return "• ", trimmed[len(b):]
		}
	}
	i := 0
	for i < len(trimmed) && trimmed[i] >= '0' && trimmed[i] <= '9' {
		i++
	}
	if i > 0 && i+1 < len(trimmed) && trimmed[i] == '.' && trimmed[i+1] == ' ' {
		return trimmed[:i+2], trimmed[i+2:]
	}
	return "", ""
}

func mdRule(width int) []mdCell {
	out := make([]mdCell, width)
	for i := range out {
		out[i] = mdCell{'─', stMdRule}
	}
	return out
}

func codeLine(line string, width int) []mdCell {
	line = strings.ReplaceAll(line, "\t", strings.Repeat(" ", tabWidth))
	return cellsOf(runewidth.Truncate(line, width, ""), stMdCode)
}

func cellsOf(s string, st tui.Style) []mdCell {
	rs := []rune(s)
	out := make([]mdCell, len(rs))
	for i, r := range rs {
		out[i] = mdCell{r, st}
	}
	return out
}

func indentCells(n int) []mdCell {
	out := make([]mdCell, n)
	for i := range out {
		out[i] = mdCell{' ', tui.DefaultStyle}
	}
	return out
}

func cellWidth(r rune) int {
	if w := runewidth.RuneWidth(r); w > 0 {
		return w
	}
	return 1
}

func cellsWidth(cells []mdCell) int {
	w := 0
	for _, c := range cells {
		w += cellWidth(c.r)
	}
	return w
}

func indexRune(rs []rune, target rune, from int) int {
	for i := from; i < len(rs); i++ {
		if rs[i] == target {
			return i
		}
	}
	return -1
}
