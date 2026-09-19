package localstate

import "time"

// Run is agentsctl's local record of a Codex managed run: the interactive
// Codex CLI process + PTY agentsctl's supervisor started, tracked
// separately from the Codex app-server thread it may or may not yet be
// bound to (see the DesignDoc's Codex run-to-thread binding). Provider and
// supervisor code exchange this domain type; the JSON representation it is
// persisted as (see data.Runs) is private to this package.
type Run struct {
	ID        string
	Provider  string
	SessionID string
	CWD       string
	PID       int
	StartTime uint64
	UID       uint32
	Socket    string
	State     string
	Error     string
	Baseline  []string
	StartedAt time.Time
	// PendingRename is a session name a rename-only dispatch is still
	// waiting to apply: it belongs to the run until the run is bound to a
	// thread that can be renamed, and is cleared after the one attempt to
	// apply it (see provider/codex.Provider.applyPendingRenames).
	PendingRename string
	// RenameError records why that attempt failed, so the failure is
	// surfaced instead of lost; it is not retried automatically -- the
	// thread exists, so an ordinary rename of it is the retry.
	RenameError string
}

// data is the raw persisted schema: package-private so no external package
// can perform a raw read-modify-write against it directly (see the
// DesignDoc's "Encapsulate local persistence schema" -- Store is the only
// Root Owner of this shape). Field names/JSON tags are unchanged from the
// pre-refactor internal/state.Data so an existing state.json written by an
// older agentsctl build round-trips unchanged.
type data struct {
	ClaudeArchived map[string]bool `json:"claudeArchived,omitempty"`
	// ClaudeNames is a legacy migration fallback, keyed by native Claude
	// session ID: entries can only be left over from before agentsctl's
	// Claude Rename became a native, in-place rename (see
	// provider/claude.Provider.Rename). New renames are never written
	// here; provider/claude's List only consults an entry when Claude's
	// own native catalog reports no name at all for that session.
	ClaudeNames map[string]string `json:"claudeNames,omitempty"`
	Pinned      map[string]bool   `json:"pinned,omitempty"`
	Runs        map[string]run    `json:"runs,omitempty"`
	// ChatGPTCatalogs is provider/chatgpt's persisted last-known-good
	// catalog cache, keyed by ChatGPT Project ID (see
	// (*Store).ChatGPTCatalog/SaveChatGPTCatalog in chatgpt.go) so a
	// catalog from one configured Project can never be read back under
	// another.
	ChatGPTCatalogs map[string]chatGPTCatalog `json:"chatgptCatalogs,omitempty"`
}

// run is data.Runs' persisted element shape, converted to/from the
// exported Run domain type at the package boundary (toRun/fromRun).
type run struct {
	ID        string    `json:"id"`
	Provider  string    `json:"provider"`
	SessionID string    `json:"sessionId,omitempty"`
	CWD       string    `json:"cwd"`
	PID       int       `json:"pid,omitempty"`
	StartTime uint64    `json:"startTime,omitempty"`
	UID       uint32    `json:"uid,omitempty"`
	Socket    string    `json:"socket,omitempty"`
	State     string    `json:"state"`
	Error     string    `json:"error,omitempty"`
	Baseline  []string  `json:"baseline,omitempty"`
	StartedAt time.Time `json:"startedAt"`

	PendingRename string `json:"pendingRename,omitempty"`
	RenameError   string `json:"renameError,omitempty"`
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

func toRun(r run) Run {
	return Run{ID: r.ID, Provider: r.Provider, SessionID: r.SessionID, CWD: r.CWD, PID: r.PID, StartTime: r.StartTime, UID: r.UID, Socket: r.Socket, State: r.State, Error: r.Error, Baseline: r.Baseline, StartedAt: r.StartedAt, PendingRename: r.PendingRename, RenameError: r.RenameError}
}
func fromRun(r Run) run {
	return run{ID: r.ID, Provider: r.Provider, SessionID: r.SessionID, CWD: r.CWD, PID: r.PID, StartTime: r.StartTime, UID: r.UID, Socket: r.Socket, State: r.State, Error: r.Error, Baseline: r.Baseline, StartedAt: r.StartedAt, PendingRename: r.PendingRename, RenameError: r.RenameError}
}

func emptyData() data {
	return data{ClaudeArchived: map[string]bool{}, ClaudeNames: map[string]string{}, Pinned: map[string]bool{}, Runs: map[string]run{}, ChatGPTCatalogs: map[string]chatGPTCatalog{}}
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
	if d.Pinned == nil {
		d.Pinned = map[string]bool{}
	}
	if d.Runs == nil {
		d.Runs = map[string]run{}
	}
	if d.ChatGPTCatalogs == nil {
		d.ChatGPTCatalogs = map[string]chatGPTCatalog{}
	}
}
