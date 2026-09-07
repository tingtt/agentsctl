// Package session holds the provider-neutral session domain: the common
// model Agent View is built on (Session, Key, Activity, Actions), and pure
// rules over that model (ordering, directory scope, action-availability
// invariants). Nothing here performs I/O or knows about Claude, Codex, the
// terminal, or local persistence -- those are provider/sessionctl/agentview
// concerns that consume this package, never the other way around.
package session

import "time"

// ProviderID identifies which provider a Session/Key belongs to.
type ProviderID string

const (
	ProviderClaude ProviderID = "claude"
	ProviderCodex  ProviderID = "codex"
)

// Key is a provider-qualified session identifier: the single source of
// truth for session identity across refresh, pin/unpin, reordering, and
// provider reload. Agent View selection and row-scoped notices are keyed by
// Key, never by list position (see internal/agentview).
type Key struct {
	Provider ProviderID `json:"provider"`
	ID       string     `json:"id"`
}

func (k Key) String() string { return string(k.Provider) + ":" + k.ID }

// IsZero reports whether k is the zero Key, i.e. does not identify any
// session.
func (k Key) IsZero() bool { return k == Key{} }

// Activity is the common lifecycle state Agent View renders, normalized by
// each provider from its own native lifecycle signal.
type Activity string

const (
	ActivityStarting     Activity = "starting"
	ActivityWorking      Activity = "working"
	ActivityNeedsInput   Activity = "needsInput"
	ActivityWaitingQuota Activity = "waitingForQuota"
	ActivityIdle         Activity = "idle"
	ActivityCompleted    Activity = "completed"
	ActivityFailed       Activity = "failed"
	ActivityUnknown      Activity = "unknown"
)

// Active reports whether a represents an in-progress agent turn. This is a
// provider-independent invariant input (see Normalize in capability.go):
// e.g. an active session cannot be archived, regardless of provider.
func (a Activity) Active() bool {
	return a == ActivityWorking || a == ActivityNeedsInput || a == ActivityStarting
}

// Runtime is the common process/ownership state Agent View renders.
type Runtime string

const (
	// RuntimeAttached is presently attached from this Agent View (reserved
	// for future use; no provider emits it today).
	RuntimeAttached Runtime = "attached"
	// RuntimeDetached is a session agentsctl (or its native provider) could
	// safely attach to or operate on.
	RuntimeDetached Runtime = "detached"
	// RuntimeExternal is a live process/session whose ownership agentsctl
	// cannot prove -- see the DesignDoc's fail-closed process-ownership
	// rule. Destructive/attach actions must not be offered for it.
	RuntimeExternal Runtime = "external"
	// RuntimeStopped is confirmed not running.
	RuntimeStopped Runtime = "stopped"
	// RuntimeNone applies to a session with no runtime concept at all (a
	// Codex thread with no managed run bound to it).
	RuntimeNone    Runtime = "none"
	RuntimeUnknown Runtime = "unknown"
)

// Session is the common, provider-neutral model Agent View renders and
// acts on. Provider-specific native objects never reach Agent View
// directly -- each provider normalizes into this shape (see
// internal/sessionctl's Source capability).
type Session struct {
	Key       Key       `json:"key"`
	Name      string    `json:"name"`
	Summary   string    `json:"summary"`
	CWD       string    `json:"cwd"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
	Pinned    bool      `json:"pinned"`
	Activity  Activity  `json:"activity"`
	Runtime   Runtime   `json:"runtime"`
	Archived  bool      `json:"archived"`
	// Actions is the action-specific availability a provider computed for
	// this session (see action.go). sessionctl.Controller further narrows
	// it: an action absent here, or present but backed by a provider that
	// does not implement the corresponding capability interface, is
	// fail-closed unavailable (see internal/sessionctl's Load).
	Actions Actions `json:"actions"`
	// RunID is set only for a Codex session with an agentsctl-managed run
	// (see the DesignDoc's Codex run-to-thread binding); empty otherwise.
	RunID string `json:"runId,omitempty"`
}

// DisplayName is the title Agent View shows: the provider-assigned name,
// falling back to a content summary, falling back to the raw session ID.
func (s Session) DisplayName() string {
	if s.Name != "" {
		return s.Name
	}
	if s.Summary != "" {
		return s.Summary
	}
	return s.Key.ID
}
