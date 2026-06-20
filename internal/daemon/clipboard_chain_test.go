package daemon

// Copy/paste is the load-bearing feature of tide over SSH, so this file
// pins the whole chain rather than any single function: the OSC 52 byte
// contract, the bytes that actually reach the host terminal, what a range
// of real terminal emulators would make of them, the multi-client fan-out,
// and the paste/selection escape hatches. The deployment fact that matters:
// over SSH the client runs on the REMOTE box, so the native clipboard tool
// (TypeCopy → pbcopy/xclip) writes the remote clipboard — only the OSC 52
// escape on the render stream crosses back to the user's local terminal.
// These tests therefore treat the OSC 52 path as the primary contract and
// the native frame as the local-only fallback it is.

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/calper-ql/tide/internal/protocol"
)

// --- shared helpers -----------------------------------------------------

// all returns a snapshot of every byte this client has received — the exact
// stream a host terminal would render, OSC 52 escapes and all.
func (s *sink) all() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.data.Bytes()...)
}

// pressShift is a left-button press carrying the Shift modifier (SGR button
// bit 2 = 4): the escape hatch that makes tide own the mouse even when the
// pane's app has mouse reporting on.
func pressShift(x, y int) []byte { return []byte(fmt.Sprintf("\x1b[<4;%d;%dM", x+1, y+1)) }

// decodedClip is one OSC 52 clipboard write recovered from a render stream.
type decodedClip struct {
	target byte   // 'c' (clipboard) or 'p' (primary)
	text   string // base64-decoded payload
	ok     bool   // payload was well-formed std base64
}

// scanOSC52 extracts every OSC 52 sequence from a byte stream — the parser a
// real terminal emulator runs over what tide writes to stdout. Sequences are
// BEL- or ST-terminated; the payload is std base64 (no raw ESC/BEL), so the
// terminator scan is unambiguous.
func scanOSC52(stream []byte) []decodedClip {
	var out []decodedClip
	intro := []byte("\x1b]52;")
	for i := 0; ; {
		j := bytes.Index(stream[i:], intro)
		if j < 0 {
			return out
		}
		start := i + j + len(intro)
		term := -1
		for k := start; k < len(stream); k++ {
			if stream[k] == 0x07 { // BEL
				term = k
				break
			}
			if stream[k] == 0x1b && k+1 < len(stream) && stream[k+1] == '\\' { // ST
				term = k
				break
			}
		}
		if term < 0 {
			return out // unterminated tail
		}
		body := stream[start:term]
		if semi := bytes.IndexByte(body, ';'); semi == 1 { // single-char target
			dec, err := base64.StdEncoding.DecodeString(string(body[semi+1:]))
			out = append(out, decodedClip{target: body[0], text: string(dec), ok: err == nil})
		}
		i = term + 1
	}
}

// hostTerminal models the OSC 52 capability class of a terminal emulator the
// user might attach with over SSH. supportsOSC52 is whether the emulator
// honors \x1b]52 writes to the system clipboard at all (Terminal.app does
// not; many VTE builds ship it gated). honorsPrimary is whether it also
// accepts target 'p'. These flags model representative classes for the chain
// test — not a definitive per-version compatibility database.
type hostTerminal struct {
	name          string
	supportsOSC52 bool
	honorsPrimary bool
}

// receive returns what this terminal's system clipboard and primary
// selection hold after rendering the stream tide wrote.
func (h hostTerminal) receive(stream []byte) (clipboard, primary string) {
	if !h.supportsOSC52 {
		return "", ""
	}
	for _, c := range scanOSC52(stream) {
		if !c.ok {
			continue
		}
		switch c.target {
		case 'c':
			clipboard = c.text
		case 'p':
			if h.honorsPrimary {
				primary = c.text
			}
		}
	}
	return clipboard, primary
}

var hostTerminals = []hostTerminal{
	{"gnome-terminal (VTE, OSC52 enabled)", true, true},
	{"xterm (clipboard allowed)", true, true},
	{"iTerm2", true, false},
	{"kitty", true, true},
	{"WezTerm", true, true},
	{"Alacritty", true, true},
	{"foot", true, true},
	{"macOS Terminal.app (no OSC52)", false, false},
}

// plantSelection writes an ASCII marker into the focused pane and selects it
// exactly, verifying the selection extracts back to the marker so callers can
// trust a subsequent copy. ASCII keeps cell width == rune count == byte
// count; UTF-8 payloads are exercised by the osc52 unit tests below, which do
// not depend on screen-cell geometry.
func plantSelection(t *testing.T, w *ws, marker string) string {
	t.Helper()
	var paneID string
	withWS(w, func() {
		paneID = w.lay.FocusedPane()
		p := w.panes[paneID]
		p.term.Write([]byte("\r\n" + marker + "\r\n"))
		_, rows, _ := p.term.ContentSize()
		view, hist := p.term.View(0, rows)
		for i, line := range view {
			text := ""
			for _, g := range line {
				if g.Char != 0 {
					text += string(g.Char)
				}
			}
			if strings.Contains(text, marker) {
				w.sel = selectionState{pane: paneID, exists: true, aLine: hist + i, aX: 0, eLine: hist + i, eX: len(marker) - 1}
				break
			}
		}
		if !w.sel.exists {
			t.Fatal("planted marker not found in pane view")
		}
		if got := w.selectionTextLocked(); got != marker {
			t.Fatalf("planted selection text = %q, want %q", got, marker)
		}
	})
	return paneID
}

// attachClient joins a second pipe client to an existing workspace.
func attachClient(t *testing.T, w *ws) (*protocol.Conn, *sink) {
	t.Helper()
	server, clientEnd := net.Pipe()
	sc := protocol.NewConn(server)
	cc := protocol.NewConn(clientEnd)
	s := startSink(cc)
	if _, err := w.attach(sc, 100, 30, func(frame []byte, clients, panes int) protocol.Message {
		return protocol.Message{Type: protocol.TypeRender, Data: frame}
	}); err != nil {
		t.Fatal(err)
	}
	return sc, s
}

// --- OSC 52 byte contract ----------------------------------------------

// TestOSC52EncodingContract pins the exact wire format every emulator parses:
// ESC ] 52 ; <target> ; <std-base64> BEL. A drift here silently breaks copy
// on every terminal at once.
func TestOSC52EncodingContract(t *testing.T) {
	cases := []struct {
		target byte
		text   string
	}{
		{'c', "hello"},
		{'p', "world"},
		{'c', ""},
		{'c', "two\nlines"},
		{'c', "tab\tand\x01ctrl"},
		{'c', "héllo 世界 🚀"},
		{'p', strings.Repeat("A", 4096)},
	}
	const prefix = "\x1b]52;"
	for _, tc := range cases {
		got := osc52(tc.target, tc.text)
		if !bytes.HasPrefix(got, []byte(prefix)) {
			t.Errorf("osc52(%q): missing %q prefix: %q", tc.text, prefix, got)
			continue
		}
		if n := len(got); got[n-1] != 0x07 {
			t.Errorf("osc52(%q): want BEL terminator, got 0x%02x", tc.text, got[n-1])
		}
		if got[len(prefix)] != tc.target {
			t.Errorf("osc52(%q): target = %q, want %q", tc.text, got[len(prefix)], tc.target)
		}
		if got[len(prefix)+1] != ';' {
			t.Errorf("osc52(%q): missing ';' after target", tc.text)
		}
		b64 := string(got[len(prefix)+2 : len(got)-1])
		if want := base64.StdEncoding.EncodeToString([]byte(tc.text)); b64 != want {
			t.Errorf("osc52(%q): payload = %q, want std base64 %q", tc.text, b64, want)
		}
		if dec, err := base64.StdEncoding.DecodeString(b64); err != nil || string(dec) != tc.text {
			t.Errorf("osc52(%q): payload round trip = %q (%v)", tc.text, dec, err)
		}
	}
}

// TestOSC52RoundTripThroughScanner is the property test: any payload tide
// copies survives encode → embed-in-render-stream → terminal-side decode
// byte-for-byte. Covers empty, multibyte UTF-8, embedded control bytes, and
// large payloads (no length cap on tide's own copy path).
func TestOSC52RoundTripThroughScanner(t *testing.T) {
	corpus := []string{
		"",
		"a",
		"a simple ascii line",
		"multi\nline\r\npayload",
		"embedded control \x00\x01\x07\x1b[31m bytes",
		"unicode: café — 日本語 — 😀🚀 — ✓",
		"trailing spaces and tabs \t   ",
	}
	for _, n := range []int{255, 256, 1024, 65535, 262144} {
		corpus = append(corpus, strings.Repeat("Z", n))
	}
	for _, target := range []byte{'c', 'p'} {
		for _, text := range corpus {
			// Surround with the kind of chrome bytes a render frame carries so
			// the scanner must locate the sequence, not assume it stands alone.
			stream := append([]byte("\x1b[2J\x1b[H chrome "), osc52(target, text)...)
			stream = append(stream, []byte(" \x1b[0m trailing")...)
			clips := scanOSC52(stream)
			if len(clips) != 1 {
				t.Fatalf("target %q, %d-byte payload: scanned %d clips, want 1", target, len(text), len(clips))
			}
			if c := clips[0]; c.target != target || !c.ok || c.text != text {
				t.Errorf("target %q, %d-byte payload: got target=%q ok=%v textlen=%d",
					target, len(text), c.target, c.ok, len(c.text))
			}
		}
	}
}

// --- the full copy chain, across simulated terminals --------------------

// TestHostTerminalClipboardMatrixCtrlC is the headline test: a real Ctrl+C
// copy through the daemon, then the captured render stream replayed into each
// terminal model. Every OSC 52-capable terminal must end up holding the text;
// Terminal.app (no OSC 52) must NOT — which is exactly why the explicit-copy
// path also emits a native TypeCopy frame the client pipes into pbcopy
// locally. (Over SSH that native frame lands on the remote box, so for a
// remote session the OSC 52 column is what the user actually gets.)
func TestHostTerminalClipboardMatrixCtrlC(t *testing.T) {
	w, conn, s := newTestWS(t)
	s.waitFor(t, "first frame", func() bool { return s.contains("1:") })

	const marker = "COPY-OVER-SSH-OK"
	plantSelection(t, w, marker)
	w.handleInput(conn, []byte{0x03}) // Ctrl+C with a selection: copy, not SIGINT
	s.waitFor(t, "OSC 52 clipboard write", func() bool { return s.contains("\x1b]52;c;") })
	s.waitFor(t, "native fallback copy frame", func() bool { return s.sawCopy(protocol.CopyClipboard, marker) })

	stream := s.all()
	for _, term := range hostTerminals {
		clipboard, _ := term.receive(stream)
		switch {
		case term.supportsOSC52 && clipboard != marker:
			t.Errorf("%s: clipboard = %q, want %q (OSC 52 must round-trip here)", term.name, clipboard, marker)
		case !term.supportsOSC52 && clipboard != "":
			t.Errorf("%s: clipboard = %q, want empty (no OSC 52 — relies on the native frame)", term.name, clipboard)
		}
	}
	if !s.sawCopy(protocol.CopyClipboard, marker) {
		t.Fatal("explicit copy must also emit a native TypeCopy frame (the Terminal.app fallback)")
	}
}

// TestMouseDragFeedsPrimaryAcrossTerminals pins the ruling that a drag feeds
// PRIMARY (target 'p'), never CLIPBOARD: primary-honoring terminals surface
// the selection, primary-blind ones do not, and CLIPBOARD stays untouched
// until an explicit Ctrl+C. This is the documented reason copy-on-select does
// not reach the system clipboard.
func TestMouseDragFeedsPrimaryAcrossTerminals(t *testing.T) {
	w, conn, s := newTestWS(t)
	s.waitFor(t, "first frame", func() bool { return s.contains("1:") })
	withWS(w, func() {
		p := w.panes[w.lay.FocusedPane()]
		p.term.Write([]byte("\rDRAG-PRIMARY-LINE\r\n"))
	})

	w.handleInput(conn, press(1, 2))
	w.handleInput(conn, motion(12, 2))
	w.handleInput(conn, release(12, 2))
	s.waitFor(t, "primary OSC 52 write", func() bool { return s.contains("\x1b]52;p;") })

	stream := s.all()
	if clips := scanOSC52(stream); len(clips) == 0 {
		t.Fatal("drag produced no OSC 52 write")
	} else {
		for _, c := range clips {
			if c.target == 'c' {
				t.Errorf("drag must not feed CLIPBOARD ('c'); got %q", c.text)
			}
		}
	}
	for _, term := range hostTerminals {
		_, primary := term.receive(stream)
		wantPrimary := term.supportsOSC52 && term.honorsPrimary
		if wantPrimary && primary == "" {
			t.Errorf("%s: expected a primary selection from the drag", term.name)
		}
		if !wantPrimary && primary != "" {
			t.Errorf("%s: must not receive a primary selection, got %q", term.name, primary)
		}
	}
}

// TestInnerProgramOSC52ReachesAllClients covers an app INSIDE a pane writing
// OSC 52 (e.g. tea.SetClipboard): the daemon consumes it in the VT and
// re-broadcasts a fresh OSC 52 render frame plus a native frame to EVERY
// attached client, and updates the shared internal clipboard.
func TestInnerProgramOSC52ReachesAllClients(t *testing.T) {
	w, _, s1 := newTestWS(t)
	s1.waitFor(t, "client 1 first frame", func() bool { return s1.contains("1:") })
	_, s2 := attachClient(t, w)
	s2.waitFor(t, "client 2 first frame", func() bool { return s2.contains("1:") })

	const inner = "FROM-INNER-PROGRAM"
	w.clipFromPane("c", inner) // what pane.readLoop calls after DrainClips

	for i, s := range []*sink{s1, s2} {
		s.waitFor(t, fmt.Sprintf("client %d OSC 52", i+1), func() bool { return s.contains("\x1b]52;c;") })
		s.waitFor(t, fmt.Sprintf("client %d native frame", i+1), func() bool { return s.sawCopy(protocol.CopyClipboard, inner) })
		if clipboard, _ := hostTerminals[0].receive(s.all()); clipboard != inner {
			t.Errorf("client %d: gnome-terminal clipboard = %q, want %q", i+1, clipboard, inner)
		}
	}
	withWS(w, func() {
		if string(w.clip) != inner {
			t.Fatalf("internal clipboard = %q, want %q", w.clip, inner)
		}
	})
}

// TestExplicitCopyTargetsOnlyActingClient pins that Ctrl+C delivers the
// clipboard write only to the client that pressed it — not every attached
// terminal — so a second viewer's clipboard is not silently overwritten.
func TestExplicitCopyTargetsOnlyActingClient(t *testing.T) {
	w, connA, sA := newTestWS(t)
	sA.waitFor(t, "A first frame", func() bool { return sA.contains("1:") })
	_, sB := attachClient(t, w)
	sB.waitFor(t, "B first frame", func() bool { return sB.contains("1:") })

	const marker = "ONLY-FOR-CLIENT-A"
	plantSelection(t, w, marker)
	w.handleInput(connA, []byte{0x03})
	sA.waitFor(t, "A OSC 52", func() bool { return sA.contains("\x1b]52;c;") })
	sA.waitFor(t, "A native frame", func() bool { return sA.sawCopy(protocol.CopyClipboard, marker) })

	time.Sleep(150 * time.Millisecond) // let any stray B frames land
	if sB.contains("\x1b]52;") {
		t.Error("client B received an OSC 52 write for A's copy")
	}
	if sB.sawType(protocol.TypeCopy) {
		t.Error("client B received a native copy frame for A's copy")
	}
}

// --- paste ---------------------------------------------------------------

// TestCtrlVPastesInternalClipboardIntoPane is the SSH-proof paste path:
// Ctrl+V injects tide's own internal clipboard into the focused pane (no
// system-clipboard read, no OSC 52 query), so the shell echoes it back. This
// is why copy-then-paste inside tide works regardless of the host terminal.
func TestCtrlVPastesInternalClipboardIntoPane(t *testing.T) {
	w, conn, s := newTestWS(t)
	s.waitFor(t, "first frame", func() bool { return s.contains("1:") })

	const payload = "paste-marker-7f3a" // single line, no control bytes → no guard
	withWS(w, func() { w.clip = []byte(payload) })
	w.handleInput(conn, []byte{0x16}) // Ctrl+V
	s.waitFor(t, "shell echoes the pasted text", func() bool { return s.contains(payload) })
	withWS(w, func() {
		if w.overlay != nil {
			t.Fatal("a safe single-line paste must not raise the confirm guard")
		}
	})
}

// TestBracketedPasteBypassesConfirmGuard: when the pane's app has bracketed
// paste on (DECSET 2004), the app frames the paste itself, so tide's
// multi-line confirm guard is skipped (the guard exists only to protect a
// bare shell).
func TestBracketedPasteBypassesConfirmGuard(t *testing.T) {
	w, conn, s := newTestWS(t)
	s.waitFor(t, "first frame", func() bool { return s.contains("1:") })
	withWS(w, func() {
		p := w.panes[w.lay.FocusedPane()]
		p.term.Write([]byte("\x1b[?2004h")) // the app enables bracketed paste
		w.clip = []byte("line1\nline2\nline3")
	})
	w.handleInput(conn, []byte{0x16}) // Ctrl+V
	time.Sleep(150 * time.Millisecond)
	withWS(w, func() {
		if w.overlay != nil {
			t.Fatal("bracketed-paste pane must accept a multi-line paste without confirming")
		}
	})
}

// --- selection escape hatch ---------------------------------------------

// TestShiftBypassEnablesSelectionWhenAppGrabsMouse pins the user-facing
// answer to "I can't select to copy": when the pane's app has mouse reporting
// on, a plain press goes to the app (so drags no longer select), but Shift+
// press makes tide own the mouse again and start a selection.
func TestShiftBypassEnablesSelectionWhenAppGrabsMouse(t *testing.T) {
	w, conn, s := newTestWS(t)
	s.waitFor(t, "first frame", func() bool { return s.contains("1:") })
	withWS(w, func() {
		p := w.panes[w.lay.FocusedPane()]
		p.term.Write([]byte("\x1b[?1000h")) // app requests mouse reporting
		p.term.Write([]byte("\rSHIFT-SELECT-LINE\r\n"))
	})

	// Plain press: forwarded to the app, no tide selection.
	w.handleInput(conn, press(2, 2))
	withWS(w, func() {
		if w.sel.dragging {
			t.Fatal("with app mouse reporting on, a plain press must not start a selection")
		}
		if w.appGrab == "" {
			t.Fatal("a plain press should grab the mouse for the app")
		}
	})
	w.handleInput(conn, release(2, 2))
	withWS(w, func() {
		if w.appGrab != "" {
			t.Fatal("release must end the app grab")
		}
	})

	// Shift+press: tide owns the mouse again and a selection begins.
	w.handleInput(conn, pressShift(2, 2))
	withWS(w, func() {
		if !w.sel.dragging {
			t.Fatal("Shift+press must start a selection even when the app wants the mouse")
		}
	})
}
