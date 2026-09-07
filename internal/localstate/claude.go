package localstate

// ClaudeState returns the Claude archive overlay (session ID -> archived)
// and the legacy rename-name fallback (session ID -> name), read together
// since provider/claude's List consults both for every row in one pass.
func (s *Store) ClaudeState() (archived map[string]bool, legacyNames map[string]string, err error) {
	d, err := s.view()
	if err != nil {
		return nil, nil, err
	}
	archived = make(map[string]bool, len(d.ClaudeArchived))
	for id, v := range d.ClaudeArchived {
		archived[id] = v
	}
	legacyNames = make(map[string]string, len(d.ClaudeNames))
	for id, v := range d.ClaudeNames {
		legacyNames[id] = v
	}
	return archived, legacyNames, nil
}

// SetClaudeArchived marks a Claude session as archived under agentsctl's
// local overlay (see the DesignDoc: Claude Archive never touches the
// native session, transcript, or worktree).
func (s *Store) SetClaudeArchived(id string) error {
	return s.update(func(d *data) error { d.ClaudeArchived[id] = true; return nil })
}

// ClearClaudeArchived removes a Claude session from the local archive
// overlay.
func (s *Store) ClearClaudeArchived(id string) error {
	return s.update(func(d *data) error { delete(d.ClaudeArchived, id); return nil })
}

// ClearLegacyClaudeName deletes a stale pre-native-rename overlay entry
// for id, the moment a native rename for that session is confirmed (see
// provider/claude.Provider.Rename) -- List() only ever treats this overlay
// as a fallback for a session with no native name at all, so a stale entry
// must not go on hiding the name Claude itself now reports.
func (s *Store) ClearLegacyClaudeName(id string) error {
	return s.update(func(d *data) error { delete(d.ClaudeNames, id); return nil })
}
