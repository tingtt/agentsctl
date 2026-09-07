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
// Claude-specific (see this package's reset-boundary handling in
// toSessionUsageWindow). UsageUnknown covers "the statusLine payload
// didn't report this window at all" -- never conflated with a genuinely
// reported 0% (UsageAvailable, Percent 0).
type usageWindowSnapshot struct {
	State   session.UsageLimitState `json:"state"`
	Percent int                     `json:"percent"`
	ResetAt time.Time               `json:"resetAt"`
}

// usageSnapshot is the whole persisted usage.json document: the last
// statusLine reading the probe's collector observed, plus when it observed
// it (ObservedAt) -- freshness bookkeeping lives in the in-memory Probe
// (see usage_probe_unix.go), but ObservedAt is what lets a refresh tell a
// newly-written snapshot apart from a stale one already on disk (see
// waitForFreshSnapshot).
type usageSnapshot struct {
	FiveHour   usageWindowSnapshot `json:"fiveHour"`
	Weekly     usageWindowSnapshot `json:"weekly"`
	ObservedAt time.Time           `json:"observedAt"`
}

// readUsageSnapshot reads path's persisted snapshot, if any. A missing
// file is reported via ok=false, not an error -- there is simply no
// snapshot yet (e.g. the probe has never successfully refreshed).
func readUsageSnapshot(path string) (snap usageSnapshot, ok bool, err error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return usageSnapshot{}, false, nil
	}
	if err != nil {
		return usageSnapshot{}, false, err
	}
	if err := json.Unmarshal(b, &snap); err != nil {
		return usageSnapshot{}, false, err
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
