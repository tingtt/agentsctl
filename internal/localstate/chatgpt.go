package localstate

import "time"

// ChatGPTConversation is one persisted ChatGPT conversation's remote
// metadata -- exactly what provider/chatgpt's last-known-good cache needs
// to rehydrate a session.Session after a restart, and nothing more. It
// deliberately excludes anything local or derived: no CWD (provider/
// chatgpt always hydrates using the CURRENT Config.Root, never a
// persisted one -- so a moved checkout or a second local clone of the
// same Project never inherits a stale logical CWD), and no Actions/
// Pinned/Runtime/Activity (reconstructed by normal domain rules on every
// hydrate/List, never persisted).
type ChatGPTConversation struct {
	ID        string
	Title     string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ChatGPTCatalog is one ChatGPT Project's persisted last-known-good
// catalog. RefreshedAt is the completed remote enumeration's own
// timestamp, not this record's write time.
type ChatGPTCatalog struct {
	RefreshedAt   time.Time
	Conversations []ChatGPTConversation
}

// ChatGPTCatalog returns the persisted last-known-good catalog for
// projectID, and whether one has ever been saved for it. Catalogs are
// strictly isolated per Project ID: a catalog saved under one project_id
// is never returned for another.
func (s *Store) ChatGPTCatalog(projectID string) (ChatGPTCatalog, bool, error) {
	d, err := s.view()
	if err != nil {
		return ChatGPTCatalog{}, false, err
	}
	raw, ok := d.ChatGPTCatalogs[projectID]
	if !ok {
		return ChatGPTCatalog{}, false, nil
	}
	return toChatGPTCatalog(raw), true, nil
}

// SaveChatGPTCatalog persists catalog as projectID's new last-known-good
// catalog, fully replacing whatever was previously saved for it (an empty
// catalog.Conversations is a valid, meaningful replacement -- e.g. a
// COMPLETE enumeration that legitimately observed zero conversations --
// and is saved as such, not skipped). Every other Project's catalog, and
// every other kind of local state (pins, Claude overlays),
// is preserved untouched by the same read-modify-write transaction (see
// (*Store).update).
//
// Callers must only pass a catalog assembled from a COMPLETE remote
// enumeration -- localstate itself does not, and cannot, enforce that
// invariant; it is provider/chatgpt's responsibility (see
// provider.runRefresh).
func (s *Store) SaveChatGPTCatalog(projectID string, catalog ChatGPTCatalog) error {
	return s.update(func(d *data) error {
		d.ChatGPTCatalogs[projectID] = fromChatGPTCatalog(catalog)
		return nil
	})
}

func toChatGPTCatalog(c chatGPTCatalog) ChatGPTCatalog {
	conversations := make([]ChatGPTConversation, len(c.Conversations))
	for i, item := range c.Conversations {
		conversations[i] = ChatGPTConversation{ID: item.ID, Title: item.Title, CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt}
	}
	return ChatGPTCatalog{RefreshedAt: c.RefreshedAt, Conversations: conversations}
}

func fromChatGPTCatalog(c ChatGPTCatalog) chatGPTCatalog {
	conversations := make([]chatGPTConversation, len(c.Conversations))
	for i, item := range c.Conversations {
		conversations[i] = chatGPTConversation{ID: item.ID, Title: item.Title, CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt}
	}
	return chatGPTCatalog{RefreshedAt: c.RefreshedAt, Conversations: conversations}
}
