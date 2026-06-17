package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/calper-ql/tide/internal/input"
)

func TestRunSearchFindsMatchesSkipsGitAndBinary(t *testing.T) {
	dir := t.TempDir()
	write := func(rel string, b []byte) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("a.go", []byte("package main\nfunc hello() {}\n"))
	write(".git/config", []byte("hello in git\n"))                // .git must be skipped
	write("blob.bin", []byte{0, 1, 2, 'h', 'e', 'l', 'l', 'o'})   // binary must be skipped
	write("sub/notes.txt", []byte("nothing here\nHELLO again\n")) // case-insensitive

	ch := make(chan searchMsg, 1)
	runSearch(context.Background(), dir, "hello", 7, ch)
	msg := <-ch

	if msg.seq != 7 {
		t.Errorf("seq = %d, want 7", msg.seq)
	}
	if len(msg.results) != 2 {
		t.Fatalf("got %d results, want 2 (a.go + notes.txt): %+v", len(msg.results), msg.results)
	}
	byBase := map[string]searchResult{}
	for _, r := range msg.results {
		byBase[filepath.Base(r.path)] = r
	}
	if r, ok := byBase["a.go"]; !ok || r.line != 2 || r.col != 6 {
		t.Errorf("a.go match = %+v, want line 2 col 6", r)
	}
	if r, ok := byBase["notes.txt"]; !ok || r.line != 2 {
		t.Errorf("notes.txt match = %+v, want line 2 (case-insensitive)", r)
	}
}

func TestApplySearchIgnoresStale(t *testing.T) {
	a := &App{searchSeq: 5}
	a.applySearch(searchMsg{seq: 3, results: []searchResult{{path: "x"}}})
	if len(a.search.results) != 0 {
		t.Error("stale (superseded) results should be ignored")
	}
	a.applySearch(searchMsg{seq: 5, results: []searchResult{{path: "x"}}})
	if len(a.search.results) != 1 {
		t.Error("current-generation results should be applied")
	}
}

func TestHandleSearchKeyEditsQuery(t *testing.T) {
	a := &App{root: t.TempDir(), searchCh: make(chan searchMsg, 8)}
	key := func(r rune) { a.handleSearchKey(input.Event{Key: input.KeyRune, Rune: r}) }
	key('h')
	key('i')
	if a.search.query != "hi" {
		t.Errorf("query = %q, want \"hi\"", a.search.query)
	}
	a.handleSearchKey(input.Event{Key: input.KeyBackspace})
	if a.search.query != "h" {
		t.Errorf("after backspace query = %q, want \"h\"", a.search.query)
	}
}
