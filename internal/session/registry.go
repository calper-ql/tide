// Package session is the daemon's session registry and its disk checkpoint
// (spec: daemon state serialization — daemon death recovers to the last
// checkpoint; sessions are never lost).
//
// Storage is split: sessions.json holds identities only (small, rewritten
// on create/kill), while each pane's content checkpoint lives in its own
// pane-<hash>.json (rewritten by that pane's debounce alone). A busy pane
// therefore never rewrites other sessions' content.
package session

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Session is the durable identity of one project's workspace plus its
// layout (tabs/splits as opaque JSON owned by internal/layout). Pane
// content checkpoints live in their own files; live processes cannot be
// checkpointed, their content can — which is what "recovery to last
// checkpoint" promises.
type Session struct {
	Root      string          `json:"root"`
	CreatedAt time.Time       `json:"created_at"`
	Layout    json.RawMessage `json:"layout,omitempty"`
}

// PaneContent is one pane's checkpointed content: a screen snapshot
// (self-contained ANSI) and rendered scrollback lines.
type PaneContent struct {
	Cols     int      `json:"cols,omitempty"`
	Rows     int      `json:"rows,omitempty"`
	Snapshot []byte   `json:"snapshot,omitempty"`
	History  [][]byte `json:"history,omitempty"`
}

// Registry owns all sessions and checkpoints every mutation atomically.
type Registry struct {
	mu        sync.Mutex
	statePath string
	sessions  map[string]Session
}

func NewRegistry(statePath string) *Registry {
	return &Registry{statePath: statePath, sessions: map[string]Session{}}
}

// paneFile maps a pane id to its content file. Pane ids are hex uuids the
// daemon generates, but they cross the protocol boundary, so they are
// hashed rather than trusted as path components.
func (r *Registry) paneFile(paneID string) string {
	sum := sha256.Sum256([]byte(paneID))
	return filepath.Join(filepath.Dir(r.statePath), fmt.Sprintf("pane-%x.json", sum[:8]))
}

// Load restores the last checkpoint; a missing file is a fresh start. A
// corrupt file must never brick the daemon: it is quarantined (renamed
// aside, bytes preserved for manual recovery) and the returned path lets
// the caller log it loudly. A corrupt pane file only costs that pane's
// content, never the session.
func (r *Registry) Load() (quarantined string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	data, err := os.ReadFile(r.statePath)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var list []Session
	if jsonErr := json.Unmarshal(data, &list); jsonErr != nil {
		q := fmt.Sprintf("%s.corrupt-%d", r.statePath, time.Now().Unix())
		if renameErr := os.Rename(r.statePath, q); renameErr != nil {
			return "", fmt.Errorf("corrupt state file %s (quarantine failed: %v): %w", r.statePath, renameErr, jsonErr)
		}
		return q, nil
	}
	for _, s := range list {
		r.sessions[s.Root] = s
	}
	return "", nil
}

// LoadPaneContent reads one pane's checkpoint; a missing or corrupt file
// only costs that pane's content, never the session.
func (r *Registry) LoadPaneContent(paneID string) (PaneContent, bool) {
	var pc PaneContent
	data, err := os.ReadFile(r.paneFile(paneID))
	if err != nil || json.Unmarshal(data, &pc) != nil {
		return PaneContent{}, false
	}
	return pc, true
}

// Ensure returns the session for root, creating it if needed. A creation
// that cannot be checkpointed does not happen at all.
func (r *Registry) Ensure(root string) (s Session, created bool, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.sessions[root]; ok {
		return s, false, nil
	}
	s = Session{Root: root, CreatedAt: time.Now().UTC()}
	r.sessions[root] = s
	if err := r.checkpointLocked(); err != nil {
		delete(r.sessions, root)
		return Session{}, false, err
	}
	return s, true, nil
}

// Kill removes root's session and the checkpoints of its panes. The prime
// rule lives here: this is the only removal path, and only explicit user
// actions reach it.
func (r *Registry) Kill(root string, paneIDs []string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sessions[root]
	if !ok {
		return false, nil
	}
	delete(r.sessions, root)
	if err := r.checkpointLocked(); err != nil {
		r.sessions[root] = s
		return false, err
	}
	for _, id := range paneIDs {
		_ = os.Remove(r.paneFile(id))
	}
	return true, nil
}

// RemovePaneContent deletes one pane's checkpoint (the pane was closed,
// its session lives on).
func (r *Registry) RemovePaneContent(paneID string) {
	_ = os.Remove(r.paneFile(paneID))
}

// SweepPaneFiles deletes pane-content files referenced by no live pane id —
// strays from crashes between a structural change and its cleanup. The
// caller must NOT sweep after a quarantine: the quarantined state still
// references those files, and they are the manual-recovery story.
func (r *Registry) SweepPaneFiles(activeIDs []string) {
	keep := map[string]bool{}
	for _, id := range activeIDs {
		keep[filepath.Base(r.paneFile(id))] = true
	}
	dir := filepath.Dir(r.statePath)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, "pane-") && strings.HasSuffix(name, ".json") && !keep[name] {
			_ = os.Remove(filepath.Join(dir, name))
		}
	}
}

// Get returns the session for root, if any.
func (r *Registry) Get(root string) (Session, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sessions[root]
	return s, ok
}

// UpdatePaneContent checkpoints a pane's content into its own file. A root
// with no session (e.g. killed while a checkpoint was pending) is a silent
// no-op.
func (r *Registry) UpdatePaneContent(root, paneID string, pc PaneContent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.sessions[root]; !ok {
		return nil
	}
	data, err := json.Marshal(pc)
	if err != nil {
		return err
	}
	return writeFileAtomic(r.paneFile(paneID), data)
}

// UpdateLayout persists a session's layout tree (opaque to this package).
// Layout changes are structural and rare, so they ride the identity file.
func (r *Registry) UpdateLayout(root string, layout json.RawMessage) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sessions[root]
	if !ok {
		return nil
	}
	s.Layout = layout
	r.sessions[root] = s
	return r.checkpointLocked()
}

// List returns all sessions sorted by root.
func (r *Registry) List() []Session {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Session, 0, len(r.sessions))
	for _, s := range r.sessions {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Root < out[j].Root })
	return out
}

func (r *Registry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.sessions)
}

// checkpointLocked atomically replaces the identity file (temp + rename),
// so a daemon death mid-write can never corrupt the previous checkpoint.
func (r *Registry) checkpointLocked() error {
	list := make([]Session, 0, len(r.sessions))
	for _, s := range r.sessions {
		list = append(list, s)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Root < list[j].Root })
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(r.statePath, data)
}

// writeFileAtomic writes via temp + fsync + rename + dir-sync: atomic
// against process death and durable against machine crashes (on darwin
// Go's Sync issues F_FULLFSYNC).
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op once renamed
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return nil
}
