package main

// The native clipboard tool (pbcopy/wl-copy/xclip/xsel) is the local fallback
// for terminals that drop OSC 52. These tests stand in a fake tool for the
// real one — the "graphical environment" boundary — and verify both the
// resolution order and that the copied bytes actually reach the tool's stdin.

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/calper-ql/tide/internal/protocol"
)

// writeFakeTool drops an executable that copies its stdin to the file named
// by $TIDE_TEST_CLIP_OUT — a stand-in for pbcopy/xclip the test can inspect.
func writeFakeTool(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\ncat > \"$TIDE_TEST_CLIP_OUT\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestWriteNativeClipboardPipesStdinToTool proves the copied text reaches the
// configured tool verbatim, bytes intact (including UTF-8 and newlines), and
// that a target with no tool is a silent no-op rather than a panic.
func TestWriteNativeClipboardPipesStdinToTool(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix clipboard tools")
	}
	dir := t.TempDir()
	tool := writeFakeTool(t, dir, "fakeclip")
	out := filepath.Join(dir, "clip.out")
	t.Setenv("TIDE_TEST_CLIP_OUT", out)

	orig := clipboardTools
	clipboardTools = func() map[string][]string {
		return map[string][]string{protocol.CopyClipboard: {tool}}
	}
	t.Cleanup(func() { clipboardTools = orig })

	const content = "clipboard payload ✓\nsecond line"
	writeNativeClipboard(protocol.CopyClipboard, []byte(content)) // synchronous (cmd.Run)

	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("tool did not receive stdin: %v", err)
	}
	if string(got) != content {
		t.Fatalf("tool received %q, want %q", got, content)
	}

	// No tool configured for primary → no-op, no panic, no file written.
	writeNativeClipboard(protocol.CopyPrimary, []byte("ignored"))
}

// TestResolveClipboardToolsLinuxOrder pins the Linux resolution: Wayland
// prefers wl-copy, otherwise xclip, then xsel, and no tool at all degrades to
// OSC-52-only (nil map).
func TestResolveClipboardToolsLinuxOrder(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux-specific clipboard resolution order")
	}
	mkPathDir := func(t *testing.T, tools ...string) string {
		dir := t.TempDir()
		for _, name := range tools {
			if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\ncat > /dev/null\n"), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}

	t.Run("wayland prefers wl-copy", func(t *testing.T) {
		t.Setenv("PATH", mkPathDir(t, "wl-copy", "xclip", "xsel"))
		t.Setenv("WAYLAND_DISPLAY", "wayland-0")
		got := resolveClipboardTools()
		if c := got[protocol.CopyClipboard]; len(c) != 1 || c[0] != "wl-copy" {
			t.Fatalf("clipboard = %v, want [wl-copy]", c)
		}
		if p := got[protocol.CopyPrimary]; len(p) != 2 || p[0] != "wl-copy" || p[1] != "--primary" {
			t.Fatalf("primary = %v, want [wl-copy --primary]", p)
		}
	})

	t.Run("x11 falls back to xclip", func(t *testing.T) {
		t.Setenv("PATH", mkPathDir(t, "xclip", "xsel"))
		t.Setenv("WAYLAND_DISPLAY", "")
		got := resolveClipboardTools()
		if c := got[protocol.CopyClipboard]; len(c) == 0 || c[0] != "xclip" {
			t.Fatalf("clipboard = %v, want xclip first", c)
		}
	})

	t.Run("xsel is last", func(t *testing.T) {
		t.Setenv("PATH", mkPathDir(t, "xsel"))
		t.Setenv("WAYLAND_DISPLAY", "")
		got := resolveClipboardTools()
		if c := got[protocol.CopyClipboard]; len(c) == 0 || c[0] != "xsel" {
			t.Fatalf("clipboard = %v, want xsel", c)
		}
	})

	t.Run("no tool degrades to OSC 52 only", func(t *testing.T) {
		t.Setenv("PATH", mkPathDir(t)) // empty dir
		t.Setenv("WAYLAND_DISPLAY", "")
		if got := resolveClipboardTools(); got != nil {
			t.Fatalf("resolve = %v, want nil (no native tool, OSC 52 only)", got)
		}
	})
}
