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
	"sync"
	"time"
)

// Session is the durable identity of one project's workspace, plus the
// last checkpoint of its pane: a screen snapshot (self-contained ANSI) and
// rendered scrollback lines. Live processes cannot be checkpointed; the
// content can, which is what "recovery to last checkpoint" promises.
type Session struct {
	Root      string    `json:"root"`
	CreatedAt time.Time `json:"created_at"`
	PaneUUID  string    `json:"pane_uuid,omitempty"`
	Cols      int       `json:"cols,omitempty"`
	Rows      int       `json:"rows,omitempty"`
	Snapshot  []byte    `json:"snapshot,omitempty"`
	History   [][]byte  `json:"history,omitempty"`
}

// identity is the subset persisted in sessions.json.
type identity struct {
	Root      string    `json:"root"`
	CreatedAt time.Time `json:"created_at"`
}

// paneState is the subset persisted per pane.
type paneState struct {
	PaneUUID string   `json:"pane_uuid,omitempty"`
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

func (r *Registry) paneFile(root string) string {
	sum := sha256.Sum256([]byte(root))
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
	var list []identity
	if jsonErr := json.Unmarshal(data, &list); jsonErr != nil {
		q := fmt.Sprintf("%s.corrupt-%d", r.statePath, time.Now().Unix())
		if renameErr := os.Rename(r.statePath, q); renameErr != nil {
			return "", fmt.Errorf("corrupt state file %s (quarantine failed: %v): %w", r.statePath, renameErr, jsonErr)
		}
		return q, nil
	}
	for _, id := range list {
		s := Session{Root: id.Root, CreatedAt: id.CreatedAt}
		if pdata, err := os.ReadFile(r.paneFile(id.Root)); err == nil {
			var ps paneState
			if json.Unmarshal(pdata, &ps) == nil {
				s.PaneUUID, s.Cols, s.Rows, s.Snapshot, s.History = ps.PaneUUID, ps.Cols, ps.Rows, ps.Snapshot, ps.History
			}
		}
		r.sessions[s.Root] = s
	}
	return "", nil
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

// Kill removes root's session and its pane checkpoint. The prime rule
// lives here: this is the only removal path, and only explicit user
// actions reach it.
func (r *Registry) Kill(root string) (bool, error) {
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
	_ = os.Remove(r.paneFile(root))
	return true, nil
}

// Get returns the session for root, if any.
func (r *Registry) Get(root string) (Session, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sessions[root]
	return s, ok
}

// UpdatePane checkpoints a pane's content into its own file. A root with no
// session (e.g. killed while a checkpoint was pending) is a silent no-op.
func (r *Registry) UpdatePane(root, uuid string, cols, rows int, snapshot []byte, history [][]byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sessions[root]
	if !ok {
		return nil
	}
	s.PaneUUID, s.Cols, s.Rows, s.Snapshot, s.History = uuid, cols, rows, snapshot, history
	r.sessions[root] = s
	data, err := json.Marshal(paneState{PaneUUID: uuid, Cols: cols, Rows: rows, Snapshot: snapshot, History: history})
	if err != nil {
		return err
	}
	return writeFileAtomic(r.paneFile(root), data)
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
	list := make([]identity, 0, len(r.sessions))
	for _, s := range r.sessions {
		list = append(list, identity{Root: s.Root, CreatedAt: s.CreatedAt})
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
