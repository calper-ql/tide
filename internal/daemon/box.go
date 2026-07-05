// Junction glyphs are resolved, never assumed: every box-drawing cell in
// the chrome picks its character from the lines that actually meet there,
// so no junction ever grows an arm poking into a pane that has no line on
// that side, and no line ever dead-ends into a flat stroke. Arms carry a
// weight so the hover layer's heavy strokes join the light frame with the
// proper mixed-weight glyphs instead of flattening tees and corners.
package daemon

// Arm weights, in increasing prominence.
const (
	armNone  = iota // no line reaches this cell from that side
	armLight        // frame / bar stroke
	armHeavy        // hover highlight stroke
)

// boxGlyph returns the box-drawing character whose four arms match
// exactly. Light-only corners use tide's rounded arcs.
func boxGlyph(up, down, left, right int) string {
	return boxGlyphs[up*27+down*9+left*3+right]
}

// boxGlyphs covers every arm/weight combination, generated from the
// Unicode character names in the Box Drawing block (U+2500–U+257F);
// index = up*27 + down*9 + left*3 + right with none=0 light=1 heavy=2.
var boxGlyphs = [81]string{
	0:  " ", // blank
	1:  "╶", // LIGHT RIGHT
	2:  "╺", // HEAVY RIGHT
	3:  "╴", // LIGHT LEFT
	4:  "─", // LIGHT HORIZONTAL
	5:  "╼", // LIGHT LEFT AND HEAVY RIGHT
	6:  "╸", // HEAVY LEFT
	7:  "╾", // HEAVY LEFT AND LIGHT RIGHT
	8:  "━", // HEAVY HORIZONTAL
	9:  "╷", // LIGHT DOWN
	10: "╭", // arc (LIGHT DOWN AND RIGHT)
	11: "┍", // DOWN LIGHT AND RIGHT HEAVY
	12: "╮", // arc (LIGHT DOWN AND LEFT)
	13: "┬", // LIGHT DOWN AND HORIZONTAL
	14: "┮", // RIGHT HEAVY AND LEFT DOWN LIGHT
	15: "┑", // DOWN LIGHT AND LEFT HEAVY
	16: "┭", // LEFT HEAVY AND RIGHT DOWN LIGHT
	17: "┯", // DOWN LIGHT AND HORIZONTAL HEAVY
	18: "╻", // HEAVY DOWN
	19: "┎", // DOWN HEAVY AND RIGHT LIGHT
	20: "┏", // HEAVY DOWN AND RIGHT
	21: "┒", // DOWN HEAVY AND LEFT LIGHT
	22: "┰", // DOWN HEAVY AND HORIZONTAL LIGHT
	23: "┲", // LEFT LIGHT AND RIGHT DOWN HEAVY
	24: "┓", // HEAVY DOWN AND LEFT
	25: "┱", // RIGHT LIGHT AND LEFT DOWN HEAVY
	26: "┳", // HEAVY DOWN AND HORIZONTAL
	27: "╵", // LIGHT UP
	28: "╰", // arc (LIGHT UP AND RIGHT)
	29: "┕", // UP LIGHT AND RIGHT HEAVY
	30: "╯", // arc (LIGHT UP AND LEFT)
	31: "┴", // LIGHT UP AND HORIZONTAL
	32: "┶", // RIGHT HEAVY AND LEFT UP LIGHT
	33: "┙", // UP LIGHT AND LEFT HEAVY
	34: "┵", // LEFT HEAVY AND RIGHT UP LIGHT
	35: "┷", // UP LIGHT AND HORIZONTAL HEAVY
	36: "│", // LIGHT VERTICAL
	37: "├", // LIGHT VERTICAL AND RIGHT
	38: "┝", // VERTICAL LIGHT AND RIGHT HEAVY
	39: "┤", // LIGHT VERTICAL AND LEFT
	40: "┼", // LIGHT VERTICAL AND HORIZONTAL
	41: "┾", // RIGHT HEAVY AND LEFT VERTICAL LIGHT
	42: "┥", // VERTICAL LIGHT AND LEFT HEAVY
	43: "┽", // LEFT HEAVY AND RIGHT VERTICAL LIGHT
	44: "┿", // VERTICAL LIGHT AND HORIZONTAL HEAVY
	45: "╽", // LIGHT UP AND HEAVY DOWN
	46: "┟", // DOWN HEAVY AND RIGHT UP LIGHT
	47: "┢", // UP LIGHT AND RIGHT DOWN HEAVY
	48: "┧", // DOWN HEAVY AND LEFT UP LIGHT
	49: "╁", // DOWN HEAVY AND UP HORIZONTAL LIGHT
	50: "╆", // RIGHT DOWN HEAVY AND LEFT UP LIGHT
	51: "┪", // UP LIGHT AND LEFT DOWN HEAVY
	52: "╅", // LEFT DOWN HEAVY AND RIGHT UP LIGHT
	53: "╈", // UP LIGHT AND DOWN HORIZONTAL HEAVY
	54: "╹", // HEAVY UP
	55: "┖", // UP HEAVY AND RIGHT LIGHT
	56: "┗", // HEAVY UP AND RIGHT
	57: "┚", // UP HEAVY AND LEFT LIGHT
	58: "┸", // UP HEAVY AND HORIZONTAL LIGHT
	59: "┺", // LEFT LIGHT AND RIGHT UP HEAVY
	60: "┛", // HEAVY UP AND LEFT
	61: "┹", // RIGHT LIGHT AND LEFT UP HEAVY
	62: "┻", // HEAVY UP AND HORIZONTAL
	63: "╿", // HEAVY UP AND LIGHT DOWN
	64: "┞", // UP HEAVY AND RIGHT DOWN LIGHT
	65: "┡", // DOWN LIGHT AND RIGHT UP HEAVY
	66: "┦", // UP HEAVY AND LEFT DOWN LIGHT
	67: "╀", // UP HEAVY AND DOWN HORIZONTAL LIGHT
	68: "╄", // RIGHT UP HEAVY AND LEFT DOWN LIGHT
	69: "┩", // DOWN LIGHT AND LEFT UP HEAVY
	70: "╃", // LEFT UP HEAVY AND RIGHT DOWN LIGHT
	71: "╇", // DOWN LIGHT AND UP HORIZONTAL HEAVY
	72: "┃", // HEAVY VERTICAL
	73: "┠", // VERTICAL HEAVY AND RIGHT LIGHT
	74: "┣", // HEAVY VERTICAL AND RIGHT
	75: "┨", // VERTICAL HEAVY AND LEFT LIGHT
	76: "╂", // VERTICAL HEAVY AND HORIZONTAL LIGHT
	77: "╊", // LEFT LIGHT AND RIGHT VERTICAL HEAVY
	78: "┫", // HEAVY VERTICAL AND LEFT
	79: "╉", // RIGHT LIGHT AND LEFT VERTICAL HEAVY
	80: "╋", // HEAVY VERTICAL AND HORIZONTAL
}
