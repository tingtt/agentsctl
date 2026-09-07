package localstate

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
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

func TestStartRunRejectsDuplicateID(t *testing.T) {
	s := New(filepath.Join(t.TempDir(), "state.json"))
	if err := s.StartRun(Run{ID: "r1", State: "starting"}); err != nil {
		t.Fatal(err)
	}
	if err := s.StartRun(Run{ID: "r1", State: "starting"}); err == nil {
		t.Fatal("StartRun must reject a run ID that already exists")
	}
}

func TestMarkRunStoppedClearsPID(t *testing.T) {
	s := New(filepath.Join(t.TempDir(), "state.json"))
	if err := s.StartRun(Run{ID: "r1", State: "running", PID: 4242}); err != nil {
		t.Fatal(err)
	}
	stopped, err := s.MarkRunStopped("r1")
	if err != nil {
		t.Fatal(err)
	}
	if stopped.State != "stopped" || stopped.PID != 0 {
		t.Fatalf("stopped=%+v", stopped)
	}
	runs, err := s.Runs()
	if err != nil || runs["r1"].PID != 0 {
		t.Fatalf("runs=%+v err=%v", runs, err)
	}
}

func TestMarkAllRunningStaleOnlyAffectsRunningOrStarting(t *testing.T) {
	s := New(filepath.Join(t.TempDir(), "state.json"))
	_ = s.StartRun(Run{ID: "running", State: "running"})
	_ = s.StartRun(Run{ID: "starting", State: "starting"})
	_ = s.StartRun(Run{ID: "stopped", State: "stopped"})
	if err := s.MarkAllRunningStale("supervisor restarted"); err != nil {
		t.Fatal(err)
	}
	runs, err := s.Runs()
	if err != nil {
		t.Fatal(err)
	}
	if runs["running"].State != "stale" || runs["starting"].State != "stale" {
		t.Fatalf("runs=%+v", runs)
	}
	if runs["stopped"].State != "stopped" {
		t.Fatalf("an already-stopped run must not be touched: %+v", runs["stopped"])
	}
}

// TestDeleteTerminalUnboundRunRechecksAtDeleteTime fixes the atomicity
// guarantee: the terminal+unbound shape is re-verified inside the same
// locked transaction as the delete, so a run that no longer matches by
// delete time (bound to a thread since, or left its terminal state) must
// survive, never be removed based on a caller's earlier, now-stale read.
func TestDeleteTerminalUnboundRunRechecksAtDeleteTime(t *testing.T) {
	isTerminal := func(state string) bool { return state == "failed" || state == "stale" || state == "stopped" }
	cases := []struct {
		name     string
		seed     *Run // nil means the run is absent
		wantKept bool
	}{
		{name: "bound to a thread", seed: &Run{ID: "r", SessionID: "thread-abc", State: "failed"}, wantKept: true},
		{name: "left terminal state", seed: &Run{ID: "r", State: "running"}, wantKept: true},
		{name: "already deleted", seed: nil, wantKept: false},
		{name: "still terminal and unbound", seed: &Run{ID: "r", State: "stopped"}, wantKept: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := New(filepath.Join(t.TempDir(), "state.json"))
			if tc.seed != nil {
				if err := s.StartRun(*tc.seed); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.DeleteTerminalUnboundRun("r", isTerminal); err != nil {
				t.Fatal(err)
			}
			runs, err := s.Runs()
			if err != nil {
				t.Fatal(err)
			}
			_, ok := runs["r"]
			if tc.wantKept && !ok {
				t.Fatal("run was deleted despite no longer matching the unbound-terminal shape")
			}
			if !tc.wantKept && ok {
				t.Fatal("run was not deleted despite matching the unbound-terminal shape")
			}
		})
	}
}

func TestReconcileRunsPersistsOnlyChangedRuns(t *testing.T) {
	s := New(filepath.Join(t.TempDir(), "state.json"))
	_ = s.StartRun(Run{ID: "bind-me", State: "running"})
	_ = s.StartRun(Run{ID: "leave-me", State: "running"})
	err := s.ReconcileRuns(func(id string, r Run) (Run, bool) {
		if id != "bind-me" {
			return r, false
		}
		r.SessionID = "thread-1"
		return r, true
	})
	if err != nil {
		t.Fatal(err)
	}
	runs, err := s.Runs()
	if err != nil {
		t.Fatal(err)
	}
	if runs["bind-me"].SessionID != "thread-1" {
		t.Fatalf("bind-me=%+v", runs["bind-me"])
	}
	if runs["leave-me"].SessionID != "" {
		t.Fatalf("leave-me must be untouched: %+v", runs["leave-me"])
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
			if err := s.StartRun(Run{ID: fmt.Sprint(i)}); err != nil {
				t.Errorf("start run: %v", err)
			}
		}(i)
	}
	wg.Wait()
	runs, err := a.Runs()
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 20 {
		t.Fatalf("runs=%d", len(runs))
	}
}
