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
}

func toRun(r run) Run {
	return Run{ID: r.ID, Provider: r.Provider, SessionID: r.SessionID, CWD: r.CWD, PID: r.PID, StartTime: r.StartTime, UID: r.UID, Socket: r.Socket, State: r.State, Error: r.Error, Baseline: r.Baseline, StartedAt: r.StartedAt}
}
func fromRun(r Run) run {
	return run{ID: r.ID, Provider: r.Provider, SessionID: r.SessionID, CWD: r.CWD, PID: r.PID, StartTime: r.StartTime, UID: r.UID, Socket: r.Socket, State: r.State, Error: r.Error, Baseline: r.Baseline, StartedAt: r.StartedAt}
}

func emptyData() data {
	return data{ClaudeArchived: map[string]bool{}, ClaudeNames: map[string]string{}, Pinned: map[string]bool{}, Runs: map[string]run{}}
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
}
