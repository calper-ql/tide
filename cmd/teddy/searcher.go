package main

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	maxSearchResults  = 1000
	maxSearchFileSize = 1 << 20 // 1 MiB — skip larger files
	searchTextCap     = 240     // cap a result line's stored width
)

// searchOpts is a query plus the VS Code-style match modifiers.
type searchOpts struct {
	query     string
	matchCase bool
	wholeWord bool
	regex     bool
}

// searchResult is one matching line.
type searchResult struct {
	path string // absolute
	line int    // 1-based
	col  int    // 1-based rune column of the match
	text string // the matching line (trimmed, capped)
}

// searchMsg carries a completed search back to the UI, tagged with the
// generation seq so stale results (from a superseded query) are dropped. err
// is set when the pattern itself is invalid (a bad regex).
type searchMsg struct {
	seq       int
	results   []searchResult
	truncated bool
	err       string
}

// buildMatcher compiles the options into a per-line matcher returning the
// 1-based rune column of the first match. Everything routes through regexp so
// case-folding, whole-word, and regex compose with correct byte offsets; a
// literal query is escaped with QuoteMeta.
func buildMatcher(opts searchOpts) (func(string) (int, bool), error) {
	pat := opts.query
	if !opts.regex {
		pat = regexp.QuoteMeta(pat)
	}
	if opts.wholeWord {
		pat = `\b` + pat + `\b`
	}
	if !opts.matchCase {
		pat = `(?i)` + pat
	}
	re, err := regexp.Compile(pat)
	if err != nil {
		return nil, err
	}
	return func(line string) (int, bool) {
		loc := re.FindStringIndex(line)
		if loc == nil {
			return 0, false
		}
		return len([]rune(line[:loc[0]])) + 1, true
	}, nil
}

// runSearch greps opts under root and sends the results on out tagged with
// seq. It skips .git, binaries (a NUL in the first KB), empty files, and files
// over maxSearchFileSize, stopping at maxSearchResults. ctx aborts it promptly.
func runSearch(ctx context.Context, root string, opts searchOpts, seq int, out chan<- searchMsg) {
	match, err := buildMatcher(opts)
	if err != nil {
		send(ctx, out, searchMsg{seq: seq, err: "invalid pattern"})
		return
	}

	var results []searchResult
	truncated := false
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return nil
		}
		if ctx.Err() != nil {
			return filepath.SkipAll
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil || info.Size() == 0 || info.Size() > maxSearchFileSize {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		if bytes.IndexByte(data[:min(len(data), 1024)], 0) >= 0 {
			return nil // binary
		}
		for i, line := range strings.Split(string(data), "\n") {
			if col, ok := match(line); ok {
				results = append(results, searchResult{path: path, line: i + 1, col: col, text: capText(line)})
				if len(results) >= maxSearchResults {
					truncated = true
					return filepath.SkipAll
				}
			}
		}
		return nil
	})

	send(ctx, out, searchMsg{seq: seq, results: results, truncated: truncated})
}

func send(ctx context.Context, out chan<- searchMsg, msg searchMsg) {
	select {
	case out <- msg:
	case <-ctx.Done():
	}
}

func capText(s string) string {
	s = strings.TrimRight(s, "\r")
	if r := []rune(s); len(r) > searchTextCap {
		return string(r[:searchTextCap])
	}
	return s
}
