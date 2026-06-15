// tide extension to the vt10x port: scrollback history.

package vt

// pushHistory copies a line into the fixed-capacity history ring. Called
// from scrollUp for full-screen scrolls on the main screen; the alt screen
// and partial scroll regions never feed history.
func (t *State) pushHistory(l line) {
	if cap(t.history) == 0 {
		return
	}
	cp := make(line, len(l))
	copy(cp, l)
	if t.historyCount < cap(t.history) {
		t.history = t.history[:t.historyCount+1]
		t.history[t.historyCount] = cp
		t.historyCount++
		return
	}
	t.history[t.historyStart] = cp
	t.historyStart = (t.historyStart + 1) % cap(t.history)
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
}
