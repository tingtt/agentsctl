package agentview

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
}

// InsertAtCursor splices text into Prompt at Cursor (a rune index) and
// advances the cursor past the inserted runes. Shared by ordinary
// character insertion and an embedded newline (Option+Enter/Shift+Enter)
// so both splice in exactly the same way -- not appended, and not treated
// as a separate line buffer.
func (c *Composer) InsertAtCursor(text string) {
	runes := []rune(c.Prompt)
	c.clampCursor(runes)
	insert := []rune(text)
	before := append([]rune(nil), runes[:c.Cursor]...)
	after := append([]rune(nil), runes[c.Cursor:]...)
	c.Prompt = string(append(append(before, insert...), after...))
	c.Cursor += len(insert)
}

func (c *Composer) Backspace() {
	r := []rune(c.Prompt)
	c.clampCursor(r)
	if c.Cursor > 0 {
		r = append(r[:c.Cursor-1], r[c.Cursor:]...)
		c.Cursor--
		c.Prompt = string(r)
	}
}

func (c *Composer) Delete() {
	r := []rune(c.Prompt)
	c.clampCursor(r)
	if c.Cursor < len(r) {
		r = append(r[:c.Cursor], r[c.Cursor+1:]...)
		c.Prompt = string(r)
	}
}

func (c *Composer) Home() { c.Cursor = 0 }
func (c *Composer) End()  { c.Cursor = len([]rune(c.Prompt)) }
func (c *Composer) Left() { c.Cursor = max(0, min(c.Cursor, len([]rune(c.Prompt)))-1) }
func (c *Composer) Right() {
	c.Cursor = min(len([]rune(c.Prompt)), c.Cursor+1)
}

// ToggleStash swaps Prompt and Stash (Ctrl+S), a no-op if both are empty.
func (c *Composer) ToggleStash() {
	if c.Prompt != "" || c.Stash != "" {
		c.Prompt, c.Stash = c.Stash, c.Prompt
		c.Cursor = len([]rune(c.Prompt))
	}
}

// Clear resets the prompt after a successful dispatch.
func (c *Composer) Clear() {
	c.Prompt = ""
	c.Cursor = 0
}

func (c *Composer) clampCursor(runes []rune) {
	c.Cursor = min(max(c.Cursor, 0), len(runes))
}

// IsMultiline reports whether Prompt currently spans more than one
// logical line, the condition that (per the DesignDoc/#14) will someday
// give Up/Down cursor-movement priority over session-list navigation.
// Not wired to that behavior yet (#14 is out of scope for this refactor),
// but exposed now so update.go's key routing has a single, correct place
// to add that priority without touching composer internals.
func (c Composer) IsMultiline() bool {
	for _, r := range c.Prompt {
		if r == '\n' {
			return true
		}
	}
	return false
}
