package localstate

// ListPinned returns a copy of the provider-qualified pinned session keys.
// Implements sessionctl.PinStore.
func (s *Store) ListPinned() (map[string]bool, error) {
	d, err := s.view()
	if err != nil {
		return nil, err
	}
	result := make(map[string]bool, len(d.Pinned))
	for key, pinned := range d.Pinned {
		result[key] = pinned
	}
	return result, nil
}

// TogglePinned atomically changes and returns a session's pinned state.
// Implements sessionctl.PinStore.
func (s *Store) TogglePinned(key string) (bool, error) {
	pinned := false
	err := s.update(func(d *data) error {
		pinned = !d.Pinned[key]
		if pinned {
			d.Pinned[key] = true
		} else {
			delete(d.Pinned, key)
		}
		return nil
	})
	return pinned, err
}

// MigratePinned moves a pin from one provider-qualified key to another
// when a session's canonical key replaces a provisional one (see
// session.Session.PreviousKeys). The move is atomic and idempotent: if
// from is pinned, from is removed and to becomes pinned (merging with an
// existing pin on to, never duplicating it); if from is not pinned it does
// nothing, so it never alters a destination the user pinned or left
// unpinned themselves. Implements sessionctl.PinStore.
func (s *Store) MigratePinned(from, to string) error {
	if from == to {
		return nil
	}
	return s.update(func(d *data) error {
		if !d.Pinned[from] {
			return nil
		}
		delete(d.Pinned, from)
		d.Pinned[to] = true
		return nil
	})
}
