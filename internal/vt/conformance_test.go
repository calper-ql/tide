package vt

import (
	"bytes"
	"testing"
)

// penAttr runs a stream and returns the resulting cursor pen.
func penAttr(stream string) Glyph {
	a := New(20, 4, 0, nil)
	a.Write([]byte(stream))
	a.State.lock()
	defer a.State.unlock()
	return a.cur.Attr
}

// ---- SGR: colon sub-parameters (ITU/xterm) -------------------------------

func TestColonTruecolorFG(t *testing.T) {
	want := Color(255<<16 | 0<<8 | 0)
	if g := penAttr("\x1b[38:2:255:0:0m"); g.FG != want {
		t.Fatalf("38:2:255:0:0 -> FG %v, want %v", g.FG, want)
	}
	// W3C form with an empty colorspace-id field must skip it.
	if g := penAttr("\x1b[48:2::0:128:0m"); g.BG != Color(0<<16|128<<8|0) {
		t.Fatalf("48:2::0:128:0 -> BG %v, want green", g.BG)
	}
	if g := penAttr("\x1b[38:5:196m"); g.FG != Color(196) {
		t.Fatalf("38:5:196 -> FG %v, want 196", g.FG)
	}
}

// A colon sequence must NOT drop the args that follow it in the same SGR.
func TestColonDoesNotPoisonTrailingArgs(t *testing.T) {
	g := penAttr("\x1b[38:2:255:0:0;1m")
	if g.FG != Color(255<<16) {
		t.Fatalf("FG = %v, want red (colon color lost)", g.FG)
	}
	if g.Mode&attrBold == 0 {
		t.Fatal("trailing ;1 (bold) was dropped after the colon color")
	}
}

// ESC[4:3m is ONE styled underline, not underline+italic (which ESC[4;3m is).
func TestColonUnderlineStyleNotItalic(t *testing.T) {
	g := penAttr("\x1b[4:3m")
	if g.Mode&attrUnderline == 0 {
		t.Fatal("4:3 did not set underline")
	}
	if g.Mode&attrItalic != 0 {
		t.Fatal("4:3 wrongly set italic (colon subparam leaked as a separate SGR)")
	}
	if g := penAttr("\x1b[4m\x1b[4:0m"); g.Mode&attrUnderline != 0 {
		t.Fatal("4:0 did not turn underline off")
	}
}

// Legacy ';' truecolor/indexed forms must still work.
func TestLegacyExtendedColorStillWorks(t *testing.T) {
	if g := penAttr("\x1b[38;2;1;2;3m"); g.FG != Color(1<<16|2<<8|3) {
		t.Fatalf("38;2;1;2;3 -> FG %v", g.FG)
	}
	if g := penAttr("\x1b[48;5;200m"); g.BG != Color(200) {
		t.Fatalf("48;5;200 -> BG %v", g.BG)
	}
}

// ---- SGR: previously-dropped attributes ----------------------------------

func TestStrikethroughConcealOverline(t *testing.T) {
	if g := penAttr("\x1b[9m"); g.Mode&attrStrike == 0 {
		t.Fatal("SGR 9 (strikethrough) not set")
	}
	if g := penAttr("\x1b[8m"); g.Mode&attrConceal == 0 {
		t.Fatal("SGR 8 (conceal) not set")
	}
	if g := penAttr("\x1b[53m"); g.Mode&attrOverline == 0 {
		t.Fatal("SGR 53 (overline) not set")
	}
	// And they re-emit so the client renders them.
	var b bytes.Buffer
	appendSGR(&b, Glyph{FG: DefaultFG, BG: DefaultBG, Mode: attrStrike | attrConceal | attrOverline})
	if got := b.String(); got != "\x1b[0;8;9;53m" {
		t.Fatalf("appendSGR = %q, want %q", got, "\x1b[0;8;9;53m")
	}
	// 58 (underline color) must be consumed, not misparsed into following SGRs.
	if g := penAttr("\x1b[58:2::1:2:3;1m"); g.Mode&attrBold == 0 {
		t.Fatal("58:2:... poisoned the trailing ;1 bold")
	}
}

// ---- CSI parser: empty parameters ----------------------------------------

func TestEmptyCSIParamsKeepDefaults(t *testing.T) {
	// ESC[;5H -> row default (1), col 5.
	a := New(20, 4, 0, nil)
	a.Write([]byte("\x1b[;5HX"))
	a.State.lock()
	defer a.State.unlock()
	if a.lines[0][4].Char != 'X' {
		t.Fatalf("ESC[;5H put X at the wrong cell; row0 = %q", lineText(a.lines[0]))
	}
}

func TestEmptyCSIParamMidList(t *testing.T) {
	// ESC[1;;3m -> [1,0,3] -> bold, reset, italic == italic only.
	g := penAttr("\x1b[1;;3m")
	if g.Mode&attrItalic == 0 {
		t.Fatal("ESC[1;;3m did not end italic")
	}
	if g.Mode&attrBold != 0 {
		t.Fatal("ESC[1;;3m left bold set (empty middle param not treated as reset)")
	}
}

// ---- REP (CSI b) ----------------------------------------------------------

func TestREP(t *testing.T) {
	a := New(20, 3, 0, nil)
	a.Write([]byte("A\x1b[5b"))
	a.State.lock()
	got := lineText(a.lines[0])
	a.State.unlock()
	if got != "AAAAAA" {
		t.Fatalf("A then REP 5 = %q, want AAAAAA", got)
	}
	// No prior graphic char -> no-op (not a NUL/space spray).
	b := New(20, 3, 0, nil)
	b.Write([]byte("\x1b[3b"))
	b.State.lock()
	got = lineText(b.lines[0])
	b.State.unlock()
	if got != "" {
		t.Fatalf("REP with no prior char produced %q", got)
	}
}

// ---- Erase: ED 1 (above) and ED 3 (scrollback) ---------------------------

func TestEraseAboveClearsRowZero(t *testing.T) {
	a := New(10, 4, 0, nil)
	a.Write([]byte("XXXXX\r\nYY")) // row0 full, cursor on row1 col2
	a.Write([]byte("\x1b[1J"))     // erase above (inclusive)
	a.State.lock()
	defer a.State.unlock()
	if got := lineText(a.lines[0]); got != "" {
		t.Fatalf("ED 1 left row0 = %q (off-by-one; row0 not cleared)", got)
	}
}

func TestEraseScrollback(t *testing.T) {
	a := New(10, 3, 50, nil)
	for i := 0; i < 20; i++ {
		a.Write([]byte("line\r\n"))
	}
	a.State.lock()
	had := a.HistoryLen()
	a.State.unlock()
	if had == 0 {
		t.Fatal("setup: expected scrollback")
	}
	a.Write([]byte("\x1b[3J"))
	a.State.lock()
	defer a.State.unlock()
	if a.HistoryLen() != 0 {
		t.Fatalf("ESC[3J left %d scrollback lines", a.HistoryLen())
	}
}

// ---- RIS (ESC c) ----------------------------------------------------------

func TestRISClearsWholeScreen(t *testing.T) {
	a := New(30, 6, 0, nil)
	a.Write([]byte("\x1b#8")) // fill every cell with 'E'
	a.Write([]byte("\x1bc"))  // RIS
	a.State.lock()
	defer a.State.unlock()
	for y := 0; y < a.rows; y++ {
		for x := 0; x < a.cols; x++ {
			if a.lines[y][x].Char != ' ' && a.lines[y][x].Char != 0 {
				t.Fatalf("RIS left (%d,%d)=%q uncleared", x, y, a.lines[y][x].Char)
			}
		}
	}
}

func TestRISReturnsToMainScreen(t *testing.T) {
	a := New(20, 4, 0, nil)
	a.Write([]byte("main\x1b[?1049halt\x1bc"))
	a.State.lock()
	defer a.State.unlock()
	if a.mode&ModeAltScreen != 0 {
		t.Fatal("RIS left the alt-screen bit set")
	}
}

// ---- BCE on scroll --------------------------------------------------------

func TestScrollHonorsBCE(t *testing.T) {
	a := New(10, 3, 0, nil)
	a.Write([]byte("\x1b[41m"))         // red background pen
	a.Write([]byte("\r\n\r\n\r\n\r\n")) // force scrolling at the bottom
	a.State.lock()
	defer a.State.unlock()
	bottom := a.lines[a.rows-1]
	if bottom[0].BG != Color(Red) {
		t.Fatalf("scrolled-in bottom line BG = %v, want red (BCE)", bottom[0].BG)
	}
}

// ---- IRM insert mode ------------------------------------------------------

func TestInsertMode(t *testing.T) {
	a := New(20, 3, 0, nil)
	a.Write([]byte("ABCDE"))          // row: ABCDE
	a.Write([]byte("\x1b[4h\x1b[1G")) // insert mode on, cursor to col 1
	a.Write([]byte("XY"))             // should shift ABCDE right
	a.State.lock()
	got := lineText(a.lines[0])
	a.State.unlock()
	if got != "XYABCDE" {
		t.Fatalf("IRM insert = %q, want XYABCDE", got)
	}
}

// ---- charset designation consumes its selector ---------------------------

func TestG1CharsetSelectorConsumed(t *testing.T) {
	a := New(10, 3, 0, nil)
	a.Write([]byte("\x1b)0X")) // designate G1 = line-drawing, then print X
	a.State.lock()
	got := lineText(a.lines[0])
	a.State.unlock()
	if got != "X" {
		t.Fatalf("ESC ) 0 X = %q, want X (selector '0' must not print)", got)
	}
}

// ---- CAN/SUB abort a sequence and return to ground ------------------------

func TestCANAbortsCSI(t *testing.T) {
	a := New(10, 3, 0, nil)
	a.Write([]byte("\x1b[31\x18m")) // CAN mid-CSI; the 'm' is then a literal
	a.State.lock()
	got := lineText(a.lines[0])
	a.State.unlock()
	if got != "m" {
		t.Fatalf("after CAN, row0 = %q, want m (parser must return to ground)", got)
	}
}

// ---- DECSTR soft reset ----------------------------------------------------

func TestDECSTRSoftReset(t *testing.T) {
	a := New(20, 10, 0, nil)
	a.Write([]byte("\x1b[3;8r\x1b[4h\x1b[?6h")) // scroll region, IRM, origin
	a.Write([]byte("\x1b[!p"))                  // DECSTR
	a.State.lock()
	defer a.State.unlock()
	if a.top != 0 || a.bottom != a.rows-1 {
		t.Fatalf("DECSTR left scroll region %d-%d", a.top, a.bottom)
	}
	if a.mode&ModeInsert != 0 {
		t.Fatal("DECSTR left IRM set")
	}
	if a.cur.State&cursorOrigin != 0 {
		t.Fatal("DECSTR left origin mode set")
	}
}

// ---- DECSTBM rejects an inverted region -----------------------------------

func TestDECSTBMInvertedIgnored(t *testing.T) {
	a := New(20, 10, 0, nil)
	a.Write([]byte("\x1b[5;2r")) // top >= bottom: ignore
	a.State.lock()
	defer a.State.unlock()
	if a.top != 0 || a.bottom != a.rows-1 {
		t.Fatalf("inverted DECSTBM created region %d-%d, want full", a.top, a.bottom)
	}
}

// ---- DECSCUSR cursor shape ------------------------------------------------

func TestDECSCUSR(t *testing.T) {
	a := New(20, 3, 0, nil)
	a.Write([]byte("\x1b[5 q")) // blinking bar
	if a.CursorShape() != 5 {
		t.Fatalf("cursor shape = %d, want 5", a.CursorShape())
	}
	if snap := a.Snapshot(false, 0); !bytes.Contains(snap, []byte("\x1b[5 q")) {
		t.Fatalf("snapshot did not restore cursor shape: %q", snap)
	}
}

// ---- DECALN homes the cursor ----------------------------------------------

func TestDECALNHomesCursor(t *testing.T) {
	a := New(20, 5, 0, nil)
	a.Write([]byte("\x1b[5;3H\x1b#8"))
	a.State.lock()
	defer a.State.unlock()
	if a.cur.X != 0 || a.cur.Y != 0 {
		t.Fatalf("DECALN left cursor at (%d,%d), want (0,0)", a.cur.X, a.cur.Y)
	}
	if a.lines[2][2].Char != 'E' {
		t.Fatal("DECALN did not fill the screen")
	}
}

// ---- OSC empty title clears it --------------------------------------------

func TestEmptyTitleClears(t *testing.T) {
	a := New(20, 3, 0, nil)
	a.Write([]byte("\x1b]0;hello\a"))
	if a.TitleSnapshot() != "hello" {
		t.Fatalf("title = %q, want hello", a.TitleSnapshot())
	}
	a.Write([]byte("\x1b]0;\a"))
	if got := a.TitleSnapshot(); got != "" {
		t.Fatalf("empty OSC 0 left title = %q, want cleared", got)
	}
}

// ---- snapshot round-trip for the new attributes ---------------------------

func TestNewAttrsRoundtrip(t *testing.T) {
	roundtrip(t, "strike", []byte("\x1b[9mcrossed\x1b[29m plain"), nil)
	roundtrip(t, "conceal", []byte("\x1b[8mhidden\x1b[28m shown"), nil)
	roundtrip(t, "overline", []byte("\x1b[53mover\x1b[55m plain"), nil)
	roundtrip(t, "colon-truecolor", []byte("\x1b[38:2:10:20:30mrgb\x1b[0m"), nil)
}
