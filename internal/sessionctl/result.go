package sessionctl

import "github.com/tingtt/agentsctl/internal/session"

// Result tells the caller (internal/agentview) what happened to the
// catalog as a side effect of an operation, so Agent View does not need to
// hardcode a per-action refresh policy -- the exact thing the current TUI
// event loop does today ("Pin は refresh しない / Rename は refresh しな
// い / その他は refresh"). Most operations require a full provider reload
// to reflect newly-authoritative native state; Pin and Rename already know
// their own outcome synchronously and are reflected with a local Patch
// instead (see TogglePin, Rename).
type Result struct {
	// Reload, when true, means the caller must re-run Controller.Load to
	// see this operation's effect (a new, changed, or removed session).
	Reload bool
	// Patch, set only when Reload is false, is the local, already-
	// confirmed change the caller can apply directly to its existing rows
	// without a provider round-trip.
	Patch *Patch
}

// Patch is a local, already-confirmed change to one session's row.
type Patch struct {
	Key    session.Key
	Pinned *bool
	Name   *string
}
