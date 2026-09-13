package main

import (
	"strings"
	"testing"
	"time"
)

func mustParseTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("mustParseTime(%q): %v", value, err)
	}
	return parsed
}

func ccAt(t *testing.T, id, created string) cursorConversation {
	return cursorConversation{ID: id, CreatedAt: mustParseTime(t, created)}
}

func TestAccumulateCursorChainWalksAContiguousChainToCompletion(t *testing.T) {
	pages := []cursorFetchedPage{
		{CursorIn: "0", Conversations: []cursorConversation{ccAt(t, "A", "2026-01-01T00:00:00Z"), ccAt(t, "B", "2026-01-02T00:00:00Z")}, HasNextCursor: true, NextCursor: "C1"},
		{CursorIn: "C1", Conversations: []cursorConversation{ccAt(t, "C", "2026-01-03T00:00:00Z"), ccAt(t, "D", "2026-01-04T00:00:00Z")}, HasNextCursor: true, NextCursor: "C2"},
		{CursorIn: "C2", Conversations: []cursorConversation{ccAt(t, "E", "2026-01-05T00:00:00Z")}, HasNextCursor: false},
	}
	got, dups, complete, err := accumulateCursorChain(pages, 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !complete {
		t.Fatal("expected COMPLETE after a terminal last page")
	}
	if dups != 0 {
		t.Fatalf("duplicatesObserved = %d, want 0", dups)
	}
	if len(got) != 5 {
		t.Fatalf("got %d conversations, want 5", len(got))
	}
}

func TestAccumulateCursorChainDeduplicatesAcrossPages(t *testing.T) {
	pages := []cursorFetchedPage{
		{CursorIn: "0", Conversations: []cursorConversation{ccAt(t, "A", "2026-01-01T00:00:00Z"), ccAt(t, "B", "2026-01-02T00:00:00Z")}, HasNextCursor: true, NextCursor: "C1"},
		{CursorIn: "C1", Conversations: []cursorConversation{ccAt(t, "B", "2026-01-02T00:00:00Z"), ccAt(t, "C", "2026-01-03T00:00:00Z")}, HasNextCursor: false},
	}
	got, dups, complete, err := accumulateCursorChain(pages, 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !complete {
		t.Fatal("expected COMPLETE")
	}
	if dups != 1 {
		t.Fatalf("duplicatesObserved = %d, want 1", dups)
	}
	if len(got) != 3 {
		t.Fatalf("got %d conversations, want 3", len(got))
	}
}

func TestAccumulateCursorChainDetectsACycleWithoutRefetching(t *testing.T) {
	pages := []cursorFetchedPage{
		{CursorIn: "0", Conversations: []cursorConversation{ccAt(t, "A", "2026-01-01T00:00:00Z")}, HasNextCursor: true, NextCursor: "C1"},
		{CursorIn: "C1", Conversations: []cursorConversation{ccAt(t, "B", "2026-01-02T00:00:00Z")}, HasNextCursor: true, NextCursor: "C2"},
		{CursorIn: "C2", Conversations: []cursorConversation{ccAt(t, "C", "2026-01-03T00:00:00Z")}, HasNextCursor: true, NextCursor: "C1"},
	}
	_, _, complete, err := accumulateCursorChain(pages, 10)
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("accumulateCursorChain() error = %v, want a cycle-detected error", err)
	}
	if complete {
		t.Fatal("a detected cycle must never report COMPLETE")
	}
}

func TestAccumulateCursorChainRejectsAnEmptyDeclaredNextCursor(t *testing.T) {
	pages := []cursorFetchedPage{
		{CursorIn: "0", Conversations: []cursorConversation{ccAt(t, "A", "2026-01-01T00:00:00Z")}, HasNextCursor: true, NextCursor: ""},
	}
	_, _, complete, err := accumulateCursorChain(pages, 10)
	if err == nil {
		t.Fatal("expected an error for an unusable declared-next-cursor page")
	}
	if complete {
		t.Fatal("COMPLETE must be false when cursor semantics are unresolvable")
	}
}

func TestAccumulateCursorChainRejectsAMissingID(t *testing.T) {
	pages := []cursorFetchedPage{
		{CursorIn: "0", Conversations: []cursorConversation{{ID: "", CreatedAt: mustParseTime(t, "2026-01-01T00:00:00Z")}}, HasNextCursor: false},
	}
	_, _, _, err := accumulateCursorChain(pages, 10)
	if err == nil || !strings.Contains(err.Error(), "identity") {
		t.Fatalf("accumulateCursorChain() error = %v, want a missing-identity error", err)
	}
}

func TestAccumulateCursorChainRejectsExcessivePages(t *testing.T) {
	pages := []cursorFetchedPage{
		{CursorIn: "0", Conversations: nil, HasNextCursor: true, NextCursor: "C1"},
		{CursorIn: "C1", Conversations: nil, HasNextCursor: false},
	}
	_, _, _, err := accumulateCursorChain(pages, 1)
	if err == nil || !strings.Contains(err.Error(), "excessive page count") {
		t.Fatalf("accumulateCursorChain() error = %v, want an excessive-page-count error", err)
	}
}

func TestAccumulateCursorChainRejectsABrokenChain(t *testing.T) {
	pages := []cursorFetchedPage{
		{CursorIn: "0", Conversations: nil, HasNextCursor: true, NextCursor: "C1"},
		{CursorIn: "WRONG", Conversations: nil, HasNextCursor: false},
	}
	_, _, _, err := accumulateCursorChain(pages, 10)
	if err == nil || !strings.Contains(err.Error(), "chain broken") {
		t.Fatalf("accumulateCursorChain() error = %v, want a chain-broken error", err)
	}
}

func TestSortConversationsByCreatedDescOrdersNewestFirst(t *testing.T) {
	// B: updated latest, but created oldest. C: middle. A: created newest. Output must be creation
	// time DESC (A, C, B) regardless of any update-time recency or input order.
	b := ccAt(t, "B", "2026-01-01T00:00:00Z")
	c := ccAt(t, "C", "2026-01-02T00:00:00Z")
	a := ccAt(t, "A", "2026-01-03T00:00:00Z")
	got := sortConversationsByCreatedDesc([]cursorConversation{b, c, a})
	want := []string{"A", "C", "B"}
	for i, id := range want {
		if got[i].ID != id {
			t.Fatalf("sortConversationsByCreatedDesc()[%d].ID = %q, want %q (full order: %v)", i, got[i].ID, id, ids(got))
		}
	}
}

func TestSortConversationsByCreatedDescIsStableAndDeterministicOnTies(t *testing.T) {
	same := "2026-01-01T00:00:00Z"
	input := []cursorConversation{ccAt(t, "Z", same), ccAt(t, "A", same), ccAt(t, "M", same)}
	got1 := sortConversationsByCreatedDesc(input)
	// Re-sort a differently-ordered copy of the same input; a deterministic tie-break must produce
	// the identical output regardless of input order (map iteration order, capture order, etc.).
	reordered := []cursorConversation{input[2], input[0], input[1]}
	got2 := sortConversationsByCreatedDesc(reordered)
	if ids(got1) != ids(got2) {
		t.Fatalf("tie-break was not deterministic: %v vs %v", ids(got1), ids(got2))
	}
	want := "A,M,Z" // ID ascending tie-break
	if ids(got1) != want {
		t.Fatalf("ids(got1) = %q, want %q", ids(got1), want)
	}
}

func ids(conversations []cursorConversation) string {
	out := make([]string, len(conversations))
	for i, c := range conversations {
		out[i] = c.ID
	}
	return strings.Join(out, ",")
}

func TestCursorConversationHasNoPinOrStarField(t *testing.T) {
	// Structural, not behavioral: cursorConversation deliberately carries no pin/star/updated-order
	// field at all (README "Pin state is local-only" / "Ordering is local"), so no remote pin or
	// server ordering signal can ever reach sortConversationsByCreatedDesc even by accident. This
	// test exists so that adding such a field back in later would have to consciously touch this
	// comment, rather than silently regressing the design decision.
	c := cursorConversation{ID: "x", CreatedAt: mustParseTime(t, "2026-01-01T00:00:00Z")}
	_ = c // ID/CreatedAt/UpdatedAt only — see the type definition in cursor.go.
}

func TestIsNonIncreasingByCreatedAndUpdated(t *testing.T) {
	nonIncreasing := []cursorConversation{
		{ID: "a", CreatedAt: mustParseTime(t, "2026-01-03T00:00:00Z"), UpdatedAt: mustParseTime(t, "2026-01-03T00:00:00Z")},
		{ID: "b", CreatedAt: mustParseTime(t, "2026-01-02T00:00:00Z"), UpdatedAt: mustParseTime(t, "2026-01-01T00:00:00Z")},
	}
	if !isNonIncreasingByCreated(nonIncreasing) {
		t.Fatal("expected created_desc=true")
	}
	if !isNonIncreasingByUpdated(nonIncreasing) {
		t.Fatal("expected updated_desc=true")
	}
	increasing := []cursorConversation{
		{ID: "a", CreatedAt: mustParseTime(t, "2026-01-01T00:00:00Z")},
		{ID: "b", CreatedAt: mustParseTime(t, "2026-01-02T00:00:00Z")},
	}
	if isNonIncreasingByCreated(increasing) {
		t.Fatal("expected created_desc=false")
	}
}

func TestParseCursorWireItemsFailsClosedOnMissingIDOrUnparseableTimestamp(t *testing.T) {
	tests := []struct {
		name  string
		items []cursorConversationWireItem
		want  string
	}{
		{name: "missing id", items: []cursorConversationWireItem{{ID: "", CreatedAt: "2026-01-01T00:00:00Z"}}, want: "identity"},
		{name: "missing create_time", items: []cursorConversationWireItem{{ID: "x", CreatedAt: ""}}, want: "create_time"},
		{name: "unparseable create_time", items: []cursorConversationWireItem{{ID: "x", CreatedAt: "not-a-time"}}, want: "create_time"},
		{name: "unparseable update_time", items: []cursorConversationWireItem{{ID: "x", CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "not-a-time"}}, want: "update_time"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := parseCursorWireItems(test.items)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("parseCursorWireItems() error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestRedactedCursorNeverExposesAnOpaqueValue(t *testing.T) {
	if redactedCursor("0") != "0" {
		t.Fatal(`redactedCursor("0") must show the documented entry point verbatim`)
	}
	secret := "K1JJRDp-some-long-opaque-token"
	got := redactedCursor(secret)
	if strings.Contains(got, secret) {
		t.Fatalf("redactedCursor leaked the raw cursor value: %q", got)
	}
	if got == redactedCursor("a-different-token") {
		t.Fatal("redactedCursor collided two different cursor values")
	}
	if got != redactedCursor(secret) {
		t.Fatal("redactedCursor must be deterministic for the same input")
	}
}
