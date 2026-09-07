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
