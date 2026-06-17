// Package highlight wraps chroma as a lexer only. It tokenizes source into
// per-line spans tagged with a small, palette-independent Category; teddy
// maps those categories onto its own 16-color styles, so highlighting obeys
// tide's theme rather than chroma's formatter. chroma (alecthomas/chroma
// v2, MIT) is vendored and used solely for its lexers — never its styles or
// terminal formatters.
package highlight

import (
	"strings"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/lexers"
)

// Category is teddy's coarse token class. It is deliberately small: enough
// to color code legibly with the terminal's 16-color palette, no finer.
type Category int

const (
	CatText        Category = iota // default foreground
	CatKeyword                     // language keywords
	CatName                        // identifiers, function/variable names
	CatBuiltin                     // builtins (len, print, true, …)
	CatType                        // type names, classes, namespaces
	CatString                      // string literals
	CatNumber                      // numeric literals
	CatComment                     // comments
	CatOperator                    // operators
	CatPunctuation                 // brackets, separators
	CatError                       // lexer error tokens
)

// Span is a run of text within a single line sharing one category. Text
// never contains a newline.
type Span struct {
	Text string
	Cat  Category
}

// Lines tokenizes source — the lexer chosen from filename, falling back to
// plain text — into per-line spans. The result always has exactly one more
// line than source has newlines (a trailing newline yields a final empty
// line), and concatenating each line's span text, joined by "\n", restores
// source exactly: highlighting never loses or invents a byte.
func Lines(filename, source string) [][]Span {
	lexer := lexers.Match(filename)
	if lexer == nil {
		lexer = lexers.Fallback // plain text
	}
	it, err := chroma.Coalesce(lexer).Tokenise(nil, source)
	if err != nil {
		return rawLines(source)
	}

	var lines [][]Span
	cur := []Span{}
	for _, tok := range it.Tokens() {
		cat := categoryOf(tok.Type)
		// A token's value can straddle line breaks; split it so every span
		// stays within one rendered line.
		parts := strings.Split(tok.Value, "\n")
		for i, part := range parts {
			if i > 0 {
				lines = append(lines, cur)
				cur = []Span{}
			}
			if part != "" {
				cur = append(cur, Span{Text: part, Cat: cat})
			}
		}
	}
	return append(lines, cur)
}

// rawLines is the no-highlight fallback: every line one plain span.
func rawLines(source string) [][]Span {
	parts := strings.Split(source, "\n")
	out := make([][]Span, len(parts))
	for i, p := range parts {
		if p != "" {
			out[i] = []Span{{Text: p, Cat: CatText}}
		} else {
			out[i] = []Span{}
		}
	}
	return out
}

// categoryOf collapses chroma's fine-grained token hierarchy onto teddy's
// coarse categories. It keys off Category()/SubCategory() (the /1000 and
// /100 rounding chroma defines) so subtypes ride along with their parent.
func categoryOf(tt chroma.TokenType) Category {
	if tt == chroma.Error {
		return CatError
	}
	switch tt.Category() {
	case chroma.Keyword:
		if tt == chroma.KeywordType {
			return CatType
		}
		return CatKeyword
	case chroma.Name:
		switch tt {
		case chroma.NameBuiltin, chroma.NameBuiltinPseudo:
			return CatBuiltin
		case chroma.NameClass, chroma.NameNamespace, chroma.NameException:
			return CatType
		}
		return CatName
	case chroma.Literal:
		switch tt.SubCategory() {
		case chroma.LiteralString:
			return CatString
		case chroma.LiteralNumber:
			return CatNumber
		}
		return CatText
	case chroma.Comment:
		return CatComment
	case chroma.Operator:
		return CatOperator
	case chroma.Punctuation:
		return CatPunctuation
	}
	return CatText
}
