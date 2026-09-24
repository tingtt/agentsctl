package localstate

import "time"

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

// ClaudeCreatedAt returns the known authoritative creation time of each
// Claude session, keyed by its full native sessionId.
func (s *Store) ClaudeCreatedAt() (map[string]time.Time, error) {
	d, err := s.view()
	if err != nil {
		return nil, err
	}
	known := make(map[string]time.Time, len(d.ClaudeCreatedAt))
	for id, t := range d.ClaudeCreatedAt {
		known[id] = t
	}
	return known, nil
}

// SyncClaudeCreatedAt records each authoritative creation time --
// overwriting, not merging with, any earlier value for that sessionId --
// and forgets every entry whose sessionId is absent from catalog. Callers
// must only pass the catalog of a complete, successful `claude agents`
// listing, since every sessionId missing from it is deleted.
func (s *Store) SyncClaudeCreatedAt(authoritative map[string]time.Time, catalog map[string]bool) error {
	return s.update(func(d *data) error {
		for id := range d.ClaudeCreatedAt {
			if !catalog[id] {
				delete(d.ClaudeCreatedAt, id)
			}
		}
		for id, t := range authoritative {
			d.ClaudeCreatedAt[id] = t
		}
		return nil
	})
}
