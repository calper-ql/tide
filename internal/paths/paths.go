// Package paths derives tide's user-private runtime and state locations
// (spec: socket security — 0700 dir, owner-only; no network listeners).
package paths

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// RuntimeDir returns the directory holding the daemon socket, lock, and
// log, creating it 0700 if needed. TIDE_RUNTIME_DIR overrides it for tests
// and unusual setups.
func RuntimeDir() (string, error) {
	dir := os.Getenv("TIDE_RUNTIME_DIR")
	if dir == "" {
		if xdg := os.Getenv("XDG_RUNTIME_DIR"); xdg != "" {
			dir = filepath.Join(xdg, "tide")
		} else {
			// Fixed per-uid path, tmux-style — NOT os.TempDir(): $TMPDIR
			// differs between GUI and SSH logins on macOS, which would
			// split the central daemon into one per login context.
			dir = filepath.Join("/tmp", fmt.Sprintf("tide-%d", os.Getuid()))
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	// MkdirAll is a no-op on an existing dir: re-assert owner and mode so a
	// pre-created dir can't widen access to the socket.
	info, err := os.Stat(dir)
	if err != nil {
		return "", err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) != os.Getuid() {
		return "", fmt.Errorf("runtime dir %s is not owned by the current user", dir)
	}
	if info.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(dir, 0o700); err != nil {
			return "", err
		}
	}
	return dir, nil
}

func SocketPath(runtimeDir string) string { return filepath.Join(runtimeDir, "daemon.sock") }
func LockPath(runtimeDir string) string   { return filepath.Join(runtimeDir, "daemon.lock") }
func LogPath(runtimeDir string) string    { return filepath.Join(runtimeDir, "daemon.log") }

// StatePath returns the checkpoint file for the session registry (spec:
// daemon state serialization). TIDE_STATE_DIR overrides it for tests.
func StatePath() (string, error) {
	dir := os.Getenv("TIDE_STATE_DIR")
	if dir == "" {
		if xdg := os.Getenv("XDG_STATE_HOME"); xdg != "" {
			dir = filepath.Join(xdg, "tide")
		} else {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", err
			}
			dir = filepath.Join(home, ".local", "state", "tide")
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return filepath.Join(dir, "sessions.json"), nil
}
