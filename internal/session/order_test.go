package session

import (
	"testing"
	"time"
)

func TestSortOverviewPinnedFirst(t *testing.T) {
	now := time.Now()
	sessions := []Session{
		{Key: Key{ID: "a"}, CreatedAt: now.Add(-time.Hour)},
		{Key: Key{ID: "b"}, CreatedAt: now, Pinned: true},
	}
	SortOverview(sessions)
	if sessions[0].Key.ID != "b" {
		t.Fatalf("pinned session must sort first regardless of CreatedAt: %+v", sessions)
	}
}

func TestSortOverviewNewestFirstWithinGroup(t *testing.T) {
	now := time.Now()
	sessions := []Session{
		{Key: Key{ID: "old"}, CreatedAt: now.Add(-time.Hour)},
		{Key: Key{ID: "new"}, CreatedAt: now},
	}
	SortOverview(sessions)
	if sessions[0].Key.ID != "new" || sessions[1].Key.ID != "old" {
		t.Fatalf("want newest first, got %+v", sessions)
	}
}

func TestSortOverviewIgnoresActivityAndRuntime(t *testing.T) {
	// The DesignDoc is explicit that activity/runtime changes alone must
	// never reorder the list -- only CreatedAt and pin state do, so a
	// background refresh never moves the row a user is currently looking
	// at just because its status changed.
	now := time.Now()
	sessions := []Session{
		{Key: Key{ID: "a"}, CreatedAt: now, Activity: ActivityWorking, Runtime: RuntimeDetached},
		{Key: Key{ID: "b"}, CreatedAt: now.Add(-time.Minute), Activity: ActivityFailed, Runtime: RuntimeStopped},
	}
	before := append([]Session(nil), sessions...)
	SortOverview(sessions)
	if sessions[0].Key != before[0].Key || sessions[1].Key != before[1].Key {
		t.Fatalf("activity/runtime must not affect order: got %+v", sessions)
	}
}

func TestSortOverviewBreaksTiesByKey(t *testing.T) {
	now := time.Now()
	sessions := []Session{
		{Key: Key{Provider: ProviderCodex, ID: "z"}, CreatedAt: now},
		{Key: Key{Provider: ProviderClaude, ID: "a"}, CreatedAt: now},
	}
	SortOverview(sessions)
	if sessions[0].Key.String() != "claude:a" {
		t.Fatalf("equal CreatedAt must break ties by key string: %+v", sessions)
	}
}
