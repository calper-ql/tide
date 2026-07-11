package vt

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

// maskGlyph drops bookkeeping bits that have no display meaning and no SGR
// representation (attrWrap, attrGfx) and normalizes the NUL/space ambiguity.
func maskGlyph(g Glyph) Glyph {
	g.Mode &^= attrWrap | attrGfx
	if g.Char == 0 {
		g.Char = ' '
	}
	return g
}

func compareTerms(t *testing.T, name string, a, b *Term) {
	t.Helper()
	a.State.lock()
	b.State.lock()
	defer a.State.unlock()
	defer b.State.unlock()

	if a.cols != b.cols || a.rows != b.rows {
		t.Fatalf("%s: size %dx%d != %dx%d", name, a.cols, a.rows, b.cols, b.rows)
	}
	if a.mode != b.mode {
		t.Errorf("%s: mode %032b != %032b", name, a.mode, b.mode)
	}
	if a.title != b.title {
		t.Errorf("%s: title %q != %q", name, a.title, b.title)
	}
	if a.top != b.top || a.bottom != b.bottom {
		t.Errorf("%s: scroll region %d-%d != %d-%d", name, a.top, a.bottom, b.top, b.bottom)
	}
	if a.cur.X != b.cur.X || a.cur.Y != b.cur.Y || a.cur.State != b.cur.State {
		t.Errorf("%s: cursor (%d,%d,%08b) != (%d,%d,%08b)",
			name, a.cur.X, a.cur.Y, a.cur.State, b.cur.X, b.cur.Y, b.cur.State)
	}
	if maskGlyph(a.cur.Attr) != maskGlyph(b.cur.Attr) ||
		a.cur.Attr.Mode&attrGfx != b.cur.Attr.Mode&attrGfx {
		t.Errorf("%s: pen %+v != %+v", name, a.cur.Attr, b.cur.Attr)
	}
	if a.curSaved.X != b.curSaved.X || a.curSaved.Y != b.curSaved.Y ||
		maskGlyph(a.curSaved.Attr) != maskGlyph(b.curSaved.Attr) {
		t.Errorf("%s: saved cursor (%d,%d) != (%d,%d)",
			name, a.curSaved.X, a.curSaved.Y, b.curSaved.X, b.curSaved.Y)
	}
	if a.mode&ModeAltScreen == 0 && a.curSaved.State != b.curSaved.State {
		// (the alt path reconstructs the saved slot via ?1049h, which
		// cannot carry State bits — a documented gap)
		t.Errorf("%s: saved cursor state %08b != %08b", name, a.curSaved.State, b.curSaved.State)
	}
	if string(a.seq) != string(b.seq) || a.seqOverflow != b.seqOverflow {
		t.Errorf("%s: in-flight seq %q (ovf %v) != %q (ovf %v)",
			name, a.seq, a.seqOverflow, b.seq, b.seqOverflow)
	}
	if string(a.pending) != string(b.pending) {
		t.Errorf("%s: pending utf8 %q != %q", name, a.pending, b.pending)
	}
	for i, k := range a.tabs {
		if b.tabs[i] != k {
			t.Errorf("%s: tab stop %d: %v != %v", name, i, k, b.tabs[i])
			break
		}
	}
	for c, v := range a.colorOverride {
		if b.colorOverride[c] != v {
			t.Errorf("%s: color override %d: %v != %v", name, c, v, b.colorOverride[c])
		}
	}
	compareGrid(t, name+"/screen", a.lines, b.lines, a.cols, a.rows)
	compareGrid(t, name+"/altscreen", a.altLines, b.altLines, a.cols, a.rows)
}

func compareGrid(t *testing.T, name string, ga, gb []line, cols, rows int) {
	t.Helper()
	for y := 0; y < rows; y++ {
		for x := 0; x < cols; x++ {
			a, b := maskGlyph(ga[y][x]), maskGlyph(gb[y][x])
			if a != b {
				t.Errorf("%s: cell (%d,%d): %+v != %+v", name, x, y, a, b)
				return
			}
		}
	}
}

func roundtrip(t *testing.T, name string, stream, continuation []byte) {
	t.Helper()
	a := New(80, 24, 200, nil)
	a.Write(stream)
	snap := a.Snapshot(false, 0)
	b := New(80, 24, 200, nil)
	b.Write(snap)
	compareTerms(t, name+"/restored", a, b)
	if len(continuation) > 0 {
		// The decisive property: after restoring, both terminals must
		// interpret the SAME raw byte stream identically — parser state
		// (pen, charset, wrap-pending, region) survived the snapshot.
		a.Write(continuation)
		b.Write(continuation)
		compareTerms(t, name+"/continued", a, b)
	}
}

func TestSnapshotRoundtrip(t *testing.T) {
	continuation := []byte("Z\r\ncontinued \x1b[31mred\x1b[0m é漢\t.\x1b8Q")
	cases := []struct {
		name   string
		stream string
	}{
		{"empty", ""},
		{"plain", "hello\r\nworld"},
		{"wrap", strings.Repeat("a", 200)},
		{"wrap-pending", strings.Repeat("x", 80)},
		{"sgr-zoo", "\x1b[1;31mbold red\x1b[0m \x1b[4;42munder green-bg\x1b[0m " +
			"\x1b[38;5;200mxterm\x1b[0m \x1b[38;2;1;2;3mrgb\x1b[0m \x1b[90mbright\x1b[m"},
		{"reverse-bold", "\x1b[7;1;31;44mREV\x1b[0m plain \x1b[7mdefrev\x1b[27m"},
		{"faint", "\x1b[2mdim\x1b[22m normal \x1b[2;31mdim-red\x1b[0m \x1b[1;2mbold+dim\x1b[0m"},
		{"cup-clears", "1234567890\x1b[1;3H\x1b[K\x1b[10;10Hmid\x1b[2;1Habove\x1b[J"},
		{"region-origin", "\x1b[5;20r\x1b[?6htop\r\n\x1b[20;1Hbottom-line\r\nscrolled"},
		{"region-no-origin", "\x1b[3;10rline\r\nline\r\nline\r\nline"},
		{"altscreen", "main screen text\x1b[?1049halt content\x1b[5;5Halt-mid"},
		{"modes", "\x1b[?1h\x1b[?2004h\x1b[?1000h\x1b[?1006h\x1b[?25l\x1b="},
		{"no-wrap-mode", "\x1b[?7l" + strings.Repeat("y", 100)},
		{"charset-pen", "\x1b(0lqqk"},
		{"charset-mixed", "abc\x1b(0xx\x1b(Bdone\x1b(0"},
		{"title", "\x1b]0;my build\atext"},
		{"tabs", "\x1b[3g\x1b[1;5H\x1bH\x1b[1;13H\x1bH\rA\tB\tC"},
		{"decsc", "\x1b[33m\x1b[3;7H\x1b7\x1b[0m\x1b[10;1Hafter-save"},
		{"osc4", "\x1b]4;1;rgb:aa/bb/cc\acolored\x1b[31mred-override"},
		{"osc10", "\x1b]10;#102030\afg-override"},
		{"osc11", "\x1b]11;#334455\abg-override"},
		{"decsc-origin", "\x1b[?6h\x1b[3;7H\x1b[35m\x1b7\x1b[?6l\x1b[0m\x1b[H"},
		{"decsc-wrap-pending", strings.Repeat("p", 80) + "\x1b7\x1b[5;1Helse"},
		{"wide", "漢字 wide 🚀 mixed 末"},
		{"wide-wrap", strings.Repeat("x", 79) + "漢 after-wrap"},
		{"wide-overwrite", "ab漢cd\x1b[1;4HX"},
		{"utf8", "héllo ⌘ 漢字 → done"},
		{"scrolled", func() string {
			var sb strings.Builder
			for i := 0; i < 40; i++ {
				fmt.Fprintf(&sb, "line %d\r\n", i)
			}
			return sb.String()
		}()},
		{"insert-delete", "abcdef\x1b[1;3H\x1b[2@XY\x1b[2;1Hqrstuv\x1b[2;2H\x1b[3P"},
		{"scroll-updown", "\x1b[2;10rA\r\nB\r\nC\r\nD\x1b[2S\x1b[1T"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			roundtrip(t, tc.name, []byte(tc.stream), continuation)
		})
	}
}

func TestSnapshotRoundtripWrapPendingContinuation(t *testing.T) {
	// The next printable after a full row must land at column 0 of the next
	// row in both terminals — wrap-pending survived the snapshot.
	a := New(10, 4, 0, nil)
	a.Write([]byte(strings.Repeat("w", 10)))
	b := New(10, 4, 0, nil)
	b.Write(a.Snapshot(false, 0))
	a.Write([]byte("Z"))
	b.Write([]byte("Z"))
	compareTerms(t, "wrap-pending-z", a, b)
	b.State.lock()
	g := b.lines[1][0]
	b.State.unlock()
	if g.Char != 'Z' {
		t.Fatalf("continuation glyph landed wrong: %+v", g)
	}
}

func TestHistoryCaptureAndReplay(t *testing.T) {
	a := New(20, 5, 100, nil)
	for i := 0; i < 30; i++ {
		fmt.Fprintf(a, "history line %d\r\n", i)
	}
	a.State.lock()
	n := a.HistoryLen()
	a.State.unlock()
	// 30 lines printed on a 5-row screen; the cursor sits on the line after
	// the 30th, so 26 lines have scrolled off the top.
	if n != 26 {
		t.Fatalf("HistoryLen = %d, want 26", n)
	}

	b := New(20, 5, 100, nil)
	b.Write(a.Snapshot(true, 0))
	compareTerms(t, "with-history", a, b)

	// B's history must start with A's history verbatim (text-wise); the
	// replay pads with blank lines after, which is acceptable.
	b.State.lock()
	defer b.State.unlock()
	a.State.lock()
	defer a.State.unlock()
	if b.HistoryLen() < n {
		t.Fatalf("replayed history %d < original %d", b.HistoryLen(), n)
	}
	for i := 0; i < n; i++ {
		if got, want := lineText(b.historyLine(i)), lineText(a.historyLine(i)); got != want {
			t.Fatalf("history line %d: %q != %q", i, got, want)
		}
	}
	for i := n; i < b.HistoryLen(); i++ {
		if got := strings.TrimSpace(lineText(b.historyLine(i))); got != "" {
			t.Fatalf("padding line %d not blank: %q", i, got)
		}
	}
}

func lineText(l line) string {
	var sb strings.Builder
	for _, g := range l {
		if g.Char == 0 {
			sb.WriteByte(' ')
		} else {
			sb.WriteRune(g.Char)
		}
	}
	return strings.TrimRight(sb.String(), " ")
}

func TestHistoryRingWraps(t *testing.T) {
	a := New(10, 3, 5, nil)
	for i := 0; i < 20; i++ {
		fmt.Fprintf(a, "%d\r\n", i)
	}
	a.State.lock()
	defer a.State.unlock()
	if a.HistoryLen() != 5 {
		t.Fatalf("HistoryLen = %d, want capped 5", a.HistoryLen())
	}
	// 20 lines on a 3-row screen leave "18","19",cursor visible: lines
	// 0..17 scrolled off, and the capped ring retains the newest five,
	// 13..17.
	for i := 0; i < 5; i++ {
		if got, want := lineText(a.historyLine(i)), fmt.Sprint(13+i); got != want {
			t.Fatalf("ring line %d = %q, want %q", i, got, want)
		}
	}
}

// TestSnapshotMidSequence pins the mid-keystroke promise at its sharpest:
// a snapshot taken while an escape sequence is partially received must
// leave the restored terminal waiting mid-sequence exactly like the source.
func TestSnapshotMidSequence(t *testing.T) {
	cases := []struct{ name, pre, post string }{
		{"csi", "hello \x1b[3", "1mRED"},
		{"csi-args", "x\x1b[12;", "24Hy"},
		{"osc-title", "\x1b]0;ti", "tle\adone"},
		{"esc-only", "z\x1b", "[33mc"},
		{"charset", "q\x1b(", "0lq"},
		{"dcs", "d\x1bP1$", "r0m\x1b\\after"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := New(80, 24, 50, nil)
			a.Write([]byte(tc.pre))
			b := New(80, 24, 50, nil)
			b.Write(a.Snapshot(false, 0))
			compareTerms(t, tc.name+"/mid", a, b)
			a.Write([]byte(tc.post))
			b.Write([]byte(tc.post))
			compareTerms(t, tc.name+"/completed", a, b)
		})
	}
}

func TestSnapshotMidUTF8Rune(t *testing.T) {
	a := New(20, 3, 0, nil)
	a.Write([]byte{'h', 0xe6, 0xbc}) // first two bytes of 漢 (E6 BC A2)
	b := New(20, 3, 0, nil)
	b.Write(a.Snapshot(false, 0))
	compareTerms(t, "mid-utf8", a, b)
	a.Write([]byte{0xa2, '!'})
	b.Write([]byte{0xa2, '!'})
	compareTerms(t, "mid-utf8/completed", a, b)
	b.State.lock()
	defer b.State.unlock()
	if b.lines[0][1].Char != '漢' {
		t.Fatalf("completed rune = %q", b.lines[0][1].Char)
	}
}

func TestAltScreenResetOnMainIsNoop(t *testing.T) {
	a := New(10, 3, 0, nil)
	a.Write([]byte("main\x1b[?1049l"))
	a.State.lock()
	defer a.State.unlock()
	if a.mode&ModeAltScreen != 0 {
		t.Fatal("1049l on the main screen must not swap to alt (upstream bug)")
	}
	if a.lines[0][0].Char != 'm' {
		t.Fatalf("screen clobbered: %q", a.lines[0][0].Char)
	}
}

func TestSnapshotAvoidsRISAndResetsBeforeHistory(t *testing.T) {
	a := New(20, 4, 50, nil)
	for i := 0; i < 10; i++ {
		fmt.Fprintf(a, "hist-line-%d\r\n", i)
	}
	snap := a.Snapshot(true, 0)
	if bytes.Contains(snap, []byte("\x1bc")) {
		t.Fatal("snapshot contains RIS, which wipes scrollback on VTE terminals")
	}
	reset := bytes.Index(snap, []byte("\x1b[?1049l"))
	hist := bytes.Index(snap, []byte("hist-line-0"))
	if reset == -1 || hist == -1 || reset > hist {
		t.Fatalf("reset (%d) must precede the history replay (%d)", reset, hist)
	}
}

func TestWideGlyphsOccupyTwoCells(t *testing.T) {
	a := New(10, 3, 0, nil)
	a.Write([]byte("a漢b"))
	a.State.lock()
	r := a.lines[0]
	if r[0].Char != 'a' || r[1].Char != '漢' || r[1].Mode&attrWide == 0 ||
		r[2].Mode&attrWideDummy == 0 || r[3].Char != 'b' || a.cur.X != 4 {
		a.State.unlock()
		t.Fatalf("row = %+v cursor=%d", r[:5], a.cur.X)
	}
	a.State.unlock()

	// Overwriting the dummy half blanks the lead — no torn pairs.
	a.Write([]byte("\x1b[1;3HX"))
	a.State.lock()
	defer a.State.unlock()
	r = a.lines[0]
	if r[1].Char != ' ' || r[1].Mode&attrWide != 0 || r[2].Char != 'X' {
		t.Fatalf("torn pair not repaired: %+v", r[:4])
	}
}

func TestWideGlyphWrapsAtLineEnd(t *testing.T) {
	a := New(4, 3, 0, nil)
	a.Write([]byte("abc漢"))
	a.State.lock()
	defer a.State.unlock()
	if a.lines[1][0].Char != '漢' || a.lines[1][0].Mode&attrWide == 0 ||
		a.lines[1][1].Mode&attrWideDummy == 0 {
		t.Fatalf("wide glyph should wrap whole: row1 = %+v", a.lines[1][:2])
	}
}

func TestResizeShrinkPushesRowsToHistory(t *testing.T) {
	a := New(20, 10, 100, nil)
	for i := 0; i < 8; i++ {
		fmt.Fprintf(a, "row-%d\r\n", i)
	}
	a.Resize(20, 4)
	a.State.lock()
	defer a.State.unlock()
	// Cursor was on row 8, so the slide drops rows 0..4 — they must land in
	// the ring, not vanish (requirement 1: content survives a shrink).
	if a.HistoryLen() != 5 {
		t.Fatalf("HistoryLen = %d, want 5", a.HistoryLen())
	}
	for i := 0; i < 5; i++ {
		if got, want := lineText(a.historyLine(i)), fmt.Sprintf("row-%d", i); got != want {
			t.Fatalf("history[%d] = %q, want %q", i, got, want)
		}
	}
}

func TestUTF8SplitAcrossWrites(t *testing.T) {
	a := New(10, 2, 0, nil)
	a.Write([]byte{'h', 0xc3})
	a.Write([]byte{0xa9, '!'}) // é split across writes, then '!'
	a.State.lock()
	defer a.State.unlock()
	if a.lines[0][0].Char != 'h' || a.lines[0][1].Char != 'é' || a.lines[0][2].Char != '!' {
		t.Fatalf("glyphs = %q %q %q", a.lines[0][0].Char, a.lines[0][1].Char, a.lines[0][2].Char)
	}
}

func TestSnapshotIsParseableANSI(t *testing.T) {
	// Smoke check: a snapshot must not contain raw NUL bytes and must end
	// with the terminal usable (cursor placed).
	a := New(80, 24, 10, nil)
	a.Write([]byte("x\x1b[31my\x1b[?2004h"))
	snap := a.Snapshot(true, 0)
	if bytes.IndexByte(snap, 0) != -1 {
		t.Fatal("snapshot contains NUL bytes")
	}
	if !bytes.Contains(snap, []byte("\x1b[?2004h")) {
		t.Fatal("snapshot missing bracketed-paste restore")
	}
}

func TestResizeGrowPullsHistoryBack(t *testing.T) {
	a := New(20, 6, 100, nil)
	a.Write([]byte("1\r\n2\r\n3\r\n4\r\n5\r\n$ "))
	a.Resize(20, 4) // 1 and 2 leave through the history ring
	if a.HistoryLen() != 2 {
		t.Fatalf("HistoryLen after shrink = %d, want 2", a.HistoryLen())
	}
	a.Resize(20, 6) // growth pulls them back: content stays bottom-anchored
	a.State.lock()
	defer a.State.unlock()
	if a.HistoryLen() != 0 {
		t.Fatalf("HistoryLen after grow = %d, want 0", a.HistoryLen())
	}
	want := []string{"1", "2", "3", "4", "5", "$"}
	for i, w := range want {
		if got := lineText(a.lines[i]); got != w {
			t.Fatalf("row %d = %q, want %q", i, got, w)
		}
	}
	if a.cur.Y != 5 || a.cur.X != 2 {
		t.Fatalf("cursor = (%d,%d), want (2,5)", a.cur.X, a.cur.Y)
	}
}

func TestResizeShrinkDropsBelowCursorRows(t *testing.T) {
	// Rows cut off BELOW the cursor are dropped (tmux/xterm), never pushed:
	// pushing them would put below-cursor content above the screen in
	// scrollback order.
	a := New(20, 6, 100, nil)
	a.Write([]byte("A\r\nB\r\nC\r\nD\x1b[2;1H")) // cursor on row 1 (B)
	a.Resize(20, 2)
	a.State.lock()
	if a.HistoryLen() != 0 {
		t.Fatalf("HistoryLen = %d, want 0 (C/D dropped, not pushed)", a.HistoryLen())
	}
	if got := lineText(a.lines[0]); got != "A" {
		t.Fatalf("row 0 = %q, want A", got)
	}
	if got := lineText(a.lines[1]); got != "B" {
		t.Fatalf("row 1 = %q, want B", got)
	}
	a.State.unlock()
	a.Resize(20, 6) // nothing to pull; blanks appear below
	a.State.lock()
	defer a.State.unlock()
	if got := strings.TrimSpace(lineText(a.lines[2])); got != "" {
		t.Fatalf("row 2 = %q, want blank", got)
	}
}

func TestResizeUnderAltRestoresMainOnExit(t *testing.T) {
	// The main grid keeps round-tripping through history while an alt-screen
	// app is up (tmux-verified: shrink+grow under vim, then exit, restores
	// the shell screen exactly); the alt grid itself never touches history.
	a := New(20, 6, 100, nil)
	a.Write([]byte("1\r\n2\r\n3\r\n4\r\n5\r\n$ "))
	a.Write([]byte("\x1b[?1049h\x1b[2J\x1b[Halt"))
	a.Resize(20, 4)
	a.Resize(20, 6)
	a.Write([]byte("\x1b[?1049l")) // back to main: fully restored
	a.State.lock()
	defer a.State.unlock()
	if a.HistoryLen() != 0 {
		t.Fatalf("HistoryLen = %d, want 0 (main pulled back under alt)", a.HistoryLen())
	}
	want := []string{"1", "2", "3", "4", "5", "$"}
	for i, w := range want {
		if got := lineText(a.lines[i]); got != w {
			t.Fatalf("main row %d after alt roundtrip = %q, want %q", i, got, w)
		}
	}
	if a.cur.Y != 5 || a.cur.X != 2 {
		t.Fatalf("restored cursor = (%d,%d), want (2,5)", a.cur.X, a.cur.Y)
	}
}

func TestResizeGrowAfterClearStaysTopAnchored(t *testing.T) {
	// Growth spends the blank slack below the content before pulling from
	// history (tmux-verified): a just-cleared screen must not have old
	// scrollback shoved back on top of the fresh prompt.
	a := New(20, 6, 100, nil)
	for i := 0; i < 12; i++ {
		fmt.Fprintf(a, "line%02d\r\n", i)
	}
	a.Write([]byte("\x1b[2J\x1b[1;1H$ ")) // clear + prompt at top
	hist := a.HistoryLen()
	a.Resize(20, 10)
	a.State.lock()
	defer a.State.unlock()
	if a.HistoryLen() != hist {
		t.Fatalf("HistoryLen = %d, want %d (no pull into a cleared screen)", a.HistoryLen(), hist)
	}
	if got := lineText(a.lines[0]); got != "$" {
		t.Fatalf("row 0 = %q, want %q", got, "$")
	}
	if a.cur.Y != 0 {
		t.Fatalf("cursor row = %d, want 0", a.cur.Y)
	}
}

func TestResizePullAfterRingWrap(t *testing.T) {
	// Pulling from a WRAPPED ring must leave it consistent: the next push
	// lands in the popped slot, not over a live line, and no history read
	// panics (the ring slice must never shrink below the wrap point).
	a := New(20, 4, 6, nil) // tiny ring so it wraps fast
	for i := 0; i < 20; i++ {
		fmt.Fprintf(a, "L%02d\r\n", i)
	}
	// 17 lines scrolled through the 4-row screen; the wrapped ring holds
	// L11..L16 and the screen shows L17 L18 L19 with the cursor below.
	a.Resize(20, 2) // slide 2: L17 L18 join the (still wrapped) ring
	a.Resize(20, 4) // pull L17 L18 back out of the wrapped ring
	a.Write([]byte("X1\r\nX2\r\nX3\r\n")) // re-pushes L17 L18 L19
	a.State.lock()
	defer a.State.unlock()
	if a.HistoryLen() != 6 {
		t.Fatalf("HistoryLen = %d, want 6", a.HistoryLen())
	}
	want := []string{"L14", "L15", "L16", "L17", "L18", "L19"}
	for i, w := range want {
		if got := lineText(a.historyLine(i)); got != w {
			var all []string
			for j := 0; j < a.HistoryLen(); j++ {
				all = append(all, lineText(a.historyLine(j)))
			}
			t.Fatalf("history[%d] = %q, want %q (full: %v)", i, got, w, all)
		}
	}
}
