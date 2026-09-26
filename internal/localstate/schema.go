package localstate

import "time"

// data is the raw persisted schema: package-private so no external package
// can perform a raw read-modify-write against it directly (see the
// DesignDoc's "Encapsulate local persistence schema" -- Store is the only
// Root Owner of this shape). Field names/JSON tags are unchanged from the
// pre-refactor internal/state.Data so an existing state.json written by an
// older agentsctl build round-trips unchanged. A field an older build wrote
// that no longer exists here (e.g. its "runs") is ignored on decode and
// dropped on the next save.
type data struct {
	ClaudeArchived map[string]bool `json:"claudeArchived,omitempty"`
	// ClaudeNames is a legacy migration fallback, keyed by native Claude
	// session ID: entries can only be left over from before agentsctl's
	// Claude Rename became a native, in-place rename (see
	// provider/claude.Provider.Rename). New renames are never written
	// here; provider/claude's List only consults an entry when Claude's
	// own native catalog reports no name at all for that session.
	ClaudeNames map[string]string `json:"claudeNames,omitempty"`
	// ClaudeCreatedAt maps a full native Claude sessionId (never the
	// shortened `id`) to that session's authoritative creation time: only
	// a value observed while the session had no running process is ever
	// recorded (see provider/claude.Provider.knownCreatedAt).
	ClaudeCreatedAt map[string]time.Time `json:"claudeCreatedAt,omitempty"`
	Pinned          map[string]bool      `json:"pinned,omitempty"`
	// ChatGPTCatalogs is provider/chatgpt's persisted last-known-good
	// catalog cache, keyed by ChatGPT Project ID (see
	// (*Store).ChatGPTCatalog/SaveChatGPTCatalog in chatgpt.go) so a
	// catalog from one configured Project can never be read back under
	// another.
	ChatGPTCatalogs map[string]chatGPTCatalog `json:"chatgptCatalogs,omitempty"`
}

// chatGPTCatalog is data.ChatGPTCatalogs' persisted element shape,
// converted to/from the exported ChatGPTCatalog domain type at the
// package boundary (toChatGPTCatalog/fromChatGPTCatalog in chatgpt.go).
type chatGPTCatalog struct {
	RefreshedAt   time.Time             `json:"refreshedAt"`
	Conversations []chatGPTConversation `json:"conversations,omitempty"`
}

type chatGPTConversation struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

func emptyData() data {
	return data{ClaudeArchived: map[string]bool{}, ClaudeNames: map[string]string{}, ClaudeCreatedAt: map[string]time.Time{}, Pinned: map[string]bool{}, ChatGPTCatalogs: map[string]chatGPTCatalog{}}
}

// normalize ensures every map field is non-nil after decode/mutation, so
// callers never see a nil map (matching the pre-refactor internal/state
// behavior).
func (d *data) normalize() {
	if d.ClaudeArchived == nil {
		d.ClaudeArchived = map[string]bool{}
	}
	if d.ClaudeNames == nil {
		d.ClaudeNames = map[string]string{}
	}
	if d.ClaudeCreatedAt == nil {
		d.ClaudeCreatedAt = map[string]time.Time{}
	}
	if d.Pinned == nil {
		d.Pinned = map[string]bool{}
	}
	if d.ChatGPTCatalogs == nil {
		d.ChatGPTCatalogs = map[string]chatGPTCatalog{}
	}
}
