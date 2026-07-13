package main

import "testing"

// The blank context line carries a leading space, exactly as git emits it.
const sampleDiff = "diff --git a/foo.go b/foo.go\n" +
	"index 111..222 100644\n" +
	"--- a/foo.go\n" +
	"+++ b/foo.go\n" +
	"@@ -1,4 +1,4 @@\n" +
	" package main\n" +
	"-import \"old\"\n" +
	"+import \"new\"\n" +
	" \n" +
	" func main() {}\n"

func TestParseDiffClassifiesLines(t *testing.T) {
	lines := parseDiff(sampleDiff)
	// hunk header + 4 context/change lines (blank context line included).
	if len(lines) != 6 {
		t.Fatalf("lines = %d, want 6: %+v", len(lines), lines)
	}
	if lines[0].kind != dlHunk {
		t.Errorf("line 0 = %v, want hunk header", lines[0].kind)
	}
	// package main (context, old 1 / new 1)
	if l := lines[1]; l.kind != dlContext || l.oldNum != 1 || l.newNum != 1 || l.text != "package main" {
		t.Errorf("line 1 = %+v, want context 1/1 'package main'", l)
	}
	// -import "old" (deletion, old line 2)
	if l := lines[2]; l.kind != dlDel || l.oldNum != 2 || l.text != `import "old"` {
		t.Errorf("line 2 = %+v, want del old 2", l)
	}
	// +import "new" (addition, new line 2)
	if l := lines[3]; l.kind != dlAdd || l.newNum != 2 || l.text != `import "new"` {
		t.Errorf("line 3 = %+v, want add new 2", l)
	}
	// blank context line preserved
	if l := lines[4]; l.kind != dlContext || l.text != "" {
		t.Errorf("line 4 = %+v, want empty context line", l)
	}
}

// A del immediately followed by an add pairs onto one side-by-side row.
func TestBuildSideRowsPairsChanges(t *testing.T) {
	rows := buildSideRows(parseDiff(sampleDiff))

	var paired *sideRow
	for i := range rows {
		if rows[i].lkind == dlDel && rows[i].rkind == dlAdd {
			paired = &rows[i]
			break
		}
	}
	if paired == nil {
		t.Fatalf("expected a del/add paired row, got %+v", rows)
	}
	if paired.ltext != `import "old"` || paired.rtext != `import "new"` {
		t.Errorf("paired row = %+v, want old|new imports", *paired)
	}
	if paired.lnum != 2 || paired.rnum != 2 {
		t.Errorf("paired nums = %d|%d, want 2|2", paired.lnum, paired.rnum)
	}
}

// An untracked file becomes an all-additions diff covering the whole file.
func TestUntrackedDiffIsAllAdditions(t *testing.T) {
	lines := untrackedDiff([]byte("one\ntwo\n"))
	if len(lines) != 3 || lines[0].kind != dlHunk {
		t.Fatalf("lines = %+v, want hunk + 2 additions", lines)
	}
	for i, l := range lines[1:] {
		if l.kind != dlAdd || l.newNum != i+1 {
			t.Errorf("line %d = %+v, want add newNum %d", i+1, l, i+1)
		}
	}
}
