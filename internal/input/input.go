// Package input is the daemon's terminal input layer: an incremental
// decoder that turns the raw byte stream from an attached client into
// events, and re-encoders that render routed events as the bytes a pane
// expects given its own terminal modes. Client and pane encodings never
// match by assumption — the client terminal may have kitty keys pushed
// while the pane app speaks legacy CSI — so the router decodes once here
// and re-encodes per pane, tmux-style; raw client bytes are never
// forwarded blindly.
package input

// Mod is a bitfield of key/mouse modifiers. The bit values equal the
// xterm modifier parameter minus one, so mods-param conversion is the
// identity in both directions.
type Mod uint8

const (
	Shift Mod = 1 << iota
	Alt
	Ctrl
)

// Key identifies a decoded key. KeyRune carries the rune in Event.Rune;
// everything else is a named key.
type Key int

const (
	KeyRune Key = iota // printable rune in Event.Rune
	KeyEnter
	KeyTab
	KeyBackspace
	KeyEscape
	KeyUp
	KeyDown
	KeyRight
	KeyLeft
	KeyHome
	KeyEnd
	KeyPageUp
	KeyPageDown
	KeyInsert
	KeyDelete
	KeyF1
	KeyF2
	KeyF3
	KeyF4
	KeyF5
	KeyF6
	KeyF7
	KeyF8
	KeyF9
	KeyF10
	KeyF11
	KeyF12
	KeySpace // space with modifiers; bare space may arrive as KeyRune ' '
)

// EventType selects which Event fields are meaningful.
type EventType int

const (
	EvKey EventType = iota
	EvMouse
	EvPaste
	EvFocus   // Gained reports direction
	EvUnknown // unrecognized escape sequence, Raw carries it verbatim
)

// MouseType is the kind of mouse event.
type MouseType int

const (
	MousePress MouseType = iota
	MouseRelease
	MouseMotion // motion with a button held (1002) or none (1003)
	MouseWheelUp
	MouseWheelDown
)

// Event is one decoded input event.
type Event struct {
	Type EventType

	// EvKey
	Key  Key
	Rune rune // when Key == KeyRune
	Mods Mod  // also set for EvMouse

	// EvMouse (X, Y are 0-based screen cells)
	Mouse  MouseType
	Button int // 1 left, 2 middle, 3 right; 0 none (motion/wheel)
	X, Y   int

	// EvPaste
	Paste []byte

	// EvFocus
	Gained bool

	// always: the exact bytes that produced this event. For EvPaste the
	// payload — and therefore Raw — is capped at the paste size limit.
	Raw []byte
}
