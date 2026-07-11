// Themes are built strictly from the terminal's own 16-color palette and
// default fg/bg (ratified: no hardcoded RGB — tide inherits the user's
// palette and survives light and dark terminals alike). A theme picks the
// accent slot pair and paints EVERY chrome surface with it: the bar strip
// and the popup card carry the accent fill, not just the pills (they used
// to share a reverse-dim strip and a slot-8 gray card across all presets,
// which read as an unthemed white bar and gray menu). Invariants every
// preset must hold:
//   - no style pairs fg and bg from the same palette slot (the class of
//     bug that made 90-on-100 menu rows invisible);
//   - glyphs sitting ON a chromatic fill are the terminal's default bg via
//     reverse video — the pairing the default fg/bg contract keeps legible
//     on light and dark palettes alike — or a slot vetted per preset;
//   - chromatic fills never rely on dim: terminals disagree on whether 7;2
//     dims the fill or the glyphs (the old shared bar rendered as a full
//     white strip on the latter), so fills use plain slots and look the
//     same everywhere. Ink's neutral strips are the one exception — either
//     reading of 7;2 stays legible there;
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
//
// Surfaces: the bar strip and the popup card share the normal-slot fill
// with default-bg glyphs; the pills (project segment, active tab, popup
// title) sit on the bright slot with bold, so they pop under both bold
// conventions and still differ by bold glyphs on palettes where bright
// equals normal. Hover — bar buttons and menu rows alike — is the neutral
// reverse-bold pair: the one fill that out-brightens any accent on any
// palette (reverse pairs cannot lose contrast). Disabled rows keep the
// card fill and take slot-8 glyphs (reverse swaps the explicit bg code in
// as the glyph color); where slot 8 hides against a fill the "— reason"
// suffix still marks them. Danger rows put bright-red glyphs on the card
// fill; presets whose fill fights red override it.
func accentTheme(name string, n int) theme {
	return theme{
		name:       name,
		bar:        fmt.Sprintf("\x1b[0;7;3%dm", n),
		accentBar:  fmt.Sprintf("\x1b[0;7;1;9%dm", n),
		barHover:   "\x1b[0;7;1m",
		frame:      "\x1b[0;2m",
		focus:      fmt.Sprintf("\x1b[0;3%dm", n),
		hover:      fmt.Sprintf("\x1b[0;1;3%dm", n),
		dead:       "\x1b[0;31m",
		flash:      "\x1b[0;7;1m",
		menu:       fmt.Sprintf("\x1b[0;7;3%dm", n),
		menuTitle:  fmt.Sprintf("\x1b[0;7;1;9%dm", n),
		menuDim:    fmt.Sprintf("\x1b[0;7;3%d;100m", n),
		menuHover:  "\x1b[0;7;1m",
		menuDanger: fmt.Sprintf("\x1b[0;7;3%d;101m", n),
	}
}

// themes is the curated preset list, in picker order. Tide is the default.
var themes = func() []theme {
	tide := accentTheme("Tide", 6)
	ocean := accentTheme("Ocean", 4)
	// Slot 4 goes too dark for default-bg glyphs on dark palettes: the
	// perimeter and the hover strokes (1003-only, so zero baseline risk)
	// take the bright slot, and the navy strip and card carry explicit
	// bright-white glyphs instead of default-bg ones — the classic
	// white-on-blue card. Slot 7 plays the muted glyph on that fill; slot 8
	// would sink into it.
	ocean.focus = "\x1b[0;94m"
	ocean.hover = "\x1b[0;1;94m"
	ocean.bar = "\x1b[0;7;34;107m"
	ocean.menu = "\x1b[0;7;34;107m"
	ocean.menuDim = "\x1b[0;7;34;47m"
	moss := accentTheme("Moss", 2)
	plum := accentTheme("Plum", 5)
	// Red glyphs vanish into a magenta fill (adjacent hues): Plum's danger
	// rows become the red fill itself with default-bg glyphs — a reverse
	// pair that cannot lose contrast, and red stays the destruction color.
	plum.menuDanger = "\x1b[0;7;31m"
	ember := accentTheme("Ember", 3)
	// Bold-as-bright yellow fills over-glow on light palettes: Ember's
	// pills stay on the normal slot with bold (under bold-as-weight it
	// thickens, under bold-as-bright it is the lesser of the yellows). The
	// strip takes slot-8 glyphs so the bold default-bg pill still reads
	// against it, and danger glyphs go dark red — bright red vibrates
	// against yellow.
	ember.bar = "\x1b[0;7;33;100m"
	ember.accentBar = "\x1b[0;7;1;33m"
	ember.menuTitle = "\x1b[0;7;1;33m"
	ember.menuDanger = "\x1b[0;7;33;41m"
	// Ink is the guaranteed-contrast escape hatch: no chroma, everything
	// derived from the user's own default fg/bg (reverse pairs cannot lose
	// contrast on any palette — including ones that render an accent slot
	// too close to the default fg or bg for the chromatic fills to read).
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
