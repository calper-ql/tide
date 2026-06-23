package manage

import (
	"strings"
	"testing"

	"github.com/calper-ql/tide/internal/input"
)

func key(k input.Key) input.Event { return input.Event{Type: input.EvKey, Key: k} }
func runeKey(r rune) input.Event  { return input.Event{Type: input.EvKey, Key: input.KeyRune, Rune: r} }
func click(x, y int) input.Event {
	return input.Event{Type: input.EvMouse, Mouse: input.MousePress, X: x, Y: y}
}
func sess(root string) Session { return Session{Root: root, Panes: 1} }

func two() []Session { return []Session{sess("/a/proj"), sess("/b/work")} }

func TestManageRendersSessions(t *testing.T) {
	m := New(two(), 80, 30)
	f := string(m.Render())
	for _, want := range []string{"manage sessions (2)", "proj", "work", "/a/proj"} {
		if !strings.Contains(f, want) {
			t.Errorf("render missing %q", want)
		}
	}
}

// The safety invariant: selecting/opening never kills — a kill needs an
// explicit confirm AND an explicit 'y' (or the Kill button).
func TestManageKillNeedsConfirmThenYes(t *testing.T) {
	m := New(two(), 80, 30)
	// Clicking a row opens the confirmation; it must NOT kill yet.
	m.Handle(click(5, listTop)) // first session
	if !m.Confirming() {
		t.Fatal("clicking a session should open the confirmation")
	}
	if _, ok := m.TakeKill(); ok {
		t.Fatal("opening the confirmation must not request a kill")
	}
	// 'y' confirms the kill.
	m.Handle(runeKey('y'))
	root, ok := m.TakeKill()
	if !ok || root != "/a/proj" {
		t.Fatalf("after 'y', TakeKill = %q,%v; want /a/proj,true", root, ok)
	}
	if m.Confirming() {
		t.Fatal("confirmation should close after killing")
	}
}

func TestManageConfirmCancelPaths(t *testing.T) {
	for _, c := range []struct {
		name string
		ev   input.Event
	}{
		{"n", runeKey('n')},
		{"Esc", key(input.KeyEscape)},
		{"Enter", key(input.KeyEnter)}, // Enter is NOT confirm — safe default
		{"q", runeKey('q')},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := New(two(), 80, 30)
			m.Handle(key(input.KeyEnter)) // open confirm on the selected row
			if !m.Confirming() {
				t.Fatal("setup: confirmation should be open")
			}
			m.Handle(c.ev)
			if _, ok := m.TakeKill(); ok {
				t.Fatalf("%s must cancel, not kill", c.name)
			}
			if m.Confirming() {
				t.Fatalf("%s should close the confirmation", c.name)
			}
		})
	}
}

func TestManageClickKillButtonKills(t *testing.T) {
	m := New(two(), 80, 30)
	m.Handle(key(input.KeyDown))  // select second session
	m.Handle(key(input.KeyEnter)) // open confirm
	_ = m.Render()                // populates the Kill/Cancel button rects
	// Click within the Kill button on the confirmation row.
	m.Handle(click(m.killBtnX+1, m.rows-1))
	if root, ok := m.TakeKill(); !ok || root != "/b/work" {
		t.Fatalf("clicking Kill = %q,%v; want /b/work,true", root, ok)
	}
}

func TestManageClickElsewhereDuringConfirmCancels(t *testing.T) {
	m := New(two(), 80, 30)
	m.Handle(key(input.KeyEnter))
	_ = m.Render()
	m.Handle(click(0, m.rows-1)) // far from the Kill button
	if _, ok := m.TakeKill(); ok {
		t.Fatal("clicking away from Kill must cancel")
	}
	if m.Confirming() {
		t.Fatal("confirmation should close")
	}
}

func TestManageQuit(t *testing.T) {
	m := New(two(), 80, 30)
	m.Handle(runeKey('q'))
	if !m.Quit() {
		t.Fatal("q should quit")
	}
}

func TestManageSetSessionsClampsSelection(t *testing.T) {
	m := New(two(), 80, 30)
	m.Handle(key(input.KeyDown)) // hover the second (index 1)
	m.SetSessions([]Session{sess("/a/proj")})
	if m.hover != 0 {
		t.Fatalf("hover = %d after shrink, want clamped to 0", m.hover)
	}
	m.SetSessions(nil)
	if m.hover != -1 {
		t.Fatalf("hover = %d with no sessions, want -1", m.hover)
	}
}

func TestManageEmptyShowsNotice(t *testing.T) {
	m := New(nil, 80, 30)
	if !strings.Contains(string(m.Render()), "no live sessions") {
		t.Error("empty manager should show the no-sessions notice")
	}
}
