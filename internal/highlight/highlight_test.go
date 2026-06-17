package highlight

import (
	"strings"
	"testing"
)

// reconstruct joins a Lines result back into source text: each line is its
// spans concatenated, lines joined by "\n".
func reconstruct(lines [][]Span) string {
	rows := make([]string, len(lines))
	for i, spans := range lines {
		var b strings.Builder
		for _, s := range spans {
			b.WriteString(s.Text)
		}
		rows[i] = b.String()
	}
	return strings.Join(rows, "\n")
}

func hasCat(spans []Span, c Category) bool {
	for _, s := range spans {
		if s.Cat == c {
			return true
		}
	}
	return false
}

func TestLinesRoundTrip(t *testing.T) {
	cases := []struct{ name, file, src string }{
		{"go", "main.go", "// hi\nfunc main() {\n\tx := \"s\" + 12\n}\n"},
		{"plain", "notes.txt", "just\nplain text\n"},
		{"empty", "empty.go", ""},
		{"trailing", "t.go", "package p\n"},
		{"no-trailing-newline", "n.go", "package p"},
		{"blank-lines", "b.md", "a\n\n\nb"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lines := Lines(tc.file, tc.src)
			// One line per newline, plus one: an editor's line count.
			want := strings.Count(tc.src, "\n") + 1
			if len(lines) != want {
				t.Errorf("line count = %d, want %d", len(lines), want)
			}
			if got := reconstruct(lines); got != tc.src {
				t.Errorf("round-trip mismatch:\n got %q\nwant %q", got, tc.src)
			}
		})
	}
}

func TestCategories(t *testing.T) {
	lines := Lines("main.go", "// c\nfunc f() string { return \"x\" }\n")
	if !hasCat(lines[0], CatComment) {
		t.Errorf("line 0 expected a comment span, got %+v", lines[0])
	}
	if !hasCat(lines[1], CatKeyword) {
		t.Errorf("line 1 expected a keyword span, got %+v", lines[1])
	}
	if !hasCat(lines[1], CatString) {
		t.Errorf("line 1 expected a string span, got %+v", lines[1])
	}
}

func TestFallbackPlainText(t *testing.T) {
	lines := Lines("unknown.xyzzy", "one\ntwo")
	if len(lines) != 2 {
		t.Fatalf("line count = %d, want 2", len(lines))
	}
	for i, spans := range lines {
		for _, s := range spans {
			if s.Cat != CatText {
				t.Errorf("line %d: unknown file type should be all CatText, got cat %d", i, s.Cat)
			}
		}
	}
}
