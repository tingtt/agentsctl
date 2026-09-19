package session

// IdentityTransitions validates the identity continuity providers state
// through Session.PreviousKeys and returns the accepted transitions as a
// map from each old (provisional) Key to the Key that replaced it. It is
// the single place that decides whether a stated continuity may be
// followed, so every consumer of it -- selection and other transient UI
// state, local metadata such as pins -- shares the same semantics.
//
// PreviousKeys being present is not enough. A transition old -> new is
// accepted only if:
//
//   - old is not itself the Key of a session in sessions (its identity
//     still exists, so nothing was replaced);
//   - old is claimed by exactly one destination (a key claimed by more
//     than one distinct session is ambiguous and is dropped for all of
//     them, never resolved by input order);
//   - old differs from new (a self transition is not a transition);
//   - old and new belong to the same provider (identity never crosses a
//     provider boundary).
//
// Anything else is ignored: continuity fails closed. The result never
// depends on the order of sessions, and is never nil.
func IdentityTransitions(sessions []Session) map[Key]Key {
	present := make(map[Key]struct{}, len(sessions))
	for _, s := range sessions {
		present[s.Key] = struct{}{}
	}
	moved := map[Key]Key{}
	ambiguous := map[Key]struct{}{}
	for _, s := range sessions {
		for _, prev := range s.PreviousKeys {
			if prev == s.Key || prev.Provider != s.Key.Provider {
				continue
			}
			if _, exists := present[prev]; exists {
				continue
			}
			if dest, claimed := moved[prev]; claimed && dest != s.Key {
				ambiguous[prev] = struct{}{}
			}
			moved[prev] = s.Key
		}
	}
	for prev := range ambiguous {
		delete(moved, prev)
	}
	return moved
}
