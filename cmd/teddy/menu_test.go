package main

import "testing"

func TestMenuActions(t *testing.T) {
	// No document: Save is present but disabled; Quit always there.
	a := &App{}
	var hasQuit bool
	var saveEnabled = true
	for _, it := range a.menuActions() {
		switch it.label {
		case "Quit":
			hasQuit = true
		case "Save":
			saveEnabled = it.enabled
		}
	}
	if !hasQuit {
		t.Error("menu missing Quit")
	}
	if saveEnabled {
		t.Error("Save should be disabled with no open document")
	}

	// An unmodified doc keeps Save disabled; editing enables it.
	a.tabs = []*doc{newDoc("f.txt", []byte("hi"))}
	if menuSaveEnabled(a) {
		t.Error("Save should be disabled when the doc is unmodified")
	}
	a.tabs[0].cx = 2
	a.tabs[0].insertString("!")
	if !menuSaveEnabled(a) {
		t.Error("Save should be enabled after an edit")
	}
}

func menuSaveEnabled(a *App) bool {
	for _, it := range a.menuActions() {
		if it.label == "Save" {
			return it.enabled
		}
	}
	return false
}
