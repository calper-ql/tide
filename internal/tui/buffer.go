package tui

import "github.com/mattn/go-runewidth"

// Rect is a screen region in cells. X,Y is the top-left; W,H the size.
type Rect struct{ X, Y, W, H int }

func (r Rect) Contains(x, y int) bool {
	return x >= r.X && x < r.X+r.W && y >= r.Y && y < r.Y+r.H
}

// Cell is one grid cell. R == 0 marks the second column of a wide
// (double-width) rune to the left; the renderer skips it and the lead rune
// covers both columns.
type Cell struct {
	R  rune
	St Style
}

// Buffer is a fixed-size grid of cells, row-major. The app draws into it
// each frame; the screen diffs it against the last frame.
type Buffer struct {
	W, H  int
	cells []Cell
}

func NewBuffer(w, h int) *Buffer {
	b := &Buffer{}
	b.Resize(w, h)
	return b
}

// Resize reallocates to w×h and clears to blank default cells. Content is
// not preserved — callers redraw every frame.
func (b *Buffer) Resize(w, h int) {
	if w < 0 {
		w = 0
	}
	if h < 0 {
		h = 0
	}
	b.W, b.H = w, h
	b.cells = make([]Cell, w*h)
	b.Clear(DefaultStyle)
}

// Clear fills every cell with a space in the given style — the background
// wash for a frame.
func (b *Buffer) Clear(st Style) {
	blank := Cell{R: ' ', St: st}
	for i := range b.cells {
		b.cells[i] = blank
	}
}

func (b *Buffer) at(x, y int) Cell {
	return b.cells[y*b.W+x]
}

// Cell returns the cell at (x,y), or a blank cell if out of bounds.
func (b *Buffer) Cell(x, y int) Cell {
	if x < 0 || x >= b.W || y < 0 || y >= b.H {
		return Cell{R: ' '}
	}
	return b.at(x, y)
}

// SetCell writes a single-column cell, bounds-checked. A zero rune becomes a
// space so callers can't accidentally plant a continuation marker.
func (b *Buffer) SetCell(x, y int, r rune, st Style) {
	if x < 0 || x >= b.W || y < 0 || y >= b.H {
		return
	}
	if r == 0 {
		r = ' '
	}
	b.cells[y*b.W+x] = Cell{R: r, St: st}
}

// Set writes a rune honoring its display width, planting a continuation
// marker for wide runes, and returns the number of columns it consumed (0
// if clipped). A wide rune with no room in the last column degrades to a
// space, so a row's cells always sum to exactly W columns.
func (b *Buffer) Set(x, y int, r rune, st Style) int {
	if x < 0 || x >= b.W || y < 0 || y >= b.H {
		return 0
	}
	w := runewidth.RuneWidth(r)
	if w < 1 {
		w = 1 // control/zero-width: occupy one cell so the grid stays dense
	}
	if w == 2 {
		if x+1 >= b.W {
			b.cells[y*b.W+x] = Cell{R: ' ', St: st} // no room for the pair
			return 1
		}
		b.cells[y*b.W+x] = Cell{R: r, St: st}
		b.cells[y*b.W+x+1] = Cell{R: 0, St: st} // continuation
		return 2
	}
	b.cells[y*b.W+x] = Cell{R: r, St: st}
	return 1
}

// DrawText writes s starting at (x,y), advancing by each rune's width, and
// returns the column just past the last rune drawn. It stops at the row's
// right edge; it does not wrap.
func (b *Buffer) DrawText(x, y int, st Style, s string) int {
	for _, r := range s {
		if x >= b.W {
			break
		}
		x += b.Set(x, y, r, st)
	}
	return x
}

// Fill paints a rectangle with one rune/style, clipped to the buffer.
func (b *Buffer) Fill(rect Rect, r rune, st Style) {
	for y := rect.Y; y < rect.Y+rect.H; y++ {
		for x := rect.X; x < rect.X+rect.W; x++ {
			b.SetCell(x, y, r, st)
		}
	}
}
