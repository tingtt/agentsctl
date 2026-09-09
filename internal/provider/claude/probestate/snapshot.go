package probestate

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/tingtt/agentsctl/internal/session"
)

// WindowSnapshot is one rate-limit window (5h or weekly) as persisted to
// agentsctl-owned storage. State reuses the provider-neutral
// session.UsageLimitState vocabulary directly (available/exhausted/unknown)
// rather than a redundant parallel Claude-internal enum: the three-state
// distinction itself is not Claude-specific, only the mechanism that
// arrives at it is. UsageUnknown covers "the statusLine payload didn't
// report this window at all" -- never conflated with a genuinely reported
// 0% (UsageAvailable, Percent 0).
type WindowSnapshot struct {
	State   session.UsageLimitState `json:"state"`
	Percent int                     `json:"percent"`
	ResetAt time.Time               `json:"resetAt"`
}

// Snapshot is the whole persisted usage.json document: the last
// statusLine reading the probe's collector observed, when it observed it
// (ObservedAt), and whether that observation actually reflects a
// completed API response for this probe process's own lifetime
// (ResponseObserved). Freshness bookkeeping (the TTL) lives with this
// package's own SnapshotFresh policy and the caller applying it;
// ObservedAt is what lets a refresh tell a newly-written snapshot apart
// from a stale one already on disk, and ResponseObserved is what lets it
// tell a snapshot that actually reflects a given refresh's own prompt
// apart from a periodic statusLine re-tick of unchanged pre-response (or
// stale prior-turn) data.
type Snapshot struct {
	FiveHour         WindowSnapshot `json:"fiveHour"`
	Weekly           WindowSnapshot `json:"weekly"`
	ObservedAt       time.Time      `json:"observedAt"`
	ResponseObserved bool           `json:"responseObserved"`
}

// windowSnapshotWire is the JSON decode shape for one persisted window,
// covering both the current (`state`, since Issue #19/PR #23) and legacy
// (`available`, everything persisted by an agentsctl build before that)
// formats -- see SnapshotStore.Load's doc comment for why this exists.
// State and Available are pointers specifically so decoding can tell "the
// field was present in the JSON" apart from "the field was absent": the
// current schema's own zero value (session.UsageUnknown == 0) would
// otherwise be indistinguishable from "no state field was ever written
// here at all" (a plain non-pointer session.UsageLimitState always decodes
// to 0 either way), which is exactly the distinction that must not be
// lost -- a persisted `"state": 0` (explicitly UsageUnknown, written by
// this codebase) must never be treated as "legacy, fall back to
// available".
type windowSnapshotWire struct {
	State     *session.UsageLimitState `json:"state"`
	Available *bool                    `json:"available"`
	Percent   int                      `json:"percent"`
	ResetAt   time.Time                `json:"resetAt"`
}

// resolve picks the current-schema `state` field when present
// (authoritative for anything this codebase itself wrote from Issue #19
// onward -- Save never omits it), and only falls back to the legacy
// `available` boolean when `state` is entirely absent from the JSON (a
// snapshot persisted by a pre-#19 build, which never wrote `state` at
// all). Legacy `available: false` resolves to UsageUnknown, never
// UsageExhausted -- the pre-#19 schema had no concept of exhausted, so
// there is nothing to distinguish it from "not reported".
func (w windowSnapshotWire) resolve() WindowSnapshot {
	if w.State != nil {
		return WindowSnapshot{State: *w.State, Percent: w.Percent, ResetAt: w.ResetAt}
	}
	if w.Available != nil && *w.Available {
		return WindowSnapshot{State: session.UsageAvailable, Percent: w.Percent, ResetAt: w.ResetAt}
	}
	return WindowSnapshot{State: session.UsageUnknown, Percent: w.Percent, ResetAt: w.ResetAt}
}

// snapshotWire is Snapshot's own decode-only counterpart to
// windowSnapshotWire -- see SnapshotStore.Load.
type snapshotWire struct {
	FiveHour         windowSnapshotWire `json:"fiveHour"`
	Weekly           windowSnapshotWire `json:"weekly"`
	ObservedAt       time.Time          `json:"observedAt"`
	ResponseObserved bool               `json:"responseObserved"`
}

// SnapshotStore is the Root Owner of one usage.json: every read of the
// file decodes through the same legacy-compatible wire format, and every
// write goes through Save's atomic replace. Unlike IdentityStore, no
// internal read-modify-write transaction lock is needed here -- the only
// correctness property this file itself provides is "a reader never
// observes a half-written document" (Save's atomic rename), and the
// higher-level guarantee "at most one process refreshes and persists a
// stale snapshot at a time" is the refresh coordinator's own responsibility
// (its machine-global refresh lock), not this store's.
type SnapshotStore struct {
	path string
}

// NewSnapshotStore returns a store for the usage snapshot persisted at
// path (a usage.json).
func NewSnapshotStore(path string) *SnapshotStore {
	return &SnapshotStore{path: path}
}

// Load reads the persisted snapshot, if any. A missing file is reported
// via ok=false, not an error -- there is simply no snapshot yet (e.g. the
// probe has never successfully refreshed).
//
// Decoding goes through snapshotWire/windowSnapshotWire.resolve rather
// than a plain json.Unmarshal into Snapshot directly, so a snapshot
// persisted by an agentsctl build from before Issue #19's PR #23 (whose
// window shape had an `available bool` field, no `state` at all) still
// decodes to a meaningful session.UsageLimitState instead of silently
// dropping the unknown `available` field and defaulting State to its zero
// value (UsageUnknown) regardless of what percentage was actually
// persisted -- confirmed against a real installed agentsctl exhibiting
// exactly this symptom.
func (s *SnapshotStore) Load() (Snapshot, bool, error) {
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return Snapshot{}, false, nil
	}
	if err != nil {
		return Snapshot{}, false, err
	}
	var wire snapshotWire
	if err := json.Unmarshal(b, &wire); err != nil {
		return Snapshot{}, false, err
	}
	snap := Snapshot{
		FiveHour:         wire.FiveHour.resolve(),
		Weekly:           wire.Weekly.resolve(),
		ObservedAt:       wire.ObservedAt,
		ResponseObserved: wire.ResponseObserved,
	}
	return snap, true, nil
}

// LoadFresh is Load plus SnapshotFresh's freshness policy applied in one
// call: ok is true only if a snapshot exists AND is still fresh as of now
// (see SnapshotFresh) -- the single freshness-policy entry point every
// persisted-snapshot freshness check in this codebase goes through, so a
// caller checking whether a cross-process refresh is still necessary never
// re-implements the TTL comparison itself.
func (s *SnapshotStore) LoadFresh(now time.Time, ttl time.Duration) (Snapshot, bool, error) {
	snap, ok, err := s.Load()
	if err != nil || !ok {
		return Snapshot{}, false, err
	}
	if !SnapshotFresh(snap.ObservedAt, now, ttl) {
		return Snapshot{}, false, nil
	}
	return snap, true, nil
}

// Save persists snap to path via writeFileAtomic, so a concurrent reader
// (this same probe's Usage() call running in another process, or a future
// collector invocation) never observes a partially-written file.
func (s *SnapshotStore) Save(snap Snapshot) error {
	b, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	return writeFileAtomic(s.path, b)
}

// SnapshotFresh is the one pure definition of "is a snapshot observed at
// observedAt still fresh as of now, given ttl" -- shared by SnapshotStore's
// own persisted-freshness check (LoadFresh) and by any process-local
// in-memory cache freshness check in internal/provider/claude, so the TTL
// policy is never re-implemented in more than one place.
func SnapshotFresh(observedAt, now time.Time, ttl time.Duration) bool {
	return now.Sub(observedAt) < ttl
}

// writeFileAtomic writes data to path via a temp-file-plus-rename swap --
// the same pattern localstate.Store uses for state.json -- shared by every
// file this package owns, so none of them can ever be observed
// half-written by a concurrent reader.
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
