package main

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const (
	maxSearchResults  = 1000
	maxSearchFileSize = 1 << 20 // 1 MiB — skip larger files
	searchTextCap     = 240     // cap a result line's stored width
)

// searchResult is one matching line.
type searchResult struct {
	path string // absolute
	line int    // 1-based
	col  int    // 1-based rune column of the match
	text string // the matching line (trimmed, capped)
}

// searchMsg carries a completed search back to the UI, tagged with the
// generation seq so stale results (from a superseded query) are dropped.
type searchMsg struct {
	seq       int
	results   []searchResult
	truncated bool
}

// runSearch greps query (case-insensitive substring) under root and sends the
// results on out tagged with seq. It walks files, skipping .git, binary files
// (a NUL in the first KB), empty files, and files over maxSearchFileSize, and
// stops at maxSearchResults. ctx cancellation aborts it promptly.
func runSearch(ctx context.Context, root, query string, seq int, out chan<- searchMsg) {
	q := strings.ToLower(query)
	var results []searchResult
	truncated := false

	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
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
			idx := strings.Index(strings.ToLower(line), q)
			if idx < 0 {
				continue
			}
			results = append(results, searchResult{
				path: path,
				line: i + 1,
				col:  len([]rune(line[:idx])) + 1,
				text: capText(line),
			})
			if len(results) >= maxSearchResults {
				truncated = true
				return filepath.SkipAll
			}
		}
		return nil
	})

	select {
	case out <- searchMsg{seq: seq, results: results, truncated: truncated}:
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
