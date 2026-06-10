package input

import (
	"bytes"
	"strconv"
	"strings"
	"unicode/utf8"
)

// caps. a legitimate input escape sequence is tens of bytes; maxSeq only
// bounds the partial-sequence buffer against a malformed client (the
// overlong prefix is surfaced as one EvUnknown rather than buffered
// forever). maxPaste is the spec'd 8 MiB: beyond it the remainder of a
// bracketed paste is silently dropped, but the event still ends at the
// 201~ terminator.
const (
	maxSeq   = 64 << 10
	maxPaste = 8 << 20
)

var (
	pasteOpen  = []byte("\x1b[200~")
	pasteClose = []byte("\x1b[201~")
)

// outcomes of one parse attempt at the head of the buffer.
const (
	stEvent      = iota // one event decoded
	stSkip              // bytes consumed, no event (kitty repeat/release)
	stMore              // incomplete sequence; needs more bytes
	stPasteStart        // CSI 200~ consumed; collect a bracketed paste
)

// Decoder is an incremental input parser; Feed may receive sequences
// split at arbitrary boundaries (socket framing) and buffers partial
// sequences across calls. Not safe for concurrent use.
type Decoder struct {
	buf []byte // unconsumed bytes; the head may be a partial sequence

	inPaste bool   // between CSI 200~ and CSI 201~
	paste   []byte // paste payload collected so far, capped at maxPaste
}

func NewDecoder() *Decoder { return &Decoder{} }

// Feed appends p to the stream and returns every event that is now
// complete. Returned Raw/Paste slices are copies and never alias p.
func (d *Decoder) Feed(p []byte) []Event {
	d.buf = append(d.buf, p...)
	return d.drain(false)
}

// Flush forces out a buffered ambiguous prefix: a lone ESC that was
// waiting to see whether it starts a sequence becomes KeyEscape, and an
// unfinished sequence introducer (ESC [, ESC O, ESC ]) becomes the Alt+key
// that actually produces those bytes — at idle no completing bytes are
// coming, so that is the only reading left. The router calls Flush on an
// idle timer. An open bracketed paste is left alone; only its terminator
// ends it.
func (d *Decoder) Flush() []Event { return d.drain(true) }

// Pending reports whether bytes are buffered waiting for more input: a
// partial sequence, a lone ESC, or an open bracketed paste.
func (d *Decoder) Pending() bool { return len(d.buf) > 0 || d.inPaste }

func (d *Decoder) drain(force bool) []Event {
	var evs []Event
	for {
		if d.inPaste {
			if !d.consumePaste() {
				break
			}
			evs = append(evs, d.finishPaste())
			continue
		}
		if len(d.buf) == 0 {
			break
		}
		ev, n, st := parseOne(d.buf)
		if st == stMore {
			if len(d.buf) > maxSeq {
				ev, n, st = Event{Type: EvUnknown}, len(d.buf), stEvent
			} else if force {
				ev, n, st = forceHead(d.buf)
			} else {
				break
			}
		}
		raw := make([]byte, n)
		copy(raw, d.buf[:n])
		d.buf = d.buf[:copy(d.buf, d.buf[n:])]
		switch st {
		case stEvent:
			ev.Raw = raw
			evs = append(evs, ev)
		case stSkip:
		case stPasteStart:
			d.inPaste = true
			d.paste = nil
		}
	}
	if len(d.buf) == 0 && cap(d.buf) > 4096 {
		d.buf = nil // do not pin a large buffer after a burst
	}
	return evs
}

// forceHead resolves an incomplete head that more bytes would normally
// disambiguate; called only from Flush, when the router's idle timer says
// no more bytes are coming. Always consumes at least one byte.
func forceHead(p []byte) (Event, int, int) {
	if p[0] != 0x1b {
		// an incomplete utf-8 rune that will never complete
		r, sz := utf8.DecodeRune(p)
		return kev(KeyRune, r, 0), sz, stEvent
	}
	if len(p) == 1 {
		return kev(KeyEscape, 0, 0), 1, stEvent
	}
	switch p[1] {
	case '[', 'O', ']', 'P', 'X', '^', '_':
		if len(p) == 2 {
			// a bare introducer at idle can only have been the user typing
			// that character with alt held
			return kev(KeyRune, rune(p[1]), Alt), 2, stEvent
		}
		// Introducer plus body bytes: an unambiguous partial sequence whose
		// tail never arrived. Literalizing it would type mouse-report
		// fragments ("0;34;40M") into the pane; drop it whole instead.
		return Event{Type: EvUnknown}, len(p), stEvent
	}
	// ESC + incomplete utf-8 rune
	r, sz := utf8.DecodeRune(p[1:])
	return kev(KeyRune, r, Alt), 1 + sz, stEvent
}

// consumePaste moves buffered bytes into the paste payload, watching for
// the CSI 201~ terminator, which may itself arrive split across feeds.
// Reports whether the paste is complete.
func (d *Decoder) consumePaste() bool {
	pos, done := 0, false
	for pos < len(d.buf) {
		i := bytes.IndexByte(d.buf[pos:], 0x1b)
		if i < 0 {
			d.appendPaste(d.buf[pos:])
			pos = len(d.buf)
			break
		}
		d.appendPaste(d.buf[pos : pos+i])
		pos += i
		rest := d.buf[pos:]
		n := matchLen(rest, pasteClose)
		if n == len(pasteClose) {
			pos += n
			done = true
			break
		}
		if n == len(rest) {
			break // partial terminator at the end of input: hold it
		}
		d.appendPaste(rest[:1]) // a lone ESC inside the payload
		pos++
	}
	d.buf = d.buf[:copy(d.buf, d.buf[pos:])]
	return done
}

func (d *Decoder) appendPaste(p []byte) {
	room := maxPaste - len(d.paste)
	if room <= 0 {
		return
	}
	if len(p) > room {
		p = p[:room]
	}
	d.paste = append(d.paste, p...)
}

func (d *Decoder) finishPaste() Event {
	raw := make([]byte, 0, len(pasteOpen)+len(d.paste)+len(pasteClose))
	raw = append(raw, pasteOpen...)
	raw = append(raw, d.paste...)
	raw = append(raw, pasteClose...)
	d.inPaste = false
	d.paste = nil
	return Event{
		Type:  EvPaste,
		Paste: raw[len(pasteOpen) : len(raw)-len(pasteClose)],
		Raw:   raw,
	}
}

// matchLen reports how many leading bytes of p match pat.
func matchLen(p, pat []byte) int {
	n := 0
	for n < len(p) && n < len(pat) && p[n] == pat[n] {
		n++
	}
	return n
}

func kev(k Key, r rune, m Mod) Event {
	return Event{Type: EvKey, Key: k, Rune: r, Mods: m}
}

// parseOne attempts to decode one event from the head of p, returning the
// event, the bytes consumed, and a status. consumed is meaningful only
// when the status is not stMore.
func parseOne(p []byte) (Event, int, int) {
	if p[0] != 0x1b {
		return parseGround(p, 0)
	}
	if len(p) == 1 {
		return Event{}, 0, stMore
	}
	switch p[1] {
	case '[':
		return parseCSI(p)
	case 'O':
		return parseSS3(p)
	case ']', 'P', 'X', '^', '_':
		return parseStringSeq(p)
	case 0x1b:
		return kev(KeyEscape, 0, Alt), 2, stEvent
	default:
		// alt-modified ordinary byte: a control code or a rune
		ev, n, st := parseGround(p[1:], Alt)
		if st == stMore {
			return Event{}, 0, stMore
		}
		return ev, n + 1, st
	}
}

// parseGround decodes one non-escape token: a C0 control byte or a UTF-8
// rune (possibly still incomplete). extra carries Alt when the byte
// arrived ESC-prefixed; the caller guarantees p[0] != ESC.
func parseGround(p []byte, extra Mod) (Event, int, int) {
	switch b := p[0]; {
	case b == 0x00:
		return kev(KeySpace, 0, Ctrl|extra), 1, stEvent
	case b == 0x09:
		return kev(KeyTab, 0, extra), 1, stEvent
	case b == 0x0d:
		return kev(KeyEnter, 0, extra), 1, stEvent
	case b == 0x7f:
		return kev(KeyBackspace, 0, extra), 1, stEvent
	case b < 0x20:
		return kev(KeyRune, ctrlRune(b), Ctrl|extra), 1, stEvent
	default:
		if !utf8.FullRune(p) {
			return Event{}, 0, stMore
		}
		r, sz := utf8.DecodeRune(p)
		return kev(KeyRune, r, extra), sz, stEvent
	}
}

// ctrlRune maps a C0 byte to the unmodified key that produces it with
// ctrl held. 0x00/0x09/0x0d/0x1b/0x7f are special-cased by the caller.
func ctrlRune(b byte) rune {
	switch {
	case b >= 0x01 && b <= 0x1a:
		return rune('a' + b - 1)
	case b == 0x1c:
		return '\\'
	case b == 0x1d:
		return ']'
	case b == 0x1e:
		return '^'
	case b == 0x1f:
		return '_'
	}
	return utf8.RuneError // unreachable: caller routes everything else
}

var ss3Keys = map[byte]Key{
	'A': KeyUp, 'B': KeyDown, 'C': KeyRight, 'D': KeyLeft,
	'H': KeyHome, 'F': KeyEnd,
	'P': KeyF1, 'Q': KeyF2, 'R': KeyF3, 'S': KeyF4,
}

func parseSS3(p []byte) (Event, int, int) {
	if len(p) < 3 {
		return Event{}, 0, stMore
	}
	f := p[2]
	if k, ok := ss3Keys[f]; ok {
		return kev(k, 0, 0), 3, stEvent
	}
	if f >= 0x40 && f <= 0x7e {
		// well-formed SS3 we do not recognize: the router decides
		return Event{Type: EvUnknown}, 3, stEvent
	}
	// not a sequence final at all: this was alt+'O' followed by something
	return kev(KeyRune, 'O', Alt), 2, stEvent
}

// parseStringSeq consumes an OSC/DCS/SOS/PM/APC string, terminated by ST
// (ESC \) or, for OSC, BEL. These reach the input side only as replies to
// queries (OSC 52 reads, XTGETTCAP); the router gets them whole as one
// EvUnknown — never split or corrupted.
func parseStringSeq(p []byte) (Event, int, int) {
	osc := p[1] == ']'
	for i := 2; i < len(p); i++ {
		switch c := p[i]; {
		case c == 0x07 && osc:
			return Event{Type: EvUnknown}, i + 1, stEvent
		case c == 0x1b:
			if i+1 >= len(p) {
				return Event{}, 0, stMore // ST may be split here
			}
			if p[i+1] == '\\' {
				return Event{Type: EvUnknown}, i + 2, stEvent
			}
			// a bare ESC aborts the string; reprocess it from ground
			return Event{Type: EvUnknown}, i, stEvent
		}
	}
	return Event{}, 0, stMore
}

var csiLetterKeys = map[byte]Key{
	'A': KeyUp, 'B': KeyDown, 'C': KeyRight, 'D': KeyLeft,
	'H': KeyHome, 'F': KeyEnd,
	'P': KeyF1, 'Q': KeyF2, 'R': KeyF3, 'S': KeyF4,
}

var tildeKeys = map[int]Key{
	1: KeyHome, 2: KeyInsert, 3: KeyDelete, 4: KeyEnd,
	5: KeyPageUp, 6: KeyPageDown,
	11: KeyF1, 12: KeyF2, 13: KeyF3, 14: KeyF4,
	15: KeyF5, 17: KeyF6, 18: KeyF7, 19: KeyF8,
	20: KeyF9, 21: KeyF10, 23: KeyF11, 24: KeyF12,
}

func parseCSI(p []byte) (Event, int, int) {
	i := 2
	for i < len(p) {
		c := p[i]
		if c >= 0x40 && c <= 0x7e {
			break // final byte
		}
		if c < 0x20 || c > 0x3f {
			// malformed: surface the prefix, reprocess the stray byte (it
			// may be a control the user typed mid-burst)
			return Event{Type: EvUnknown}, i, stEvent
		}
		i++
	}
	if i == len(p) {
		return Event{}, 0, stMore
	}
	final := p[i]
	body := string(p[2:i])
	n := i + 1

	if len(body) > 0 && body[0] == '<' && (final == 'M' || final == 'm') {
		return parseSGRMouse(body[1:], final == 'm', n)
	}
	if final == 'M' && body == "" {
		return parseX10Mouse(p, n)
	}

	switch final {
	case 'A', 'B', 'C', 'D', 'H', 'F', 'P', 'Q', 'R', 'S':
		mods, skip, ok := parseLetterMods(body)
		if !ok {
			break
		}
		if skip {
			return Event{}, n, stSkip
		}
		return kev(csiLetterKeys[final], 0, mods), n, stEvent
	case 'Z': // backtab
		mods, skip, ok := parseLetterMods(body)
		if !ok {
			break
		}
		if skip {
			return Event{}, n, stSkip
		}
		return kev(KeyTab, 0, mods|Shift), n, stEvent
	case '~':
		return parseTilde(body, n)
	case 'u':
		return parseKittyU(body, n)
	case 'I':
		if body == "" {
			return Event{Type: EvFocus, Gained: true}, n, stEvent
		}
	case 'O':
		if body == "" {
			return Event{Type: EvFocus, Gained: false}, n, stEvent
		}
	}
	return Event{Type: EvUnknown}, n, stEvent
}

// parseLetterMods validates the parameter body of a CSI letter-final key
// (arrows, Home/End, F1-F4, Z): empty, "1", or "1;mods[:event]".
func parseLetterMods(body string) (mods Mod, skip, ok bool) {
	if body == "" {
		return 0, false, true
	}
	fields := strings.Split(body, ";")
	if (fields[0] != "" && fields[0] != "1") || len(fields) > 2 {
		return 0, false, false
	}
	if len(fields) == 1 {
		return 0, false, true
	}
	return parseModField(fields[1])
}

// parseModField parses an xterm modifier parameter with an optional kitty
// event-type subparameter ("5", "5:1", "5:3"). skip reports a repeat or
// release event, which produce no tide event.
func parseModField(s string) (mods Mod, skip, ok bool) {
	sub := strings.SplitN(s, ":", 3)
	m, ok := atoiDef(sub[0], 1)
	if !ok {
		return 0, false, false
	}
	if m < 1 {
		m = 1
	}
	mods = Mod(m-1) & (Shift | Alt | Ctrl)
	if len(sub) >= 2 && sub[1] != "" {
		et, ok := atoiDef(sub[1], 1)
		if !ok {
			return 0, false, false
		}
		if et > 1 {
			return mods, true, true
		}
	}
	return mods, false, true
}

func parseTilde(body string, n int) (Event, int, int) {
	if body == "200" {
		return Event{}, n, stPasteStart
	}
	unknown := Event{Type: EvUnknown}
	fields := strings.Split(body, ";")
	if fields[0] == "27" && len(fields) == 3 {
		// xterm modifyOtherKeys: CSI 27;mods;codepoint~ — semantically the
		// kitty CSI-u form with the fields reversed
		cp, ok := atoiDef(fields[2], -1)
		if !ok || cp < 0 {
			return unknown, n, stEvent
		}
		mods, skip, ok := parseModField(fields[1])
		if !ok {
			return unknown, n, stEvent
		}
		if skip {
			return Event{}, n, stSkip
		}
		return codepointKey(cp, mods, n)
	}
	num, ok := atoiDef(fields[0], -1)
	if !ok || num < 0 || len(fields) > 2 {
		return unknown, n, stEvent
	}
	var mods Mod
	if len(fields) == 2 {
		m, skip, ok := parseModField(fields[1])
		if !ok {
			return unknown, n, stEvent
		}
		if skip {
			return Event{}, n, stSkip
		}
		mods = m
	}
	k, found := tildeKeys[num]
	if !found {
		return unknown, n, stEvent // includes a stray 201~
	}
	return kev(k, 0, mods), n, stEvent
}

// parseKittyU decodes the kitty CSI-u form: CSI unicode[:alts];mods[:event] u.
// Repeat and release events are consumed silently.
func parseKittyU(body string, n int) (Event, int, int) {
	unknown := Event{Type: EvUnknown}
	fields := strings.Split(body, ";")
	cp, ok := atoiDef(strings.SplitN(fields[0], ":", 2)[0], -1)
	if !ok || cp < 0 {
		return unknown, n, stEvent
	}
	var mods Mod
	if len(fields) >= 2 {
		m, skip, ok := parseModField(fields[1])
		if !ok {
			return unknown, n, stEvent
		}
		if skip {
			return Event{}, n, stSkip
		}
		mods = m
	}
	return codepointKey(cp, mods, n)
}

// codepointKey maps a key expressed as a unicode codepoint plus modifiers
// to an event: the shared tail of the kitty CSI-u and xterm
// modifyOtherKeys forms. Kitty functional code points (numpad, media keys
// — the U+E000 private-use block) surface as EvUnknown for the router to
// judge.
func codepointKey(cp int, mods Mod, n int) (Event, int, int) {
	switch cp {
	case 13:
		return kev(KeyEnter, 0, mods), n, stEvent
	case 9:
		return kev(KeyTab, 0, mods), n, stEvent
	case 27:
		return kev(KeyEscape, 0, mods), n, stEvent
	case 127:
		return kev(KeyBackspace, 0, mods), n, stEvent
	case 32:
		if mods == 0 {
			return kev(KeyRune, ' ', 0), n, stEvent
		}
		return kev(KeySpace, 0, mods), n, stEvent
	}
	if cp < 32 || (cp >= 57344 && cp <= 63743) || !utf8.ValidRune(rune(cp)) {
		return Event{Type: EvUnknown}, n, stEvent
	}
	return kev(KeyRune, rune(cp), mods), n, stEvent
}

// parseSGRMouse decodes CSI < b;x;y M|m. body is the parameter text after
// the '<'; release reports a final of 'm'.
func parseSGRMouse(body string, release bool, n int) (Event, int, int) {
	unknown := Event{Type: EvUnknown}
	fields := strings.Split(body, ";")
	if len(fields) != 3 {
		return unknown, n, stEvent
	}
	b, ok1 := atoiDef(fields[0], -1)
	x, ok2 := atoiDef(fields[1], -1)
	y, ok3 := atoiDef(fields[2], -1)
	if !ok1 || !ok2 || !ok3 || b < 0 || b >= 128 || x < 1 || y < 1 {
		return unknown, n, stEvent // includes extended buttons 8-11
	}
	ev := Event{Type: EvMouse, X: x - 1, Y: y - 1}
	if b&4 != 0 {
		ev.Mods |= Shift
	}
	if b&8 != 0 {
		ev.Mods |= Alt
	}
	if b&16 != 0 {
		ev.Mods |= Ctrl
	}
	btn := b & 3
	switch {
	case b&64 != 0:
		switch btn {
		case 0:
			ev.Mouse = MouseWheelUp
		case 1:
			ev.Mouse = MouseWheelDown
		default:
			return unknown, n, stEvent // wheel left/right: not modeled
		}
	case b&32 != 0:
		ev.Mouse = MouseMotion
		if btn < 3 {
			ev.Button = btn + 1
		}
	case release:
		ev.Mouse = MouseRelease
		if btn < 3 {
			ev.Button = btn + 1
		}
	case btn == 3:
		return unknown, n, stEvent // x10 release marker is not valid SGR press
	default:
		ev.Mouse = MousePress
		ev.Button = btn + 1
	}
	return ev, n, stEvent
}

// parseX10Mouse decodes the legacy X10 mouse report CSI M Cb Cx Cy: three
// raw payload bytes after the final, each offset by 32. Terminals without
// SGR (1006) support fall back to this encoding even when it was
// requested, so the payload must be consumed or it leaks as typed runes.
// n is the length of the CSI M prefix; the payload may arrive split
// across feeds.
func parseX10Mouse(p []byte, n int) (Event, int, int) {
	if len(p) < n+3 {
		return Event{}, 0, stMore
	}
	b := int(p[n]) - 32
	// coordinates are 1-based after the 32 offset; a terminal that wraps
	// past byte 255 can put the arithmetic below zero — clamp at 0
	x := max(int(p[n+1])-32-1, 0)
	y := max(int(p[n+2])-32-1, 0)
	n += 3
	ev := Event{Type: EvMouse, X: x, Y: y}
	if b&4 != 0 {
		ev.Mods |= Shift
	}
	if b&8 != 0 {
		ev.Mods |= Alt
	}
	if b&16 != 0 {
		ev.Mods |= Ctrl
	}
	btn := b & 3
	switch {
	case b&64 != 0:
		switch btn {
		case 0:
			ev.Mouse = MouseWheelUp
		case 1:
			ev.Mouse = MouseWheelDown
		default:
			return Event{Type: EvUnknown}, n, stEvent // wheel left/right: not modeled
		}
	case b&32 != 0:
		ev.Mouse = MouseMotion
		if btn < 3 {
			ev.Button = btn + 1
		}
	case btn == 3:
		ev.Mouse = MouseRelease // legacy release cannot name its button
	default:
		ev.Mouse = MousePress
		ev.Button = btn + 1
	}
	return ev, n, stEvent
}

// atoiDef parses a decimal parameter field; an empty field yields def,
// garbage yields ok=false.
func atoiDef(s string, def int) (int, bool) {
	if s == "" {
		return def, true
	}
	v, err := strconv.Atoi(s)
	if err != nil || v < 0 {
		return 0, false
	}
	return v, true
}
