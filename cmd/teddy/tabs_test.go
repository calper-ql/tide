package main

import (
	"path/filepath"
	"testing"
)

func TestOpenFileFocusesExisting(t *testing.T) {
	dir := t.TempDir()
	f1 := filepath.Join(dir, "a.txt")
	f2 := filepath.Join(dir, "b.txt")
	mustWrite(t, f1)
	mustWrite(t, f2)

	a := &App{active: 0, dragFrom: -1, pressClose: -1}
	_ = a.openFile(f1)
	_ = a.openFile(f2)
	if len(a.tabs) != 2 {
		t.Fatalf("tabs = %d, want 2", len(a.tabs))
	}
	_ = a.openFile(f1) // re-open: focus, not a new tab
	if len(a.tabs) != 2 {
		t.Errorf("re-open created a duplicate tab: %d tabs", len(a.tabs))
	}
	if a.active != 0 {
		t.Errorf("active = %d, want 0 (focused a.txt)", a.active)
	}
}

func TestCloseTabShiftsActive(t *testing.T) {
	a := &App{tabs: []*doc{newDoc("a", nil), newDoc("b", nil), newDoc("c", nil)}, active: 2}
	a.closeTab(0) // removed before active → active follows its tab
	if len(a.tabs) != 2 || a.active != 1 || a.tabs[a.active].path != "c" {
		t.Errorf("after closing 0: active=%d tab=%q", a.active, a.tabs[a.active].path)
	}
}

func TestCloseActiveLastTabClamps(t *testing.T) {
	a := &App{tabs: []*doc{newDoc("a", nil), newDoc("b", nil)}, active: 1}
	a.closeTab(1)
	if len(a.tabs) != 1 || a.active != 0 {
		t.Errorf("after closing the active last tab: active=%d len=%d", a.active, len(a.tabs))
	}
}

func TestMoveTab(t *testing.T) {
	mk := func() *App {
		return &App{tabs: []*doc{newDoc("a", nil), newDoc("b", nil), newDoc("c", nil)}}
	}
	check := func(t *testing.T, a *App, want ...string) {
		t.Helper()
		for i, w := range want {
			if a.tabs[i].path != w {
				t.Errorf("order = %v at %d, want %v", paths(a), i, want)
				return
			}
		}
	}
	a := mk()
	a.moveTab(0, 2)
	check(t, a, "b", "c", "a")

	a = mk()
	a.moveTab(2, 0)
	check(t, a, "c", "a", "b")
}

func TestTabLabelsDisambiguate(t *testing.T) {
	tabs := []*doc{
		newDoc("/x/pkg/main.go", nil),
		newDoc("/x/cmd/main.go", nil),
		newDoc("/x/util.go", nil),
	}
	got := tabLabels(tabs)
	want := []string{"pkg/main.go", "cmd/main.go", "util.go"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("label[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func paths(a *App) []string {
	out := make([]string, len(a.tabs))
	for i, d := range a.tabs {
		out[i] = d.path
	}
	return out
}
