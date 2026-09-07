package session

// ActionID identifies one Agent View session action. Each action's
// availability is independent of every other action's -- Stop can be
// unavailable on a session for one reason while Rename is available on
// that same session (see Actions), rather than the whole row sharing one
// bool group and one Reason string.
type ActionID string

const (
	// ActionOpen is the common Agent View intent for taking over the
	// terminal for a session, whatever the provider's transport is (see
	// the DesignDoc's "Open as common Agent View intent").
	ActionOpen ActionID = "open"
	// ActionDispatch starts a new session for a provider. It is not
	// per-session (no session exists yet); sessionctl exposes it
	// per-provider rather than through Actions, but the ActionID is shared
	// so error messages/reasons stay consistent.
	ActionDispatch ActionID = "dispatch"
	ActionStop     ActionID = "stop"
	ActionRename   ActionID = "rename"
	ActionArchive  ActionID = "archive"
)

// Availability is one action's yes/no state, plus, when unavailable, why.
// Reason is only meaningful when Available is false.
type Availability struct {
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
}

// Actions is a Session's action-specific availability, keyed by ActionID.
// An action absent from the map is unavailable with no specific reason
// (see Available/Reason) -- the fail-closed default for any action a
// provider never populated, including one whose provider does not
// implement the corresponding capability interface at all (see
// internal/sessionctl.Controller.Load, which enforces that against the
// concrete provider's implemented interfaces).
type Actions map[ActionID]Availability

// Available reports whether id is present and available on a.
func (a Actions) Available(id ActionID) bool { return a[id].Available }

// Reason returns id's unavailable reason on a, or a generic fallback if
// the action carries no specific one.
func (a Actions) Reason(id ActionID) string {
	if r := a[id].Reason; r != "" {
		return r
	}
	return "action is unavailable in the current state"
}

// With returns a copy of a with id's availability set to v. Actions is
// treated as immutable by convention (see Normalize in capability.go,
// which always builds a new map) so a Session's provider-reported Actions
// is never mutated out from under it by a later normalization pass.
func (a Actions) With(id ActionID, v Availability) Actions {
	next := make(Actions, len(a)+1)
	for k, existing := range a {
		next[k] = existing
	}
	next[id] = v
	return next
}
