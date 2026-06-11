// Native clipboard bridge. OSC 52 stays on the render stream (it is what
// works over SSH), but plenty of terminals silently discard it —
// Terminal.app has no support at all, VTE gained it late and gated — so
// copy frames are also piped into the platform's clipboard tool when one
// is installed. No tool found means OSC 52 is the only path, which is the
// pre-v3 behavior.
package main

import (
	"bytes"
	"os"
	"os/exec"
	"runtime"
	"sync"

	"github.com/calper-ql/tide/internal/protocol"
)

// clipboardTools maps a copy target to the argv that writes it, resolved
// once per process: tool availability does not change mid-session. A
// missing key means no native path for that target on this machine.
var clipboardTools = sync.OnceValue(resolveClipboardTools)

func resolveClipboardTools() map[string][]string {
	if runtime.GOOS == "darwin" {
		if _, err := exec.LookPath("pbcopy"); err == nil {
			// macOS has no PRIMARY selection; primary copies are OSC 52 only.
			return map[string][]string{protocol.CopyClipboard: {"pbcopy"}}
		}
		return nil
	}
	// Wayland first, by the session's own signal — an X11 tool under
	// Wayland only reaches XWayland clients.
	if os.Getenv("WAYLAND_DISPLAY") != "" {
		if _, err := exec.LookPath("wl-copy"); err == nil {
			return map[string][]string{
				protocol.CopyClipboard: {"wl-copy"},
				protocol.CopyPrimary:   {"wl-copy", "--primary"},
			}
		}
	}
	if _, err := exec.LookPath("xclip"); err == nil {
		return map[string][]string{
			protocol.CopyClipboard: {"xclip", "-in", "-selection", "clipboard"},
			protocol.CopyPrimary:   {"xclip", "-in", "-selection", "primary"},
		}
	}
	if _, err := exec.LookPath("xsel"); err == nil {
		return map[string][]string{
			protocol.CopyClipboard: {"xsel", "--input", "--clipboard"},
			protocol.CopyPrimary:   {"xsel", "--input", "--primary"},
		}
	}
	return nil
}

// writeNativeClipboard pipes text into the tool for the given target.
// Failures are deliberately silent: the OSC 52 escape already went out on
// the render stream, and there is no surface here to report to — stdout
// belongs to the composed frames.
func writeNativeClipboard(target string, text []byte) {
	argv := clipboardTools()[target]
	if len(argv) == 0 {
		return
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin = bytes.NewReader(text)
	_ = cmd.Run()
}
