package session

// Normalize applies the provider-independent action-availability
// invariants on top of whatever action-specific availability a provider
// already computed in s.Actions (see internal/sessionctl.Controller.Load
// for the other half of this narrowing -- fail-closed on an action whose
// provider does not implement the corresponding capability interface).
//
// These rules hold for every provider, so no provider implementation
// needs to duplicate them:
//   - an active session (s.Activity.Active()) cannot be archived; Stop
//     should be used first (mirrors the DesignDoc's "Archive と Stop を同
//     じ lifecycle action とする" rejected alternative).
//   - an archived session exposes no action except none of the above are
//     archive-scoped in this refactor's UI (Non-Goal: no unarchive-from-
//     Agent-View operation), so an archived session simply has no
//     available actions at all.
func Normalize(s Session) Actions {
	actions := make(Actions, len(s.Actions))
	for id, v := range s.Actions {
		actions[id] = v
	}
	if s.Archived {
		for id := range actions {
			actions[id] = Availability{Reason: "session is archived"}
		}
		return actions
	}
	if s.Activity.Active() {
		if actions[ActionArchive].Available {
			actions[ActionArchive] = Availability{Reason: "stop the session before archiving"}
		}
	}
	return actions
}
