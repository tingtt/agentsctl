package claude

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tingtt/agentsctl/internal/session"
)

// TestUsageSnapshotRoundTripsAtomically fixes the write/read contract:
// writeUsageSnapshotAtomic followed by readUsageSnapshot must reproduce the
// same values, including a genuinely-unavailable window staying
// unavailable (not silently becoming a 0% on the way through JSON).
func TestUsageSnapshotRoundTripsAtomically(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "usage.json")
	want := usageSnapshot{
		FiveHour:   usageWindowSnapshot{State: session.UsageAvailable, Percent: 42, ResetAt: time.Unix(1000, 0)},
		Weekly:     usageWindowSnapshot{State: session.UsageUnknown},
		ObservedAt: time.Unix(2000, 0),
	}
	if err := writeUsageSnapshotAtomic(path, want); err != nil {
		t.Fatal(err)
	}
	got, ok, err := readUsageSnapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("readUsageSnapshot reported no snapshot right after writing one")
	}
	if got.FiveHour != want.FiveHour {
		t.Fatalf("FiveHour=%+v, want %+v", got.FiveHour, want.FiveHour)
	}
	if got.Weekly.State == session.UsageAvailable {
		t.Fatalf("Weekly=%+v, want State=UsageUnknown preserved across the round trip", got.Weekly)
	}
	if !got.ObservedAt.Equal(want.ObservedAt) {
		t.Fatalf("ObservedAt=%v, want %v", got.ObservedAt, want.ObservedAt)
	}
}

// TestReadUsageSnapshotMissingFileIsNotAnError fixes that "no snapshot
// yet" (the probe has never successfully refreshed) is reported via ok,
// not an error a caller must special-case.
func TestReadUsageSnapshotMissingFileIsNotAnError(t *testing.T) {
	_, ok, err := readUsageSnapshot(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil {
		t.Fatalf("err=%v, want nil for a missing file", err)
	}
	if ok {
		t.Fatal("ok=true for a missing file")
	}
}

// writeRawSnapshot writes raw JSON text directly to path -- used by the
// legacy-schema migration tests below to place a persisted document
// shaped exactly like what a pre-#19 agentsctl build (or the current
// state-based schema) would have written, bypassing writeUsageSnapshotAtomic
// (which can only ever produce the current schema).
func writeRawSnapshot(t *testing.T, path, raw string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestReadUsageSnapshotMigratesLegacyAvailableTrue fixes Issue #19's
// follow-up root cause: a snapshot persisted by a pre-#19 agentsctl build
// (usageWindowSnapshot's old `available bool` field, no `state` field at
// all) with `available: true` must decode to UsageAvailable at the
// persisted percentage, not silently drop to UsageUnknown (a plain
// json.Unmarshal into the current state-based struct would drop the
// unrecognized `available` field and leave State at its zero value --
// exactly what was observed against a real installed agentsctl's
// persisted usage.json, rendering as "claude ?% / ?%" despite a real
// 60%/36% reading sitting on disk).
func TestReadUsageSnapshotMigratesLegacyAvailableTrue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	writeRawSnapshot(t, path, `{"fiveHour":{"available":true,"percent":92,"resetAt":"2026-09-07T22:10:00+09:00"},"observedAt":"2026-09-07T17:58:34.496934+09:00"}`)
	snap, ok, err := readUsageSnapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("readUsageSnapshot reported no snapshot for an existing legacy file")
	}
	if snap.FiveHour.State != session.UsageAvailable || snap.FiveHour.Percent != 92 {
		t.Fatalf("FiveHour=%+v, want Available/92%% migrated from the legacy available:true field", snap.FiveHour)
	}
}

// TestReadUsageSnapshotMigratesLegacyAvailableFalse fixes the symmetric
// legacy case: `available: false` (the pre-#19 "not reported" value)
// migrates to UsageUnknown -- never UsageExhausted, since the pre-#19
// schema had no concept of exhausted at all and this bit alone cannot
// distinguish "never reported" from "was exhausted".
func TestReadUsageSnapshotMigratesLegacyAvailableFalse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	writeRawSnapshot(t, path, `{"fiveHour":{"available":false,"percent":0},"observedAt":"2026-09-07T17:58:34.496934+09:00"}`)
	snap, ok, err := readUsageSnapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("readUsageSnapshot reported no snapshot for an existing legacy file")
	}
	if snap.FiveHour.State != session.UsageUnknown {
		t.Fatalf("FiveHour=%+v, want UsageUnknown migrated from legacy available:false", snap.FiveHour)
	}
}

// TestReadUsageSnapshotNewSchemaAvailable fixes that a snapshot already
// in the current schema (a `state` field present) decodes straight
// through unaffected by the legacy-compatibility path.
func TestReadUsageSnapshotNewSchemaAvailable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	writeRawSnapshot(t, path, `{"fiveHour":{"state":1,"percent":42}}`)
	snap, ok, err := readUsageSnapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("readUsageSnapshot reported no snapshot for an existing file")
	}
	if snap.FiveHour.State != session.UsageAvailable || snap.FiveHour.Percent != 42 {
		t.Fatalf("FiveHour=%+v, want Available/42%%", snap.FiveHour)
	}
}

// TestReadUsageSnapshotNewSchemaExplicitUnknownIsNotMisreadAsLegacy fixes
// the critical distinction the wire type's pointer fields exist for: a
// current-schema snapshot with an explicit `"state": 0` (UsageUnknown,
// something this codebase itself wrote -- e.g. a window the statusLine
// payload never reported) must decode as UsageUnknown directly, never
// fall through to the legacy `available` fallback (which is entirely
// absent here) or be treated as if `state` were missing.
func TestReadUsageSnapshotNewSchemaExplicitUnknownIsNotMisreadAsLegacy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	writeRawSnapshot(t, path, `{"fiveHour":{"state":0,"percent":0}}`)
	snap, ok, err := readUsageSnapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("readUsageSnapshot reported no snapshot for an existing file")
	}
	if snap.FiveHour.State != session.UsageUnknown {
		t.Fatalf("FiveHour=%+v, want UsageUnknown -- an explicit state:0 must decode via the state field, not the (absent) legacy fallback", snap.FiveHour)
	}
}

// TestWriteUsageSnapshotAtomicNeverExposesPartialFile fixes the atomic-
// replace guarantee itself: a writer must never leave a reader able to
// observe a temp/partial file at the final path -- writeUsageSnapshotAtomic
// must clean up its own temp file and only the final path must exist
// afterward.
func TestWriteUsageSnapshotAtomicNeverExposesPartialFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage.json")
	if err := writeUsageSnapshotAtomic(path, usageSnapshot{ObservedAt: time.Unix(1, 0)}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "usage.json" {
		t.Fatalf("dir entries=%v, want only usage.json (no leftover temp file)", entries)
	}
}
