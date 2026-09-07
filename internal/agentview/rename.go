package agentview

import "github.com/tingtt/agentsctl/internal/session"

// Rename is the inline rename editor's state, active only while renaming
// one specific session (Target).
type Rename struct {
	Active   bool
	Target   session.Key
	Original string
	Draft    string
	Cursor   int
}

// Start begins renaming row, seeding the draft with its current name.
func (s *State) startRename(row session.Session) {
	s.Rename = Rename{Active: true, Target: row.Key, Original: row.Name, Draft: row.Name, Cursor: len([]rune(row.Name))}
	s.Error = ""
}

// Cancel discards the in-progress rename without applying it.
func (r *Rename) cancel() { *r = Rename{} }

func (r *Rename) insert(text string) {
	runes := []rune(r.Draft)
	r.clampCursor(runes)
	insert := []rune(text)
	before := append([]rune(nil), runes[:r.Cursor]...)
	after := append([]rune(nil), runes[r.Cursor:]...)
	r.Draft = string(append(append(before, insert...), after...))
	r.Cursor += len(insert)
}
func (r *Rename) backspace() {
	runes := []rune(r.Draft)
	if r.Cursor > 0 {
		runes = append(runes[:r.Cursor-1], runes[r.Cursor:]...)
		r.Cursor--
		r.Draft = string(runes)
	}
}
func (r *Rename) delete() {
	runes := []rune(r.Draft)
	if r.Cursor < len(runes) {
		runes = append(runes[:r.Cursor], runes[r.Cursor+1:]...)
		r.Draft = string(runes)
	}
}
func (r *Rename) home() { r.Cursor = 0 }
func (r *Rename) end()  { r.Cursor = len([]rune(r.Draft)) }
func (r *Rename) left() { r.Cursor = max(0, r.Cursor-1) }
func (r *Rename) right() {
	r.Cursor = min(len([]rune(r.Draft)), r.Cursor+1)
}
func (r *Rename) clampCursor(runes []rune) {
	r.Cursor = min(max(r.Cursor, 0), len(runes))
}
