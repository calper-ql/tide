// Package version pins the binary and wire-protocol versions exchanged in
// the hello handshake (spec: version handshake first).
package version

const (
	// Binary is the human-facing version of this build.
	Binary = "0.1.0-dev"

	// Protocol is the wire-protocol version. Clients and daemons attach
	// only on an exact match; a mismatch prompts `tide restart`, never an
	// implicit kill. v2: composed render frames replaced the raw pane
	// output stream when chrome/layout landed.
	Protocol = 2
)
