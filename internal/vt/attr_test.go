package vt

import (
	"bytes"
	"testing"
)

// TestEraseDropsDisplayAttrsKeepsBG pins BCE semantics: an erase (EL/ED/ECH)
// fills with the current BACKGROUND color but no other graphic rendition.
// Underline/bold/italic/etc. must NOT bleed into erased cells — the bug that
// made lazygit paint a continuous underscore across every line's blank tail
// (it sets underline, then erases to end of line, relying on bce).
func TestEraseDropsDisplayAttrsKeepsBG(t *testing.T) {
	a := New(20, 3, 0, nil)
	// Underline on + blue background, write "UNDER", then erase to EOL.
	a.Write([]byte("\x1b[4;44mUNDER\x1b[K"))
	a.State.lock()
	defer a.State.unlock()

	// Real content keeps its attributes.
	if a.lines[0][0].Mode&attrUnderline == 0 {
		t.Fatal("written cell lost its underline")
	}
	// The erased tail keeps the background (bce) but nothing else.
	for x := 5; x < a.cols; x++ {
		g := a.lines[0][x]
		if g.Mode != 0 {
			t.Fatalf("erased cell %d has mode %09b, want 0 (erase must drop non-color attrs)", x, g.Mode)
		}
		if g.BG != Color(Blue) {
			t.Fatalf("erased cell %d lost the background color: got %v want Blue (bce)", x, g.BG)
		}
	}
}

// TestFaintAttributeParsedAndEmitted pins SGR 2 (faint/dim): it must survive
// parsing and round-trip through the renderer, so a client terminal dims it.
// Claude Code's ghost-suggestion text is faint; without this it rendered at
// full intensity inside tide.
func TestFaintAttributeParsedAndEmitted(t *testing.T) {
	a := New(20, 3, 0, nil)
	a.Write([]byte("\x1b[2mfaint"))
	a.State.lock()
	g := a.lines[0][0]
	a.State.unlock()

	var b bytes.Buffer
	appendSGR(&b, g)
	if got := b.String(); got != "\x1b[0;2m" {
		t.Fatalf("appendSGR(faint) = %q, want %q", got, "\x1b[0;2m")
	}
}

// TestNormalIntensityClearsFaint pins SGR 22 (normal intensity) clearing both
// bold and faint, and SGR 0 (reset) clearing faint too.
func TestNormalIntensityClearsFaint(t *testing.T) {
	check := func(name, stream string) {
		a := New(20, 3, 0, nil)
		a.Write([]byte(stream))
		a.State.lock()
		g := a.lines[0][0]
		a.State.unlock()
		var b bytes.Buffer
		appendSGR(&b, g)
		if got := b.String(); got != "\x1b[0m" {
			t.Fatalf("%s: appendSGR = %q, want %q (faint not cleared)", name, got, "\x1b[0m")
		}
	}
	check("sgr-22", "\x1b[2m\x1b[22mx")
	check("sgr-0", "\x1b[2m\x1b[0mx")
}
