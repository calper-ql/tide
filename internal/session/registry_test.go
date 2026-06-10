package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckpointRoundtrip(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "sessions.json")
	r := NewRegistry(statePath)

	s1, created, err := r.Ensure("/proj/a")
	if err != nil || !created {
		t.Fatalf("first Ensure: %+v %v %v", s1, created, err)
	}
	if _, created, _ := r.Ensure("/proj/a"); created {
		t.Fatal("second Ensure of the same root must not create")
	}
	if _, _, err := r.Ensure("/proj/b"); err != nil {
		t.Fatal(err)
	}

	fresh := NewRegistry(statePath)
	if q, err := fresh.Load(); err != nil || q != "" {
		t.Fatalf("Load: quarantined=%q err=%v", q, err)
	}
	list := fresh.List()
	if len(list) != 2 || list[0].Root != "/proj/a" || list[1].Root != "/proj/b" {
		t.Fatalf("recovered = %+v", list)
	}
	if !list[0].CreatedAt.Equal(s1.CreatedAt) {
		t.Fatalf("CreatedAt not preserved: %v != %v", list[0].CreatedAt, s1.CreatedAt)
	}
}

func TestCorruptStateIsQuarantinedNotFatal(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "sessions.json")
	if err := os.WriteFile(statePath, []byte("{torn write"), 0o600); err != nil {
		t.Fatal(err)
	}

	r := NewRegistry(statePath)
	quarantined, err := r.Load()
	if err != nil {
		t.Fatalf("a corrupt checkpoint must not brick the daemon: %v", err)
	}
	if !strings.HasPrefix(quarantined, statePath+".corrupt-") {
		t.Fatalf("quarantined = %q", quarantined)
	}
	data, err := os.ReadFile(quarantined)
	if err != nil || string(data) != "{torn write" {
		t.Fatalf("quarantine must preserve the bytes: %q %v", data, err)
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatal("corrupt file must be moved aside, not left in place")
	}
	if r.Len() != 0 {
		t.Fatal("registry must start empty after quarantine")
	}
	if _, _, err := r.Ensure("/proj/recovered"); err != nil {
		t.Fatalf("registry must be usable after quarantine: %v", err)
	}
}

func TestKillUnknownRootIsNotAnError(t *testing.T) {
	r := NewRegistry(filepath.Join(t.TempDir(), "sessions.json"))
	killed, err := r.Kill("/no/such", nil)
	if err != nil || killed {
		t.Fatalf("killed=%v err=%v", killed, err)
	}
}

func TestPaneContentRoundtripAndKillCleanup(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "sessions.json")
	r := NewRegistry(statePath)
	if _, _, err := r.Ensure("/proj/x"); err != nil {
		t.Fatal(err)
	}
	pc := PaneContent{Cols: 80, Rows: 24, Snapshot: []byte("snap"), History: [][]byte{[]byte("h1")}}
	if err := r.UpdatePaneContent("/proj/x", "pane-abc", pc); err != nil {
		t.Fatal(err)
	}
	if err := r.UpdateLayout("/proj/x", []byte(`{"tabs":[]}`)); err != nil {
		t.Fatal(err)
	}

	fresh := NewRegistry(statePath)
	if _, err := fresh.Load(); err != nil {
		t.Fatal(err)
	}
	s, ok := fresh.Get("/proj/x")
	var lay struct {
		Tabs []any `json:"tabs"`
	}
	if !ok || json.Unmarshal(s.Layout, &lay) != nil || lay.Tabs == nil {
		t.Fatalf("layout not persisted: %+v", s)
	}
	got, ok := fresh.LoadPaneContent("pane-abc")
	if !ok || string(got.Snapshot) != "snap" || got.Cols != 80 || len(got.History) != 1 {
		t.Fatalf("pane content = %+v ok=%v", got, ok)
	}

	// Kill removes the pane files with the session.
	if killed, err := fresh.Kill("/proj/x", []string{"pane-abc"}); err != nil || !killed {
		t.Fatalf("kill: %v %v", killed, err)
	}
	if _, ok := fresh.LoadPaneContent("pane-abc"); ok {
		t.Fatal("pane content must die with its session")
	}
}
