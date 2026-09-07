package claude

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// usageWindowSnapshot is one rate-limit window (5h or weekly) as persisted
// to agentsctl-owned storage -- Available distinguishes "the statusLine
// payload reported 0% used" from "this window was absent from the
// payload", the same distinction session.UsageWindow makes at the
// provider-neutral boundary (see toSessionUsage in usage.go, the only
// place this Claude-specific shape is converted to session.Usage).
type usageWindowSnapshot struct {
	Available bool      `json:"available"`
	Percent   int       `json:"percent"`
	ResetAt   time.Time `json:"resetAt"`
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

// writeUsageSnapshotAtomic persists snap to path via a temp-file-plus-
// rename swap, so a concurrent reader (this same probe's Usage() call
// running in another process, or a future collector invocation) never
// observes a partially-written file -- the same pattern localstate.Store
// uses for state.json.
func writeUsageSnapshotAtomic(path string, snap usageSnapshot) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".usage-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
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
