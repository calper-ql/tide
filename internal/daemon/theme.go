// Themes are built strictly from the terminal's own 16-color palette and
// default fg/bg (ratified: no hardcoded RGB — tide inherits the user's
// palette and survives light and dark terminals alike). A theme picks the
// accent slot pair; everything else is shared. Invariants every preset
// must hold:
//   - no style pairs fg and bg from the same palette slot (the class of
//     bug that made 90-on-100 menu rows invisible);
//   - red (31/91) is reserved for dead panes and destructive items, so
//     "exited" can never read as "focused";
//   - chroma never carries meaning alone — bold/reverse/heavy glyphs or a
//     text suffix double-code every signal.
package daemon

import (
	"fmt"
	"strings"
)

// theme maps chrome roles to complete, self-resetting SGR prefixes.
type theme struct {
	name       string // display name; lowercase form is the prefs key
	bar        string // session bar base strip
	accentBar  string // accent "pill": project segment, active tab, scroll tag
	barHover   string // bar button / detach under the pointer (1003 terminals)
	frame      string // pane frames at rest
	focus      string // focused pane frame/bar
	hover      string // hovered boundary: heavy strokes
	dead       string // exited pane bar
	flash      string // transient bar status
	menu       string // popup surface + enabled items
	menuTitle  string // popup title row
	menuDim    string // disabled items and separator rules
	menuHover  string // item under the pointer / pre-selected default
	menuDanger string // destructive items at rest
}

// accentTheme derives a chromatic preset from one accent slot n (normal 3n,
// bright 9n). Bright accents appear only as bold text on the dark popup
// surface or as reverse-video fills — bright STROKES on a light background
// are near-invisible, so frames and boundaries stay on the normal slot.
func accentTheme(name string, n int) theme {
	return theme{
		name:       name,
		bar:        "\x1b[0;7;2m",
		accentBar:  fmt.Sprintf("\x1b[0;7;1;3%dm", n),
		barHover:   fmt.Sprintf("\x1b[0;7;1;9%dm", n),
		frame:      "\x1b[0;2m",
		focus:      fmt.Sprintf("\x1b[0;3%dm", n),
		hover:      fmt.Sprintf("\x1b[0;1;3%dm", n),
		dead:       "\x1b[0;31m",
		flash:      "\x1b[0;7;1m",
		menu:       "\x1b[0;100;97m",
		menuTitle:  fmt.Sprintf("\x1b[0;100;1;9%dm", n),
		menuDim:    "\x1b[0;100;37m",
		menuHover:  fmt.Sprintf("\x1b[0;7;1;3%dm", n),
		menuDanger: "\x1b[0;100;91m",
	}
}

// themes is the curated preset list, in picker order. Tide is the default.
var themes = func() []theme {
	tide := accentTheme("Tide", 6)
	ocean := accentTheme("Ocean", 4)
	// Slot 4 goes too dark on dark backgrounds; the perimeter and the
	// hover strokes (1003-only, so zero baseline risk) take the bright slot.
	ocean.focus = "\x1b[0;94m"
	ocean.hover = "\x1b[0;1;94m"
	moss := accentTheme("Moss", 2)
	plum := accentTheme("Plum", 5)
	ember := accentTheme("Ember", 3)
	// Bold-as-bright yellow fills over-glow on light palettes: the pill and
	// its hover state stay on the normal slot (hover keeps bold — under
	// bold-as-weight it brightens, under bold-as-bright it is the lesser of
	// the yellows).
	ember.accentBar = "\x1b[0;7;33m"
	ember.barHover = "\x1b[0;7;1;33m"
	// Ink is the guaranteed-contrast escape hatch: no chroma, everything
	// derived from the user's own default fg/bg (reverse pairs cannot lose
	// contrast on any palette — including ones where slot 8 ≈ the default
	// bg, which sink the chromatic presets' popup surface).
	ink := theme{
		name:      "Ink",
		bar:       "\x1b[0;7;2m",
		accentBar: "\x1b[0;7;1m",
		// Hover must BRIGHTEN, and 0;7 reads dimmer than the bold pills at
		// rest: Ink's hover language is un-reversed bold — a cut-out pill
		// on the reversed strip, same trick as its menuHover.
		barHover:   "\x1b[0;1m",
		frame:      "\x1b[0m",
		focus:      "\x1b[0;1m",
		hover:      "\x1b[0;1m",
		dead:       "\x1b[0;31m",
		flash:      "\x1b[0;7;1m",
		menu:       "\x1b[0;7m",
		menuTitle:  "\x1b[0;7;1m",
		menuDim:    "\x1b[0;7;2m",
		menuHover:  "\x1b[0;1m", // un-reversed row = an inverted pill on the reversed card
		menuDanger: "\x1b[0;7;1;31m",
	}
	return []theme{tide, ocean, moss, plum, ember, ink}
}()

// themeByName resolves a prefs key (case-insensitive); unknown names fall
// back to the default so a stale prefs file can never brick an attach.
func themeByName(key string) (theme, bool) {
	for _, t := range themes {
		if strings.EqualFold(t.name, key) {
			return t, true
		}
	}
	return themes[0], false
}
