// User preferences are daemon-global (a theme is the user's, not a
// session's) and live beside sessions.json. Spec: config files are not a
// user-facing concept — prefs are written only by UI actions, and a
// missing or corrupt file silently falls back to defaults rather than
// bricking the daemon.
package session

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Prefs holds UI preferences chosen from inside tide.
type Prefs struct {
	Theme string `json:"theme,omitempty"`
}

// PrefsPath places prefs.json beside the registry's state file.
func PrefsPath(statePath string) string {
	return filepath.Join(filepath.Dir(statePath), "prefs.json")
}

// LoadPrefs reads prefs; any failure (missing, unreadable, corrupt) is a
// fresh default — losing a theme choice must never cost an attach.
func LoadPrefs(path string) Prefs {
	var p Prefs
	data, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(data, &p) != nil {
		return Prefs{}
	}
	return p
}

// SavePrefs checkpoints prefs atomically (temp + fsync + rename), same
// guarantees as the registry's state file.
func SavePrefs(path string, p Prefs) error {
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, data)
}
