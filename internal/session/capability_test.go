package session

import "testing"

func TestActionsAvailableDefaultsFalseWhenAbsent(t *testing.T) {
	var a Actions
	if a.Available(ActionStop) {
		t.Fatal("absent action must default to unavailable")
	}
	if a.Reason(ActionStop) == "" {
		t.Fatal("an unavailable action must always report a non-empty reason")
	}
}

func TestActionsWithIsImmutable(t *testing.T) {
	base := Actions{ActionStop: {Available: true}}
	next := base.With(ActionRename, Availability{Available: true})
	if base.Available(ActionRename) {
		t.Fatal("With must not mutate the receiver")
	}
	if !next.Available(ActionStop) || !next.Available(ActionRename) {
		t.Fatalf("With must preserve existing entries alongside the new one: %+v", next)
	}
}

func TestNormalizeDeniesArchiveWhileActive(t *testing.T) {
	s := Session{Activity: ActivityWorking, Actions: Actions{ActionArchive: {Available: true}, ActionStop: {Available: true}}}
	got := Normalize(s)
	if got.Available(ActionArchive) {
		t.Fatal("an active session must not be archivable")
	}
	if !got.Available(ActionStop) {
		t.Fatal("Stop availability must be untouched by the active-session invariant")
	}
}

func TestNormalizeLeavesInactiveSessionActionsUntouched(t *testing.T) {
	s := Session{Activity: ActivityCompleted, Actions: Actions{ActionArchive: {Available: true}, ActionRename: {Available: true}}}
	got := Normalize(s)
	if !got.Available(ActionArchive) || !got.Available(ActionRename) {
		t.Fatalf("inactive session actions must pass through unchanged: %+v", got)
	}
}

func TestNormalizeArchivedSessionHasNoAvailableAction(t *testing.T) {
	s := Session{Archived: true, Actions: Actions{ActionStop: {Available: true}, ActionRename: {Available: true}, ActionOpen: {Available: true}}}
	got := Normalize(s)
	for id := range got {
		if got.Available(id) {
			t.Fatalf("archived session must expose no available action, got %s available", id)
		}
	}
}

func TestNormalizeDoesNotMutateSessionActions(t *testing.T) {
	original := Actions{ActionArchive: {Available: true}}
	s := Session{Activity: ActivityWorking, Actions: original}
	_ = Normalize(s)
	if !original.Available(ActionArchive) {
		t.Fatal("Normalize must not mutate the Session's own Actions map")
	}
}
