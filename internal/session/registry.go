// Package session is the daemon's session registry and its disk checkpoint
// (spec: daemon state serialization — daemon death recovers to the last
// checkpoint; sessions are never lost).
package session

import (
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

// Session is the durable identity of one project's workspace. PTYs,
// buffers, and layout join this struct as the foundation grows.
type Session struct {
	Root      string    `json:"root"`
	CreatedAt time.Time `json:"created_at"`
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

// Load restores the last checkpoint; a missing file is a fresh start. A
// corrupt file must never brick the daemon: it is quarantined (renamed
// aside, bytes preserved for manual recovery) and the returned path lets
// the caller log it loudly.
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

// Kill removes root's session. The prime rule lives here: this is the only
// removal path, and only explicit user actions reach it.
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
	return true, nil
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

// checkpointLocked atomically replaces the state file (temp + rename), so a
// daemon death mid-write can never corrupt the previous checkpoint.
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
	tmp, err := os.CreateTemp(filepath.Dir(r.statePath), ".sessions-*.json")
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
	// Sync before rename: without it a machine crash can persist the rename
	// while the data blocks are still unwritten, leaving a zero-length
	// checkpoint (on darwin Go's Sync issues F_FULLFSYNC).
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), r.statePath); err != nil {
		return err
	}
	// Best-effort directory sync so the rename itself survives a crash.
	if dir, err := os.Open(filepath.Dir(r.statePath)); err == nil {
		_ = dir.Sync()
		dir.Close()
	}
	return nil
}
