package claude

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestUsageSnapshotRoundTripsAtomically fixes the write/read contract:
// writeUsageSnapshotAtomic followed by readUsageSnapshot must reproduce the
// same values, including a genuinely-unavailable window staying
// unavailable (not silently becoming a 0% on the way through JSON).
func TestUsageSnapshotRoundTripsAtomically(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "usage.json")
	want := usageSnapshot{
		FiveHour:   usageWindowSnapshot{Available: true, Percent: 42, ResetAt: time.Unix(1000, 0)},
		Weekly:     usageWindowSnapshot{Available: false},
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
	if got.Weekly.Available {
		t.Fatalf("Weekly=%+v, want Available=false preserved across the round trip", got.Weekly)
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
