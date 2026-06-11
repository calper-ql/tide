// tide extension to the vt10x port: the Term wrapper is tide's API surface.

package vt

import (
	"io"
	"reflect"
	"unicode/utf8"
)

// Term is a virtual terminal: a screen grid, scrollback history, and the
// parser state needed to host a live PTY. All methods lock internally.
type Term struct {
	*State
	pending []byte // incomplete trailing UTF-8 sequence between Writes
}

// New returns a Term of the given size. answerback receives terminal query
// responses (DSR/CPR) and should be the PTY master; nil discards them.
// historyMax bounds the scrollback ring.
func New(cols, rows, historyMax int, answerback io.Writer) *Term {
	if answerback == nil {
		answerback = io.Discard
	}
	s := newState(answerback)
	s.history = make([]line, 0, max(historyMax, 0))
	s.numlock = true
	s.state = s.parse
	s.groundPC = reflect.ValueOf(s.parse).Pointer()
	s.cur.Attr.FG = DefaultFG
	s.cur.Attr.BG = DefaultBG
	s.resize(cols, rows)
	s.reset()
	return &Term{State: s}
}

// Write parses PTY output into the grid. Unlike upstream vt10x it is safe
// against UTF-8 runes split across PTY reads: an incomplete trailing
// sequence is held back and completed by the next Write.
func (t *Term) Write(p []byte) (int, error) {
	t.State.lock()
	defer t.State.unlock()
	buf := p
	if len(t.pending) > 0 {
		buf = append(t.pending, p...)
		t.pending = nil
	}
	for len(buf) > 0 {
		if !utf8.FullRune(buf) {
			// at most utf8.UTFMax-1 bytes of a rune still in flight
			t.pending = append(t.pending, buf...)
			break
		}
		r, sz := utf8.DecodeRune(buf)
		t.put(r)
		buf = buf[sz:]
	}
	return len(p), nil
}

// Resize changes the grid size.
func (t *Term) Resize(cols, rows int) {
	t.State.lock()
	defer t.State.unlock()
	t.State.resize(cols, rows)
}

// WithLock runs f while holding the state lock, for callers that need a
// consistent multi-read view (e.g. tests comparing grids).
func (t *Term) WithLock(f func(*State)) {
	t.State.lock()
	defer t.State.unlock()
	f(t.State)
}

// DrainClips returns any clipboard events queued by OSC 52 sequences
// during the most recent Write(s), clearing the queue. Called by the pane
// after each Write so clipboard requests from inner programs (e.g.
// bubbletea's tea.SetClipboard) reach the client's native clipboard tool.
func (t *Term) DrainClips() []ClipEvent {
	t.State.lock()
	defer t.State.unlock()
	clips := t.State.pendingClips
	t.State.pendingClips = nil
	return clips
}
