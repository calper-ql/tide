// Package tui is teddy's terminal toolkit: a cell grid, a double-buffered
// diff renderer, and terminal setup + event delivery. It is deliberately
// small and immediate-mode — the app clears the back buffer and redraws
// every frame; the renderer emits only the rows that actually changed.
//
// Like tide's compositor, tui paints strictly with the terminal's own
// 16-color palette and default fg/bg, never truecolor: teddy inherits the
// user's theme and adapts to light/dark for free. Styles map straight onto
// the same SGR conventions the vt snapshot renderer uses.
package tui

import "bytes"

// Color is a slot in the terminal's 16-color palette, or the terminal
// default. The zero value is ColorDefault, so the zero Style means "default
// fg, default bg, no attributes" — a safe blank cell.
type Color int16

const (
	ColorDefault Color = iota // terminal default fg/bg
	Black                     // the 8 normal ANSI colors
	Red
	Green
	Yellow
	Blue
	Magenta
	Cyan
	White
	BrightBlack // the 8 bright ANSI colors
	BrightRed
	BrightGreen
	BrightYellow
	BrightBlue
	BrightMagenta
	BrightCyan
	BrightWhite
)

// Style is a cell's full display attributes. It is comparable, so the
// renderer diffs styles with ==.
type Style struct {
	FG, BG    Color
	Bold      bool
	Faint     bool
	Italic    bool
	Underline bool
	Reverse   bool
}

// DefaultStyle is default fg/bg with no attributes (the zero value, named
// for readability).
var DefaultStyle = Style{}

// With* return a copy of the style with one field changed, for terse
// chaining: tui.DefaultStyle.WithFG(tui.Cyan).Bolded().
func (s Style) WithFG(c Color) Style { s.FG = c; return s }
func (s Style) WithBG(c Color) Style { s.BG = c; return s }
func (s Style) Bolded() Style        { s.Bold = true; return s }
func (s Style) Fainted() Style       { s.Faint = true; return s }
func (s Style) Italicized() Style    { s.Italic = true; return s }
func (s Style) Underlined() Style    { s.Underline = true; return s }
func (s Style) Reversed() Style      { s.Reverse = true; return s }

// writeSGR emits the SGR sequence that selects this style, always starting
// from a reset so the sequence is self-contained (matches vt's appendSGR).
func (s Style) writeSGR(b *bytes.Buffer) {
	b.WriteString("\x1b[0")
	if s.Reverse {
		b.WriteString(";7")
	}
	if s.Bold {
		b.WriteString(";1")
	}
	if s.Faint {
		b.WriteString(";2")
	}
	if s.Italic {
		b.WriteString(";3")
	}
	if s.Underline {
		b.WriteString(";4")
	}
	writeColor(b, s.FG, true)
	writeColor(b, s.BG, false)
	b.WriteByte('m')
}

// writeColor appends one color parameter. ColorDefault is omitted: the
// leading SGR 0 already selected the terminal defaults.
func writeColor(b *bytes.Buffer, c Color, fg bool) {
	if c == ColorDefault {
		return
	}
	idx := int(c - Black) // 0..15
	switch {
	case idx < 8:
		base := 30
		if !fg {
			base = 40
		}
		writeInt(b, base+idx)
	default:
		base := 90
		if !fg {
			base = 100
		}
		writeInt(b, base+idx-8)
	}
}

func writeInt(b *bytes.Buffer, n int) {
	b.WriteByte(';')
	if n >= 100 {
		b.WriteByte(byte('0' + n/100))
		b.WriteByte(byte('0' + (n/10)%10))
		b.WriteByte(byte('0' + n%10))
		return
	}
	b.WriteByte(byte('0' + n/10))
	b.WriteByte(byte('0' + n%10))
}
