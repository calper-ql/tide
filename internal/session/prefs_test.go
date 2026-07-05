package session

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPrefsRoundtrip(t *testing.T) {
	path := PrefsPath(filepath.Join(t.TempDir(), "sessions.json"))
	if p := LoadPrefs(path); p.Theme != "" {
		t.Fatalf("missing prefs must load as defaults, got %+v", p)
	}
	if err := SavePrefs(path, Prefs{Theme: "ocean"}); err != nil {
		t.Fatal(err)
	}
	if p := LoadPrefs(path); p.Theme != "ocean" {
		t.Fatalf("Theme = %q, want ocean", p.Theme)
	}
}

func TestCorruptPrefsFallBackToDefaults(t *testing.T) {
	path := PrefsPath(filepath.Join(t.TempDir(), "sessions.json"))
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if p := LoadPrefs(path); p.Theme != "" {
		t.Fatalf("corrupt prefs must load as defaults, got %+v", p)
	}
}
