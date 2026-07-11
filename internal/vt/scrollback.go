// tide extension to the vt10x port: scrollback history.

package vt

// pushHistory copies a line into the fixed-capacity history ring. Called
// from scrollUp for full-screen scrolls on the main screen; the alt screen
// and partial scroll regions never feed history. The append slot is
// start+count in ring space: after popHistory a wrapped ring is not full
// yet does not start at zero, so a raw historyCount index would clobber a
// live line (and shrinking the slice would send historyLine out of range).
func (t *State) pushHistory(l line) {
	if cap(t.history) == 0 {
		return
	}
	cp := make(line, len(l))
	copy(cp, l)
	t.histScrolled++
	if t.historyCount < cap(t.history) {
		idx := (t.historyStart + t.historyCount) % cap(t.history)
		if idx >= len(t.history) {
			t.history = t.history[:idx+1]
		}
		t.history[idx] = cp
		t.historyCount++
		return
	}
	t.history[t.historyStart] = cp
	t.historyStart = (t.historyStart + 1) % cap(t.history)
}

// popHistory drops the n newest history lines — resize growth pulls them
// back onto the screen. Only the count shrinks; the freed ring slots are
// reused by later pushes.
func (t *State) popHistory(n int) {
	t.historyCount -= min(n, t.historyCount)
}

// historyLine returns the i-th oldest history line; 0 is the oldest.
func (t *State) historyLine(i int) line {
	return t.history[(t.historyStart+i)%cap(t.history)]
}

// HistoryLen returns the number of scrollback lines held. Callers must hold
// the state lock (same convention as Cell/Cursor).
func (t *State) HistoryLen() int {
	return t.historyCount
}

// clearHistory drops all scrollback (ED 3 / CSI 3 J). The visible screen is
// untouched.
func (t *State) clearHistory() {
	t.history = t.history[:0]
	t.historyStart = 0
	t.historyCount = 0
	t.histScrolled = 0
}
