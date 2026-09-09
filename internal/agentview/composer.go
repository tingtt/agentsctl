package agentview

import "strings"

// Composer is the shared prompt-editing state: a single stash shared
// across providers and directory scopes, held only in memory and
// discarded on exit (see the DesignDoc's Prompt stash section). It is
// deliberately its own type, independent of session selection, so
// multiline prompt editing (cursor movement, insertion) never needs to
// know about the session list.
type Composer struct {
	Prompt string
	Cursor int
	Stash  string

	// preferredColumn/hasPreferredColumn implement #14's multiline Up/Down
	// cursor navigation (CursorUp/CursorDown): the column a run of
	// consecutive vertical moves is trying to stay on, independent of how
	// short an intermediate line is -- the same "sticky column" behavior
	// as any text editor's arrow-key navigation. Any horizontal edit
	// (typing, Left/Right, Home/End, stash) invalidates it via
	// resetPreferredColumn, so the next vertical move starts fresh from
	// the cursor's actual column again.
	preferredColumn    int
	hasPreferredColumn bool
}

// InsertAtCursor splices text into Prompt at Cursor (a rune index) and
// advances the cursor past the inserted runes. Shared by ordinary
// character insertion and an embedded newline (Option+Enter/Shift+Enter)
// so both splice in exactly the same way -- not appended, and not treated
// as a separate line buffer.
func (c *Composer) InsertAtCursor(text string) {
	c.resetPreferredColumn()
	runes := []rune(c.Prompt)
	c.clampCursor(runes)
	insert := []rune(text)
	before := append([]rune(nil), runes[:c.Cursor]...)
	after := append([]rune(nil), runes[c.Cursor:]...)
	c.Prompt = string(append(append(before, insert...), after...))
	c.Cursor += len(insert)
}

func (c *Composer) Backspace() {
	c.resetPreferredColumn()
	r := []rune(c.Prompt)
	c.clampCursor(r)
	if c.Cursor > 0 {
		r = append(r[:c.Cursor-1], r[c.Cursor:]...)
		c.Cursor--
		c.Prompt = string(r)
	}
}

func (c *Composer) Delete() {
	c.resetPreferredColumn()
	r := []rune(c.Prompt)
	c.clampCursor(r)
	if c.Cursor < len(r) {
		r = append(r[:c.Cursor], r[c.Cursor+1:]...)
		c.Prompt = string(r)
	}
}

func (c *Composer) Home() { c.resetPreferredColumn(); c.Cursor = 0 }
func (c *Composer) End()  { c.resetPreferredColumn(); c.Cursor = len([]rune(c.Prompt)) }
func (c *Composer) Left() {
	c.resetPreferredColumn()
	c.Cursor = max(0, min(c.Cursor, len([]rune(c.Prompt)))-1)
}
func (c *Composer) Right() {
	c.resetPreferredColumn()
	c.Cursor = min(len([]rune(c.Prompt)), c.Cursor+1)
}

// ToggleStash swaps Prompt and Stash (Ctrl+S), a no-op if both are empty.
func (c *Composer) ToggleStash() {
	c.resetPreferredColumn()
	if c.Prompt != "" || c.Stash != "" {
		c.Prompt, c.Stash = c.Stash, c.Prompt
		c.Cursor = len([]rune(c.Prompt))
	}
}

// Clear resets the prompt after a successful dispatch.
func (c *Composer) Clear() {
	c.resetPreferredColumn()
	c.Prompt = ""
	c.Cursor = 0
}

// ReplacePrompt replaces the editable prompt without changing the shared
// stash, places the cursor at the end, and resets vertical-navigation state.
func (c *Composer) ReplacePrompt(prompt string) {
	c.resetPreferredColumn()
	c.Prompt = prompt
	c.Cursor = len([]rune(prompt))
}

func (c *Composer) clampCursor(runes []rune) {
	c.Cursor = min(max(c.Cursor, 0), len(runes))
}

func (c *Composer) resetPreferredColumn() { c.hasPreferredColumn = false }

// IsMultiline reports whether Prompt currently spans more than one
// logical line -- the condition that gives CursorUp/CursorDown priority
// over session-list navigation in State.Handle (see update.go's
// bindingNavigate case).
func (c Composer) IsMultiline() bool {
	for _, r := range c.Prompt {
		if r == '\n' {
			return true
		}
	}
	return false
}

// CursorUp moves the cursor to the previous logical line (see #14's
// multiline cursor navigation), preserving its preferred column across
// shorter intermediate lines. A no-op on the first logical line.
func (c *Composer) CursorUp() {
	lines := strings.Split(c.Prompt, "\n")
	line, col := promptCursorPosition(lines, c.Cursor)
	if line == 0 {
		return
	}
	c.moveToLine(lines, line-1, col)
}

// CursorDown moves the cursor to the next logical line, mirroring
// CursorUp. A no-op on the last logical line.
func (c *Composer) CursorDown() {
	lines := strings.Split(c.Prompt, "\n")
	line, col := promptCursorPosition(lines, c.Cursor)
	if line >= len(lines)-1 {
		return
	}
	c.moveToLine(lines, line+1, col)
}

// moveToLine moves the cursor onto lines[targetLine], at the tracked
// preferredColumn if a vertical move is already in progress, or at
// currentCol (the column the cursor is actually leaving) if this is the
// first vertical move in a new run -- then clamps to that line's own
// length (a shorter line's EOL) without losing the wider preferred column,
// so moving on to a longer line later restores it. This is the same
// "sticky column" behavior common to multiline text editors' arrow-key
// navigation.
func (c *Composer) moveToLine(lines []string, targetLine, currentCol int) {
	col := currentCol
	if c.hasPreferredColumn {
		col = c.preferredColumn
	}
	c.preferredColumn, c.hasPreferredColumn = col, true
	target := []rune(lines[targetLine])
	c.Cursor = lineStartOffset(lines, targetLine) + min(col, len(target))
}

// lineStartOffset returns the rune-index offset (into the full,
// newline-joined Prompt, matching promptCursorPosition's own indexing) of
// the first rune of lines[lineIndex].
func lineStartOffset(lines []string, lineIndex int) int {
	offset := 0
	for i := 0; i < lineIndex; i++ {
		offset += len([]rune(lines[i])) + 1 // +1 for the "\n" separator
	}
	return offset
}
