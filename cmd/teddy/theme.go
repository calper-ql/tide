package main

import "github.com/calper-ql/tide/internal/tui"

// teddy's palette, like tide's, is built strictly from the terminal's own
// 16-color palette and default fg/bg: one cyan accent carries all
// signaling, dim faint marks structure, a reverse strip is the status bar.
// teddy inherits the user's theme and adapts to light/dark for free.
var (
	stText   = tui.DefaultStyle
	stDim    = tui.DefaultStyle.Fainted()
	stHint   = tui.DefaultStyle.Fainted().Italicized()
	stBorder = tui.DefaultStyle.Fainted()

	stAccent     = tui.DefaultStyle.WithFG(tui.Cyan).Bolded()
	stAccentPill = tui.DefaultStyle.WithFG(tui.Cyan).Reversed().Bolded()
	stHover      = tui.DefaultStyle.WithFG(tui.BrightCyan).Bolded()

	stStatus    = tui.DefaultStyle.Reversed()
	stStatusDim = tui.DefaultStyle.Reversed().Fainted()

	// popup menu surface (bright-black bg, like tide's menus)
	stMenu     = tui.DefaultStyle.WithBG(tui.BrightBlack).WithFG(tui.BrightWhite)
	stMenuHint = tui.DefaultStyle.WithBG(tui.BrightBlack).WithFG(tui.Cyan)
	stMenuDim  = tui.DefaultStyle.WithBG(tui.BrightBlack).Fainted()

	stSideTitle = tui.DefaultStyle.Fainted().Bolded()

	stTab       = tui.DefaultStyle.Fainted()
	stTabActive = tui.DefaultStyle.WithFG(tui.Cyan).Bolded()
	stDirty     = tui.DefaultStyle.WithFG(tui.Yellow)

	stGutter       = tui.DefaultStyle.Fainted()
	stGutterActive = tui.DefaultStyle.WithFG(tui.Cyan)

	stSelected = tui.DefaultStyle.Reversed() // selected browser row (full-width bar)
	stDir      = tui.DefaultStyle.WithFG(tui.BrightBlue)

	// syntax highlighting: chroma categories mapped onto the 16-color palette
	stHlKeyword = tui.DefaultStyle.WithFG(tui.Magenta)
	stHlString  = tui.DefaultStyle.WithFG(tui.Green)
	stHlNumber  = tui.DefaultStyle.WithFG(tui.Red)
	stHlComment = tui.DefaultStyle.Fainted()
	stHlType    = tui.DefaultStyle.WithFG(tui.Yellow)
	stHlBuiltin = tui.DefaultStyle.WithFG(tui.Cyan)
	stHlError   = tui.DefaultStyle.WithFG(tui.BrightRed)

	// markdown viz
	stMdHeading = tui.DefaultStyle.WithFG(tui.Cyan).Bolded()
	stMdCode    = tui.DefaultStyle.WithFG(tui.Green)
	stMdLink    = tui.DefaultStyle.WithFG(tui.Blue).Underlined()
	stMdQuote   = tui.DefaultStyle.Fainted().Italicized()
	stMdRule    = tui.DefaultStyle.Fainted()
	stMdBullet  = tui.DefaultStyle.WithFG(tui.Cyan)
)
