package sessionctl

import (
	"context"
	"testing"

	"github.com/tingtt/agentsctl/internal/session"
)

var (
	runKey    = session.Key{Provider: session.ProviderCodex, ID: "run-1"}
	threadKey = session.Key{Provider: session.ProviderCodex, ID: "thread-1"}
)

func startingRow() session.Session {
	return session.Session{Key: runKey, Name: "Starting", CWD: "/work", Activity: session.ActivityStarting}
}

func boundRow() session.Session {
	return session.Session{Key: threadKey, CWD: "/work", Activity: session.ActivityWorking, PreviousKeys: []session.Key{runKey}}
}

func mergeAll(c Controller, rows ...session.Session) []session.Session {
	return c.MergeSessions(rows, session.Scope{Directory: session.ScopeAll})
}

func pinnedKeys(t *testing.T, p *fakePinStore) map[string]bool {
	t.Helper()
	m, err := p.ListPinned()
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// Pin the Starting row, then the catalog shows it bound: the pin follows
// the session to its canonical key and the provisional key is gone.
func TestPinOnStartingRowMigratesToBoundThreadKey(t *testing.T) {
	pins := &fakePinStore{}
	c := Controller{Pins: pins}

	if got := mergeAll(c, startingRow()); len(got) != 1 || got[0].Pinned {
		t.Fatalf("unpinned starting row: %+v", got)
	}
	if _, err := c.TogglePin(runKey); err != nil {
		t.Fatal(err)
	}
	if got := mergeAll(c, startingRow()); !got[0].Pinned {
		t.Fatalf("starting row must show its pin: %+v", got)
	}

	got := mergeAll(c, boundRow())
	if len(got) != 1 || got[0].Key != threadKey || !got[0].Pinned {
		t.Fatalf("bound row must be pinned: %+v", got)
	}
	if want := map[string]bool{"codex:thread-1": true}; len(pinnedKeys(t, pins)) != 1 || !pinnedKeys(t, pins)["codex:thread-1"] {
		t.Fatalf("pins=%v, want %v", pinnedKeys(t, pins), want)
	}
	// Re-merging (every later refresh) is stable.
	if got := mergeAll(c, boundRow()); !got[0].Pinned || len(pinnedKeys(t, pins)) != 1 {
		t.Fatalf("second refresh: %+v pins=%v", got, pinnedKeys(t, pins))
	}
}

// The other ordering: binding is already true in the store/catalog, but the
// user's pin still lands on the provisional key because their view had not
// caught up. Whenever the catalog next arrives the outcome is the same.
func TestPinOnProvisionalKeyAfterBindingStillConverges(t *testing.T) {
	pins := &fakePinStore{}
	c := Controller{Pins: pins}
	mergeAll(c, boundRow())                        // snapshot with the binding arrives first ...
	if _, err := c.TogglePin(runKey); err != nil { // ... then a pin on the old key
		t.Fatal(err)
	}
	got := mergeAll(c, boundRow())
	if !got[0].Pinned {
		t.Fatalf("late pin on the provisional key must reach the thread: %+v", got)
	}
	if k := pinnedKeys(t, pins); len(k) != 1 || !k["codex:thread-1"] {
		t.Fatalf("pins=%v", k)
	}
}

// Migration is idempotent whichever way pin and snapshot interleave, and
// never leaves duplicate or provisional pin metadata.
func TestPinAndBindingOrderingsConvergeToSameState(t *testing.T) {
	type step string
	const (
		pin      step = "pin"
		snapshot step = "snapshot"
	)
	// Each order ends with a snapshot showing the binding.
	for _, order := range [][]step{
		{pin, snapshot},
		{snapshot, pin, snapshot},
		{pin, snapshot, snapshot},
		{snapshot, snapshot, pin, snapshot},
		{pin, pin, pin, snapshot}, // toggled on, off, on
	} {
		pins := &fakePinStore{}
		c := Controller{Pins: pins}
		for _, s := range order {
			switch s {
			case pin:
				// Always the provisional key: the user's view (still showing
				// the Starting row) is what produced the pin.
				if _, err := c.TogglePin(runKey); err != nil {
					t.Fatal(err)
				}
			case snapshot:
				mergeAll(c, boundRow())
			}
		}
		got := mergeAll(c, boundRow())
		k := pinnedKeys(t, pins)
		if !got[0].Pinned || len(k) != 1 || !k["codex:thread-1"] {
			t.Fatalf("order %v: row=%+v pins=%v", order, got[0], k)
		}
	}
}

// After migration the thread is a normal session: it can be unpinned, and
// the old provisional key cannot pin it again.
func TestMigratedPinCanBeUnpinnedAndStaysUnpinned(t *testing.T) {
	pins := &fakePinStore{}
	c := Controller{Pins: pins}
	if _, err := c.TogglePin(runKey); err != nil {
		t.Fatal(err)
	}
	mergeAll(c, boundRow())

	res, err := c.TogglePin(threadKey)
	if err != nil || res.Patch == nil || res.Patch.Pinned == nil || *res.Patch.Pinned {
		t.Fatalf("unpin thread: res=%+v err=%v", res, err)
	}
	for range 2 {
		if got := mergeAll(c, boundRow()); got[0].Pinned {
			t.Fatalf("stale provisional pin resurrected: %+v pins=%v", got, pinnedKeys(t, pins))
		}
	}
	if k := pinnedKeys(t, pins); len(k) != 0 {
		t.Fatalf("pins=%v, want none", k)
	}
}

// A destination that is already pinned and a leftover provisional pin
// collapse into one pin.
func TestMigrationDoesNotDuplicateExistingDestinationPin(t *testing.T) {
	pins := &fakePinStore{pinned: map[string]bool{"codex:run-1": true, "codex:thread-1": true}}
	c := Controller{Pins: pins}
	got := mergeAll(c, boundRow())
	if !got[0].Pinned {
		t.Fatalf("row=%+v", got[0])
	}
	if k := pinnedKeys(t, pins); len(k) != 1 || !k["codex:thread-1"] {
		t.Fatalf("pins=%v", k)
	}
}

// Without a provider-stated continuity nothing is migrated: an unbound
// run's pin stays on the run ID, and an unrelated bound-looking row is left
// alone.
func TestNoContinuityMeansNoPinMigration(t *testing.T) {
	pins := &fakePinStore{pinned: map[string]bool{"codex:run-1": true}}
	c := Controller{Pins: pins}
	unrelated := session.Session{Key: threadKey, CWD: "/work", Activity: session.ActivityWorking}
	got := mergeAll(c, startingRow(), unrelated)
	for _, r := range got {
		if r.Key == runKey && !r.Pinned {
			t.Fatalf("unbound run lost its pin: %+v", got)
		}
		if r.Key == threadKey && r.Pinned {
			t.Fatalf("pin must not be inferred onto an unrelated thread (same CWD, close in time): %+v", got)
		}
	}
	if len(pins.migrations) != 0 {
		t.Fatalf("migrations=%v", pins.migrations)
	}
}

// A store failure must not hide the pin from the user, and must not drop
// the persisted provisional pin: the next refresh retries the migration.
func TestPinMigrationFailureStillShowsPinAndRetries(t *testing.T) {
	pins := &fakePinStore{pinned: map[string]bool{"codex:run-1": true}, migrateErr: errBoom}
	c := Controller{Pins: pins}
	if got := mergeAll(c, boundRow()); !got[0].Pinned {
		t.Fatalf("row=%+v", got[0])
	}
	if !pinnedKeys(t, pins)["codex:run-1"] {
		t.Fatal("provisional pin must survive a failed migration")
	}
	pins.migrateErr = nil
	if got := mergeAll(c, boundRow()); !got[0].Pinned {
		t.Fatalf("row=%+v", got[0])
	}
	if k := pinnedKeys(t, pins); len(k) != 1 || !k["codex:thread-1"] {
		t.Fatalf("pins=%v", k)
	}
}

// End to end through Load: continuity metadata survives the provider
// boundary (actionsFor narrowing included).
func TestLoadCarriesPreviousKeysAndMigratesPin(t *testing.T) {
	pins := &fakePinStore{pinned: map[string]bool{"codex:run-1": true}}
	c := Controller{
		Providers: []Source{fakeSource{id: session.ProviderCodex, rows: []session.Session{boundRow()}}},
		Pins:      pins,
	}
	got := c.Load(context.Background(), session.Scope{Directory: session.ScopeAll})
	if len(got.Sessions) != 1 || !got.Sessions[0].Pinned || len(got.Sessions[0].PreviousKeys) != 1 || got.Sessions[0].PreviousKeys[0] != runKey {
		t.Fatalf("sessions=%+v", got.Sessions)
	}
}

func codexRow(id string, previous ...session.Key) session.Session {
	return session.Session{Key: session.Key{Provider: session.ProviderCodex, ID: id}, CWD: "/work", PreviousKeys: previous}
}

// pinnedRows reports which of rows the merge shows as pinned.
func pinnedRows(rows []session.Session) map[session.Key]bool {
	m := map[session.Key]bool{}
	for _, r := range rows {
		m[r.Key] = r.Pinned
	}
	return m
}

// Continuity that does not pass session.IdentityTransitions must not move
// a pin, whatever the row order, and must not even reach the store.
func TestInvalidContinuityNeverMigratesPin(t *testing.T) {
	claude := session.Key{Provider: session.ProviderClaude, ID: "abc"}
	tests := []struct {
		name   string
		pinned string
		rows   func() []session.Session
		want   map[session.Key]bool
	}{
		{
			name:   "ambiguous destination",
			pinned: "codex:run-1",
			rows: func() []session.Session {
				return []session.Session{codexRow("thread-1", runKey), codexRow("thread-2", runKey)}
			},
			want: map[session.Key]bool{threadKey: false, {Provider: session.ProviderCodex, ID: "thread-2"}: false},
		},
		{
			name:   "ambiguous destination, reversed order",
			pinned: "codex:run-1",
			rows: func() []session.Session {
				return []session.Session{codexRow("thread-2", runKey), codexRow("thread-1", runKey)}
			},
			want: map[session.Key]bool{threadKey: false, {Provider: session.ProviderCodex, ID: "thread-2"}: false},
		},
		{
			name:   "old key still present",
			pinned: "codex:run-1",
			rows:   func() []session.Session { return []session.Session{startingRow(), boundRow()} },
			want:   map[session.Key]bool{runKey: true, threadKey: false},
		},
		{
			name:   "cross-provider continuity",
			pinned: "claude:abc",
			rows:   func() []session.Session { return []session.Session{codexRow("thread-1", claude)} },
			want:   map[session.Key]bool{threadKey: false},
		},
		{
			name:   "self transition",
			pinned: "codex:thread-1",
			rows:   func() []session.Session { return []session.Session{codexRow("thread-1", threadKey)} },
			want:   map[session.Key]bool{threadKey: true},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pins := &fakePinStore{pinned: map[string]bool{tc.pinned: true}}
			got := mergeAll(Controller{Pins: pins}, tc.rows()...)
			for key, want := range tc.want {
				if pinnedRows(got)[key] != want {
					t.Errorf("%v pinned=%v, want %v", key, !want, want)
				}
			}
			if k := pinnedKeys(t, pins); len(k) != 1 || !k[tc.pinned] {
				t.Errorf("persisted pins=%v, want only %s", k, tc.pinned)
			}
			if len(pins.migrations) != 0 {
				t.Errorf("MigratePinned must not be called: %v", pins.migrations)
			}
		})
	}
}

// Independent valid transitions all migrate, and an invalid one next to
// them does not.
func TestValidTransitionsMigrateWhileInvalidOnesAreSkipped(t *testing.T) {
	run2 := session.Key{Provider: session.ProviderCodex, ID: "run-2"}
	run3 := session.Key{Provider: session.ProviderCodex, ID: "run-3"}
	pins := &fakePinStore{pinned: map[string]bool{"codex:run-1": true, "codex:run-2": true, "codex:run-3": true}}
	got := mergeAll(Controller{Pins: pins},
		codexRow("thread-1", runKey),
		codexRow("thread-2", run2),
		codexRow("thread-3", run3), codexRow("thread-4", run3), // run-3 is ambiguous
	)
	want := map[string]bool{"codex:thread-1": true, "codex:thread-2": true, "codex:run-3": true}
	if k := pinnedKeys(t, pins); len(k) != len(want) || !k["codex:thread-1"] || !k["codex:thread-2"] || !k["codex:run-3"] {
		t.Fatalf("persisted pins=%v, want %v", k, want)
	}
	shown := pinnedRows(got)
	if !shown[threadKey] || shown[session.Key{Provider: session.ProviderCodex, ID: "thread-3"}] || shown[session.Key{Provider: session.ProviderCodex, ID: "thread-4"}] {
		t.Fatalf("shown=%v", shown)
	}
}
