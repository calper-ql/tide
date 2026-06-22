package daemon

import (
	"os"
	"testing"
)

// TestBarShowsSessionHost: the session bar names the machine the session runs
// on (host:project), so a remote `tide -r` attach still tells you where you are.
func TestBarShowsSessionHost(t *testing.T) {
	_, _, s := newTestWS(t)
	host, _ := os.Hostname()
	if host == "" {
		t.Skip("no hostname")
	}
	s.waitFor(t, "host:project in the bar", func() bool { return s.contains(host + ":") })
}
