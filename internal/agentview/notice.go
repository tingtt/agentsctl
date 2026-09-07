package agentview

import "github.com/tingtt/agentsctl/internal/session"

// Severity is a row-scoped notice's color/urgency.
type Severity int

const (
	SeverityInfo Severity = iota
	SeverityWarning
	SeverityAlert
)

// confirmationText maps a pending confirmation's action to its armed
// warning message and severity. Archive is the only one wired today;
// adding a future action (e.g. Issue #5's Share) means adding one more
// case here, not a new confirmation mechanism -- see PendingConfirmation.
func confirmationText(action session.ActionID) (message string, severity Severity) {
	switch action {
	case session.ActionArchive:
		return "Press Ctrl+X again to archive", SeverityAlert
	default:
		return "Press again to confirm", SeverityAlert
	}
}

// PendingConfirmation is a session-scoped, two-press confirmation gate
// (armed by a first key press, resolved or cancelled by a second) --
// generalized over which action it confirms rather than one bool per
// action, so a future action (Issue #5's Share) reuses the same
// mechanism instead of Agent View accumulating one *bool per confirmable
// action. Only one confirmation can be pending at a time, matching the
// current UX (a second unrelated action implicitly cancels the first by
// re-arming for its own target).
type PendingConfirmation struct {
	Key    session.Key
	Action session.ActionID
}

// RowNotice is the resolved, renderable form of a pending confirmation
// (or, in the future, a non-confirmation informational notice such as
// Issue #5's "Public link copied.") for one session row.
type RowNotice struct {
	Message  string
	Severity Severity
}

// rowNotice returns the row-scoped notice for key, if any. It is derived
// from current state on every call -- never cached as a display string --
// so it can never go stale across selection moves, pin/reorder, or a
// provider reload: those all act on the same session.Key-keyed
// PendingConfirmation this reads (see State.Confirmation).
func (s *State) rowNotice(key session.Key) (RowNotice, bool) {
	if s.Confirmation == nil || s.Confirmation.Key != key {
		return RowNotice{}, false
	}
	message, severity := confirmationText(s.Confirmation.Action)
	return RowNotice{Message: message, Severity: severity}, true
}
