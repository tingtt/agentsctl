package localstate

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPinRoundTripsAcrossReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s := New(path)
	if pinned, err := s.TogglePinned("codex:session"); err != nil || !pinned {
		t.Fatalf("first toggle: pinned=%v err=%v", pinned, err)
	}
	reopened := New(path)
	pins, err := reopened.ListPinned()
	if err != nil || !pins["codex:session"] {
		t.Fatalf("pins=%v err=%v", pins, err)
	}
	if pinned, err := reopened.TogglePinned("codex:session"); err != nil || pinned {
		t.Fatalf("second toggle: pinned=%v err=%v", pinned, err)
	}
}

func TestClaudeArchiveOverlayRoundTrips(t *testing.T) {
	s := New(filepath.Join(t.TempDir(), "state.json"))
	if err := s.SetClaudeArchived("c1"); err != nil {
		t.Fatal(err)
	}
	archived, _, err := s.ClaudeState()
	if err != nil || !archived["c1"] {
		t.Fatalf("archived=%v err=%v", archived, err)
	}
	if err := s.ClearClaudeArchived("c1"); err != nil {
		t.Fatal(err)
	}
	archived, _, err = s.ClaudeState()
	if err != nil || archived["c1"] {
		t.Fatalf("archived after clear=%v err=%v", archived, err)
	}
}

func TestSyncClaudeCreatedAtOverwritesAndForgetsAbsentSessions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s := New(path)
	t1, t2 := time.UnixMilli(1788438867864), time.UnixMilli(1788439818830)
	if err := s.SyncClaudeCreatedAt(map[string]time.Time{"a": t1, "b": t1}, map[string]bool{"a": true, "b": true}); err != nil {
		t.Fatal(err)
	}
	// "a" is still listed but running (no authoritative value); "b" is
	// re-observed stopped with a different value; "c" is new.
	if err := s.SyncClaudeCreatedAt(map[string]time.Time{"b": t2, "c": t2}, map[string]bool{"a": true, "b": true, "c": true}); err != nil {
		t.Fatal(err)
	}
	known, err := New(path).ClaudeCreatedAt()
	if err != nil || len(known) != 3 || !known["a"].Equal(t1) || !known["b"].Equal(t2) || !known["c"].Equal(t2) {
		t.Fatalf("known=%v err=%v, want a=T1 b=T2 c=T2", known, err)
	}
	if err := s.SyncClaudeCreatedAt(nil, map[string]bool{"c": true}); err != nil {
		t.Fatal(err)
	}
	if known, err := s.ClaudeCreatedAt(); err != nil || len(known) != 1 || !known["c"].Equal(t2) {
		t.Fatalf("known=%v err=%v, want only c", known, err)
	}
}

func TestClearLegacyClaudeNameOnlyRemovesGivenID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	// Seed two legacy overlay entries directly through the persisted file
	// format, exactly as an older agentsctl build would have -- the current
	// code path never writes this field (see ClearLegacyClaudeName's doc
	// comment), so a production Store method cannot be used to set it up.
	content := `{"claudeNames":{"c1":"old c1 name","c2":"old c2 name"}}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	s := New(path)
	if err := s.ClearLegacyClaudeName("c1"); err != nil {
		t.Fatal(err)
	}
	_, legacyNames, err := s.ClaudeState()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := legacyNames["c1"]; ok {
		t.Fatalf("c1 must be cleared: %v", legacyNames)
	}
	if legacyNames["c2"] != "old c2 name" {
		t.Fatalf("clearing c1 must not affect c2: %v", legacyNames)
	}
}

func TestStoreSerializesConcurrentProcessOwners(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	a, b := New(path), New(path)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s := a
			if i%2 == 1 {
				s = b
			}
			if _, err := s.TogglePinned(fmt.Sprint("claude:", i)); err != nil {
				t.Errorf("toggle pin: %v", err)
			}
		}(i)
	}
	wg.Wait()
	pins, err := a.ListPinned()
	if err != nil {
		t.Fatal(err)
	}
	if len(pins) != 20 {
		t.Fatalf("pins=%d", len(pins))
	}
}

func TestMigratePinnedMovesPinAndIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s := New(path)
	if _, err := s.TogglePinned("codex:run-1"); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := s.MigratePinned("codex:run-1", "codex:thread-1"); err != nil {
			t.Fatal(err)
		}
	}
	pins, err := New(path).ListPinned()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(pins, map[string]bool{"codex:thread-1": true}) {
		t.Fatalf("pins=%v, want only codex:thread-1", pins)
	}
}

func TestMigratePinnedWithoutSourcePinChangesNothing(t *testing.T) {
	s := New(filepath.Join(t.TempDir(), "state.json"))
	// A destination the user pinned or deliberately left unpinned must not
	// be altered by a migration that has no source pin to carry over.
	if _, err := s.TogglePinned("codex:pinned"); err != nil {
		t.Fatal(err)
	}
	for _, to := range []string{"codex:pinned", "codex:unpinned"} {
		if err := s.MigratePinned("codex:run-1", to); err != nil {
			t.Fatal(err)
		}
	}
	pins, _ := s.ListPinned()
	if !reflect.DeepEqual(pins, map[string]bool{"codex:pinned": true}) {
		t.Fatalf("pins=%v", pins)
	}
}

func TestMigratePinnedMergesWithAlreadyPinnedDestination(t *testing.T) {
	s := New(filepath.Join(t.TempDir(), "state.json"))
	for _, k := range []string{"codex:run-1", "codex:thread-1"} {
		if _, err := s.TogglePinned(k); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.MigratePinned("codex:run-1", "codex:thread-1"); err != nil {
		t.Fatal(err)
	}
	pins, _ := s.ListPinned()
	if !reflect.DeepEqual(pins, map[string]bool{"codex:thread-1": true}) {
		t.Fatalf("pins=%v, want a single codex:thread-1 pin", pins)
	}
}

func TestMigratePinnedToSameKeyKeepsPin(t *testing.T) {
	s := New(filepath.Join(t.TempDir(), "state.json"))
	if _, err := s.TogglePinned("codex:a"); err != nil {
		t.Fatal(err)
	}
	if err := s.MigratePinned("codex:a", "codex:a"); err != nil {
		t.Fatal(err)
	}
	if pins, _ := s.ListPinned(); !pins["codex:a"] {
		t.Fatalf("pins=%v", pins)
	}
}

// TestLegacyRunsFieldIsIgnored fixes that a state.json written while Codex
// sessions still ran as locally managed runs keeps loading: its "runs"
// records decode as an unknown field, every other kind of state survives,
// and the next write drops them.
func TestLegacyRunsFieldIsIgnored(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	content := `{"pinned":{"codex:thread-1":true},"runs":{"r1":{"id":"r1","provider":"codex","sessionId":"thread-1","cwd":"/work","pid":4242,"state":"running","baseline":["old"],"startedAt":"2026-01-01T00:00:00Z","pendingRename":"name","renameError":"boom"}}}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	s := New(path)
	pins, err := s.ListPinned()
	if err != nil || !pins["codex:thread-1"] {
		t.Fatalf("pins=%v err=%v", pins, err)
	}
	if _, err := s.TogglePinned("claude:session"); err != nil {
		t.Fatalf("write over a legacy state file: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"runs"`) {
		t.Fatalf("legacy runs survived a write: %s", b)
	}
}
