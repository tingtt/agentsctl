package claude

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/tingtt/agentsctl/internal/session"
)

// usageWindowSnapshot is one rate-limit window (5h or weekly) as persisted
// to agentsctl-owned storage. State reuses the provider-neutral
// session.UsageLimitState vocabulary directly (available/exhausted/unknown
// -- see Issue #19's "Normalized state") rather than a redundant parallel
// Claude-internal enum: the three-state distinction itself is not
// Claude-specific, only the mechanism that arrives at it is (see
// usage_limit.go's classifyProbeOutput and this package's reset-boundary
// handling in toSessionUsageWindow). UsageUnknown covers "the statusLine
// payload didn't report this window at all" -- never conflated with a
// genuinely reported 0% (UsageAvailable, Percent 0).
type usageWindowSnapshot struct {
	State   session.UsageLimitState `json:"state"`
	Percent int                     `json:"percent"`
	ResetAt time.Time               `json:"resetAt"`
}

// usageSnapshot is the whole persisted usage.json document: the last
// statusLine reading the probe's collector observed, when it observed it
// (ObservedAt), and whether that observation actually reflects a
// completed API response for this probe process's own lifetime
// (ResponseObserved -- see parseStatusLinePayload's doc comment). Freshness
// bookkeeping (the TTL) lives in the in-memory Probe (see
// usage_probe_unix.go); ObservedAt is what lets a refresh tell a
// newly-written snapshot apart from a stale one already on disk, and
// ResponseObserved is what lets it tell a snapshot that actually reflects
// this refresh's own prompt apart from a periodic statusLine re-tick of
// unchanged pre-response (or stale prior-turn) data (see
// waitForProbeOutcome).
type usageSnapshot struct {
	FiveHour         usageWindowSnapshot `json:"fiveHour"`
	Weekly           usageWindowSnapshot `json:"weekly"`
	ObservedAt       time.Time           `json:"observedAt"`
	ResponseObserved bool                `json:"responseObserved"`
}

// usageWindowSnapshotWire is the JSON decode shape for one persisted
// window, covering both the current (`state`, since Issue #19/PR #23) and
// legacy (`available`, everything persisted by an agentsctl build before
// that) formats -- see readUsageSnapshot's doc comment for why this
// exists. State and Available are pointers specifically so decoding can
// tell "the field was present in the JSON" apart from "the field was
// absent": the current schema's own zero value (session.UsageUnknown ==
// 0) would otherwise be indistinguishable from "no state field was ever
// written here at all" (a plain non-pointer session.UsageLimitState
// always decodes to 0 either way), which is exactly the distinction that
// must not be lost -- a persisted `"state": 0` (explicitly UsageUnknown,
// written by this codebase) must never be treated as "legacy, fall back
// to available".
type usageWindowSnapshotWire struct {
	State     *session.UsageLimitState `json:"state"`
	Available *bool                    `json:"available"`
	Percent   int                      `json:"percent"`
	ResetAt   time.Time                `json:"resetAt"`
}

// resolve picks the current-schema `state` field when present
// (authoritative for anything this codebase itself wrote from Issue #19
// onward -- writeUsageSnapshotAtomic never omits it), and only falls back
// to the legacy `available` boolean when `state` is entirely absent from
// the JSON (a snapshot persisted by a pre-#19 build, which never wrote
// `state` at all). Legacy `available: false` resolves to UsageUnknown,
// never UsageExhausted -- the pre-#19 schema had no concept of exhausted,
// so there is nothing to distinguish it from "not reported".
func (w usageWindowSnapshotWire) resolve() usageWindowSnapshot {
	if w.State != nil {
		return usageWindowSnapshot{State: *w.State, Percent: w.Percent, ResetAt: w.ResetAt}
	}
	if w.Available != nil && *w.Available {
		return usageWindowSnapshot{State: session.UsageAvailable, Percent: w.Percent, ResetAt: w.ResetAt}
	}
	return usageWindowSnapshot{State: session.UsageUnknown, Percent: w.Percent, ResetAt: w.ResetAt}
}

// usageSnapshotWire is usageSnapshot's own decode-only counterpart to
// usageWindowSnapshotWire -- see readUsageSnapshot.
type usageSnapshotWire struct {
	FiveHour         usageWindowSnapshotWire `json:"fiveHour"`
	Weekly           usageWindowSnapshotWire `json:"weekly"`
	ObservedAt       time.Time               `json:"observedAt"`
	ResponseObserved bool                    `json:"responseObserved"`
}

// readUsageSnapshot reads path's persisted snapshot, if any. A missing
// file is reported via ok=false, not an error -- there is simply no
// snapshot yet (e.g. the probe has never successfully refreshed).
//
// Decoding goes through usageSnapshotWire/usageWindowSnapshotWire.resolve
// rather than a plain json.Unmarshal into usageSnapshot directly, so a
// snapshot persisted by an agentsctl build from before Issue #19's PR #23
// (whose usageWindowSnapshot had an `available bool` field, no `state` at
// all) still decodes to a meaningful session.UsageLimitState instead of
// silently dropping the unknown `available` field and defaulting State to
// its zero value (UsageUnknown) regardless of what percentage was
// actually persisted -- confirmed against a real installed agentsctl
// exhibiting exactly this: a legacy usage.json with `"available":true,
// "percent":60`, decoding State to UsageUnknown under a plain Unmarshal,
// rendering as the reported "claude ?% / ?%" stuck symptom in Agent View
// even though a real 60%/36% reading was sitting right there on disk.
func readUsageSnapshot(path string) (snap usageSnapshot, ok bool, err error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return usageSnapshot{}, false, nil
	}
	if err != nil {
		return usageSnapshot{}, false, err
	}
	var wire usageSnapshotWire
	if err := json.Unmarshal(b, &wire); err != nil {
		return usageSnapshot{}, false, err
	}
	snap = usageSnapshot{
		FiveHour:         wire.FiveHour.resolve(),
		Weekly:           wire.Weekly.resolve(),
		ObservedAt:       wire.ObservedAt,
		ResponseObserved: wire.ResponseObserved,
	}
	return snap, true, nil
}

// writeUsageSnapshotAtomic persists snap to path via writeFileAtomic, so a
// concurrent reader (this same probe's Usage() call running in another
// process, or a future collector invocation) never observes a partially-
// written file.
func writeUsageSnapshotAtomic(path string, snap usageSnapshot) error {
	b, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	return writeFileAtomic(path, b)
}

// writeFileAtomic writes data to path via a temp-file-plus-rename swap --
// the same pattern localstate.Store uses for state.json -- shared by every
// file this package's usage probe owns (the snapshot, the dedicated
// settings.json, and the probe identity file), so none of them can ever be
// observed half-written by a concurrent reader.
func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
