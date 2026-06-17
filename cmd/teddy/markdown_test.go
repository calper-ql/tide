package main

import (
	"strings"
	"testing"
)

func mdText(cells []mdCell) string {
	var b strings.Builder
	for _, c := range cells {
		b.WriteRune(c.r)
	}
	return b.String()
}

func styleOfRune(cells []mdCell, r rune) (mdCell, bool) {
	for _, c := range cells {
		if c.r == r {
			return c, true
		}
	}
	return mdCell{}, false
}

func TestInlineCodeBoldLink(t *testing.T) {
	cells := inline("a `b` **c** [d](http://x)", stText)
	if got := mdText(cells); got != "a b c d" {
		t.Fatalf("text = %q, want \"a b c d\"", got)
	}
	if c, _ := styleOfRune(cells, 'b'); c.st != stMdCode {
		t.Errorf("`b` not code-styled")
	}
	if c, _ := styleOfRune(cells, 'c'); !c.st.Bold {
		t.Errorf("**c** not bold")
	}
	if c, _ := styleOfRune(cells, 'd'); c.st != stMdLink {
		t.Errorf("link text not link-styled")
	}
}

func TestInlineUnbalancedIsLiteral(t *testing.T) {
	// A lone backtick has no closing pair; it should render literally.
	cells := inline("a ` b", stText)
	if got := mdText(cells); got != "a ` b" {
		t.Errorf("text = %q, want \"a ` b\" (literal)", got)
	}
}

func TestRenderHeadingAndList(t *testing.T) {
	lines := renderMarkdown("# Title\n\n- one\n- two\n", 40)
	if mdText(lines[0]) != "Title" {
		t.Errorf("line 0 = %q, want \"Title\"", mdText(lines[0]))
	}
	if len(lines[0]) == 0 || !lines[0][0].st.Bold {
		t.Errorf("heading not bold")
	}
	bullets := 0
	for _, l := range lines {
		if strings.HasPrefix(mdText(l), "• ") {
			bullets++
		}
	}
	if bullets != 2 {
		t.Errorf("rendered %d bullets, want 2", bullets)
	}
}

func TestWrapCells(t *testing.T) {
	wrapped := wrapCells(inline("aa bb cc dd", stText), 5)
	if len(wrapped) != 2 {
		t.Fatalf("wrapped into %d lines, want 2: %q", len(wrapped), linesText(wrapped))
	}
	for i, wl := range wrapped {
		if cellsWidth(wl) > 5 {
			t.Errorf("line %d width %d exceeds 5", i, cellsWidth(wl))
		}
	}
}

func TestMarkdownClassifiers(t *testing.T) {
	if !isMarkdown("README.md") || isMarkdown("main.go") {
		t.Error("isMarkdown misclassified")
	}
	if headingLevel("## Two") != 2 || headingLevel("#nope") != 0 {
		t.Error("headingLevel wrong")
	}
	if !isRule("---") || !isRule("* * *") || isRule("--") {
		t.Error("isRule wrong")
	}
	if m, rest := listItem("- hi"); m != "• " || rest != "hi" {
		t.Errorf("listItem bullet = %q,%q", m, rest)
	}
	if m, rest := listItem("3. third"); m != "3. " || rest != "third" {
		t.Errorf("listItem ordered = %q,%q", m, rest)
	}
}

func linesText(lines [][]mdCell) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = mdText(l)
	}
	return out
}
