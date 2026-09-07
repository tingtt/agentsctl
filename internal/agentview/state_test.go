package agentview

import (
	"errors"
	"testing"
	"time"

	"github.com/tingtt/agentsctl/internal/session"
	"github.com/tingtt/agentsctl/internal/sessionctl"
)

func key(id string) session.Key { return session.Key{Provider: session.ProviderClaude, ID: id} }

var errBoomState = errors.New("boom")

// TestSetRowsPreservesSelectionAcrossReorder is the core guarantee behind
// switching selection to session.Key (see the DesignDoc's "selection
// identity は session.Key"): a row's position in the slice can change
// completely (pin promotion, provider reload reordering) and selection
// must still track the same session, not whatever now occupies its old
// index.
func TestSetRowsPreservesSelectionAcrossReorder(t *testing.T) {
	s := NewState()
	s.SetRows([]session.Session{{Key: key("a")}, {Key: key("b")}})
	s.selectIndex(1) // select "b"
	// Reload with "b" now first (e.g. it became pinned/newest).
	s.SetRows([]session.Session{{Key: key("b")}, {Key: key("a")}})
	got, ok := s.SelectedRow()
	if !ok || got.Key != key("b") {
		t.Fatalf("selection did not follow session b across reorder: got=%+v ok=%v", got, ok)
	}
	if s.SelectedIndex() != 0 {
		t.Fatalf("SelectedIndex=%d, want 0 (b's new position)", s.SelectedIndex())
	}
}

// TestSetRowsFallsBackToFirstRowWhenSelectionDisappears covers the
// session-removed case (e.g. a successful archive): selection must not
// silently point at an arbitrary different row that happens to now
// occupy the old numeric index.
func TestSetRowsFallsBackToFirstRowWhenSelectionDisappears(t *testing.T) {
	s := NewState()
	s.SetRows([]session.Session{{Key: key("a")}, {Key: key("b")}})
	s.selectIndex(1) // select "b"
	s.SetRows([]session.Session{{Key: key("c")}})
	got, ok := s.SelectedRow()
	if !ok || got.Key != key("c") {
		t.Fatalf("expected fallback to the remaining row, got=%+v ok=%v", got, ok)
	}
}

func TestSetRowsNoSelectionWhenEmpty(t *testing.T) {
	s := NewState()
	s.SetRows(nil)
	if _, ok := s.SelectedRow(); ok {
		t.Fatal("an empty catalog must have no selection")
	}
	if s.SelectedIndex() != -1 {
		t.Fatalf("SelectedIndex=%d, want -1", s.SelectedIndex())
	}
}

// TestSetRowsFollowsRenameTargetEvenWithoutPriorSelection covers renaming
// mid-refresh: the row being renamed must remain resolvable by key even
// if it wasn't the tracked selection before this reload (mirrors the
// pre-refactor "a refresh/reorder must not retarget the pending rename"
// guarantee).
func TestSetRowsFollowsRenameTargetDuringRename(t *testing.T) {
	s := NewState()
	s.SetRows([]session.Session{{Key: key("a")}, {Key: key("b")}})
	s.selectIndex(0)
	s.Rename = Rename{Active: true, Target: key("b"), Draft: "new name"}
	s.SetRows([]session.Session{{Key: key("b")}, {Key: key("a")}})
	got, ok := s.SelectedRow()
	if !ok || got.Key != key("b") {
		t.Fatalf("selection must follow the rename target across reload: got=%+v ok=%v", got, ok)
	}
}

func TestApplyPatchPinReordersWithoutTouchingSelection(t *testing.T) {
	s := NewState()
	s.SetRows([]session.Session{{Key: key("a")}, {Key: key("b")}})
	s.selectIndex(1) // select "b"
	pinned := true
	s.ApplyPatch(sessionctl.Patch{Key: key("b"), Pinned: &pinned})
	got, ok := s.SelectedRow()
	if !ok || got.Key != key("b") || !got.Pinned {
		t.Fatalf("got=%+v ok=%v", got, ok)
	}
	if s.SelectedIndex() != 0 {
		t.Fatalf("pinned session must sort first: index=%d", s.SelectedIndex())
	}
}

func TestApplyPatchRenameUpdatesNameWithoutReordering(t *testing.T) {
	s := NewState()
	s.SetRows([]session.Session{
		{Key: key("a"), Name: "old", CreatedAt: time.Unix(1, 0)},
		{Key: key("b"), Name: "b", CreatedAt: time.Unix(2, 0)},
	})
	name := "new name"
	s.ApplyPatch(sessionctl.Patch{Key: key("a"), Name: &name})
	found := false
	for _, row := range s.Rows {
		if row.Key == key("a") {
			found = true
			if row.Name != "new name" {
				t.Fatalf("name patch was not applied: %+v", row)
			}
		}
	}
	if !found {
		t.Fatal("row a missing")
	}
}

// TestComposerCWDFollowsSelectedSession fixes #14's composer cwd tracking:
// the composer's directory context is the selected session's own CWD, not
// StartupCWD (the listing scope anchor), and it must follow selection
// moving to a session in a different directory.
func TestComposerCWDFollowsSelectedSession(t *testing.T) {
	s := NewState()
	s.StartupCWD = "/start"
	s.SetRows([]session.Session{
		{Key: key("a"), CWD: "/work/repo-a"},
		{Key: key("b"), CWD: "/work/repo-b"},
	})
	s.selectIndex(0)
	if got := s.ComposerCWD(); got != "/work/repo-a" {
		t.Fatalf("ComposerCWD()=%q, want the selected session's own CWD", got)
	}
	s.selectIndex(1)
	if got := s.ComposerCWD(); got != "/work/repo-b" {
		t.Fatalf("ComposerCWD()=%q, want it to follow selection to repo-b", got)
	}
}

// TestApplyUsageUpdateUpsertsSuccessfulProviderWithoutTouchingOthers fixes
// the incremental-update contract: a successful update for one provider
// replaces just that provider's entry, leaving an already-applied
// different provider's entry untouched -- the async counterpart to
// sessionctl.Controller.Usage's own partial-failure guarantee.
func TestApplyUsageUpdateUpsertsSuccessfulProviderWithoutTouchingOthers(t *testing.T) {
	s := NewState()
	s.Usage = []session.Usage{{Provider: session.ProviderCodex, FiveHour: session.UsageWindow{Available: true, Percent: 10}}}
	s.ApplyUsageUpdate(session.ProviderClaude, session.Usage{Provider: session.ProviderClaude, FiveHour: session.UsageWindow{Available: true, Percent: 70}}, nil)
	if len(s.Usage) != 2 {
		t.Fatalf("Usage=%+v, want both providers present", s.Usage)
	}
	if s.Usage[0].Provider != session.ProviderClaude || s.Usage[0].FiveHour.Percent != 70 {
		t.Fatalf("claude entry=%+v", s.Usage[0])
	}
	if s.Usage[1].Provider != session.ProviderCodex || s.Usage[1].FiveHour.Percent != 10 {
		t.Fatalf("codex entry unexpectedly changed: %+v", s.Usage[1])
	}
}

// TestApplyUsageUpdateReplacesStalePreviousValueForSameProvider fixes that
// a second update for the SAME provider overwrites its own prior entry
// rather than appending a duplicate.
func TestApplyUsageUpdateReplacesStalePreviousValueForSameProvider(t *testing.T) {
	s := NewState()
	s.ApplyUsageUpdate(session.ProviderClaude, session.Usage{Provider: session.ProviderClaude, FiveHour: session.UsageWindow{Available: true, Percent: 10}}, nil)
	s.ApplyUsageUpdate(session.ProviderClaude, session.Usage{Provider: session.ProviderClaude, FiveHour: session.UsageWindow{Available: true, Percent: 90}}, nil)
	if len(s.Usage) != 1 || s.Usage[0].FiveHour.Percent != 90 {
		t.Fatalf("Usage=%+v, want a single, updated claude entry", s.Usage)
	}
}

// TestApplyUsageUpdateErrorRemovesThatProviderOnly fixes that a failed
// update removes just its own provider's entry (matching Controller.Usage's
// "omit on failure, never a fake 0%" contract) without disturbing a
// different provider's already-applied usage.
func TestApplyUsageUpdateErrorRemovesThatProviderOnly(t *testing.T) {
	s := NewState()
	s.Usage = []session.Usage{
		{Provider: session.ProviderClaude, FiveHour: session.UsageWindow{Available: true, Percent: 70}},
		{Provider: session.ProviderCodex, FiveHour: session.UsageWindow{Available: true, Percent: 10}},
	}
	s.ApplyUsageUpdate(session.ProviderClaude, session.Usage{}, errBoomState)
	if len(s.Usage) != 1 || s.Usage[0].Provider != session.ProviderCodex {
		t.Fatalf("Usage=%+v, want only codex left after claude's update failed", s.Usage)
	}
}

// TestComposerCWDFallsBackToStartupWhenNoSessionSelectable fixes the #14
// safe-fallback rule: an empty catalog (nothing to select) must still let
// a new prompt dispatch, using StartupCWD rather than an empty string.
func TestComposerCWDFallsBackToStartupWhenNoSessionSelectable(t *testing.T) {
	s := NewState()
	s.StartupCWD = "/start"
	if got := s.ComposerCWD(); got != "/start" {
		t.Fatalf("ComposerCWD()=%q, want StartupCWD fallback %q", got, "/start")
	}
}
