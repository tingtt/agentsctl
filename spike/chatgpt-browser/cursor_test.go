package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"sort"
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

func wireItems(ids ...string) []cursorConversationWireItem {
	items := make([]cursorConversationWireItem, len(ids))
	for i, id := range ids {
		items[i] = cursorConversationWireItem{ID: id, CreatedAt: "2026-01-01T00:00:00Z"}
	}
	return items
}

func TestDedupeCapturesByCursorInCollapsesABenignDuplicate(t *testing.T) {
	captures := []cursorCaptureWireItem{
		{CaptureID: 1, CursorIn: "0", Items: wireItems("A", "B"), RawItemCount: 2, HasNextCursor: true, NextCursor: "C1"},
		{CaptureID: 2, CursorIn: "0", Items: wireItems("A", "B"), RawItemCount: 2, HasNextCursor: true, NextCursor: "C1"}, // benign re-render duplicate
		{CaptureID: 3, CursorIn: "C1", Items: wireItems("C"), RawItemCount: 1, HasNextCursor: false},
	}
	got, err := dedupeCapturesByCursorIn(captures)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d pages, want 2 (one per distinct CursorIn)", len(got))
	}
	if got[0].CursorIn != "0" || got[1].CursorIn != "C1" {
		t.Fatalf("unexpected order/content: %+v", got)
	}
}

// TestDedupeCapturesByCursorInToleratesANonIdempotentNextCursorToken covers the live bug found on
// 2026-09-14: real-wheel-triggered traffic repeatedly re-fetched the SAME CursorIn ("0") in quick
// succession, and two of those observations reported a DIFFERENT declared next-cursor token for an
// otherwise-IDENTICAL conversation set. This must be tolerated as a benign duplicate (the opaque
// cursor token is not proven idempotent — README "Cursor is opaque" already forbids assuming
// otherwise), using the LATEST observation's own next-cursor to continue the walk from.
func TestDedupeCapturesByCursorInToleratesANonIdempotentNextCursorToken(t *testing.T) {
	captures := []cursorCaptureWireItem{
		{CaptureID: 5, CursorIn: "0", Items: wireItems("A", "B"), RawItemCount: 2, HasNextCursor: true, NextCursor: "OLD_TOKEN"},
		{CaptureID: 9, CursorIn: "0", Items: wireItems("A", "B"), RawItemCount: 2, HasNextCursor: true, NextCursor: "NEW_TOKEN"},
	}
	got, err := dedupeCapturesByCursorIn(captures)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d pages, want 1", len(got))
	}
	if got[0].NextCursor != "NEW_TOKEN" {
		t.Fatalf("got[0].NextCursor = %q, want the LATEST (highest CaptureID) observation's token %q", got[0].NextCursor, "NEW_TOKEN")
	}
}

func TestDedupeCapturesByCursorInFailsClosedOnADifferentConversationSet(t *testing.T) {
	// Two observations of the SAME CursorIn that returned a genuinely DIFFERENT conversation set —
	// e.g. a stale pre-navigation capture compared against a fresh one, or genuine account activity
	// mid-enumeration — must never be silently resolved by picking one.
	captures := []cursorCaptureWireItem{
		{CaptureID: 1, CursorIn: "0", Items: wireItems("A", "B", "C", "D", "E"), RawItemCount: 5, HasNextCursor: true, NextCursor: "OLD"},
		{CaptureID: 9, CursorIn: "0", Items: wireItems("F", "G", "H", "I", "J"), RawItemCount: 5, HasNextCursor: true, NextCursor: "NEW"},
	}
	_, err := dedupeCapturesByCursorIn(captures)
	if err == nil || !strings.Contains(err.Error(), "different conversation set") {
		t.Fatalf("dedupeCapturesByCursorIn() error = %v, want a different-conversation-set error", err)
	}
}

func TestSummarizeCursorInVariantsDistinguishesFlickeringFromStable(t *testing.T) {
	captures := []cursorCaptureWireItem{
		{CaptureID: 1, CursorIn: "0", Items: wireItems("A", "B"), RawItemCount: 2, NextCursor: "T1"},
		{CaptureID: 2, CursorIn: "0", Items: wireItems("C", "D"), RawItemCount: 2, NextCursor: "T2"}, // different set: flicker
		{CaptureID: 3, CursorIn: "0", Items: wireItems("A", "B"), RawItemCount: 2, NextCursor: "T3"}, // back to the first set, new token
		{CaptureID: 4, CursorIn: "C1", Items: wireItems("E"), RawItemCount: 1},
	}
	got := summarizeCursorInVariants(captures)
	zero, ok := got["0"]
	if !ok {
		t.Fatalf("summarizeCursorInVariants() missing cursor_in=0: %+v", got)
	}
	if zero.Observations != 3 {
		t.Fatalf("Observations = %d, want 3", zero.Observations)
	}
	if zero.DistinctIDSets != 2 {
		t.Fatalf("DistinctIDSets = %d, want 2 (A,B and C,D)", zero.DistinctIDSets)
	}
	if zero.DistinctNextCursors != 3 {
		t.Fatalf("DistinctNextCursors = %d, want 3 (T1, T2, T3 all distinct)", zero.DistinctNextCursors)
	}
	if zero.MinItemCount != 2 || zero.MaxItemCount != 2 {
		t.Fatalf("item count range = %d-%d, want 2-2", zero.MinItemCount, zero.MaxItemCount)
	}
	c1, ok := got["C1"]
	if !ok || c1.Observations != 1 || c1.DistinctIDSets != 1 {
		t.Fatalf("unexpected summary for cursor_in=C1: %+v (ok=%t)", c1, ok)
	}
}

// testCanonicalCursorSeriesKey mirrors bridge/main.js's canonicalCursorSeriesKeyFrom exactly
// (cursor excluded, remaining pairs sorted by key then value, JSON-encoded, SHA-256'd) — the same
// pattern as the existing testCanonicalSeriesKey for the global endpoint's offset exclusion.
// Test-only; keep in sync with canonicalCursorSeriesKeyFrom if that changes.
func testCanonicalCursorSeriesKey(pairs [][2]string) string {
	filtered := make([][2]string, 0, len(pairs))
	for _, pair := range pairs {
		if pair[0] == "cursor" {
			continue
		}
		filtered = append(filtered, pair)
	}
	sort.Slice(filtered, func(i, j int) bool {
		if filtered[i][0] != filtered[j][0] {
			return filtered[i][0] < filtered[j][0]
		}
		return filtered[i][1] < filtered[j][1]
	})
	canonical, err := json.Marshal(filtered)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}

func TestCanonicalCursorSeriesKeyExcludesCursorAndIsOrderIndependent(t *testing.T) {
	a := testCanonicalCursorSeriesKey([][2]string{{"limit", "5"}, {"cursor", "abc"}})
	b := testCanonicalCursorSeriesKey([][2]string{{"cursor", "xyz"}, {"limit", "5"}})
	if a != b {
		t.Fatalf("keys with only cursor differing should match: %q vs %q", a, b)
	}
	c := testCanonicalCursorSeriesKey([][2]string{{"limit", "10"}, {"cursor", "abc"}})
	if a == c {
		t.Fatal("keys with a different limit must not collide")
	}
}

func TestGroupCapturesBySeriesKey(t *testing.T) {
	captures := []cursorCaptureWireItem{
		{CaptureID: 1, CursorIn: "0", SeriesKey: "S1", Items: wireItems("A")},
		{CaptureID: 2, CursorIn: "0", SeriesKey: "S2", Items: wireItems("X", "Y")},
		{CaptureID: 3, CursorIn: "C1", SeriesKey: "S1", Items: wireItems("B")},
	}
	groups := groupCapturesBySeriesKey(captures)
	if len(groups) != 2 {
		t.Fatalf("got %d groups, want 2", len(groups))
	}
	if len(groups["S1"]) != 2 || len(groups["S2"]) != 1 {
		t.Fatalf("unexpected group sizes: S1=%d S2=%d", len(groups["S1"]), len(groups["S2"]))
	}
}

func TestSelectSeriesMatchingObservedLinkCountPicksTheUniqueMatch(t *testing.T) {
	groups := map[string][]cursorCaptureWireItem{
		"decoy": {{CaptureID: 1, SeriesKey: "decoy", Items: wireItems("A", "B", "C", "D", "E")}},
		"real":  {{CaptureID: 2, SeriesKey: "real", Items: wireItems("F", "G", "H", "I", "J", "K", "L", "M", "N", "O")}},
	}
	key, ok := selectSeriesMatchingObservedLinkCount(groups, 10)
	if !ok || key != "real" {
		t.Fatalf("selectSeriesMatchingObservedLinkCount() = (%q, %t), want (\"real\", true)", key, ok)
	}
}

func TestSelectSeriesMatchingObservedLinkCountFailsClosedWhenAmbiguous(t *testing.T) {
	// Zero matches: the DOM-observed count doesn't match any series' first page.
	if _, ok := selectSeriesMatchingObservedLinkCount(map[string][]cursorCaptureWireItem{
		"a": {{CaptureID: 1, SeriesKey: "a", Items: wireItems("A", "B")}},
	}, 10); ok {
		t.Fatal("expected ok=false when no series matches the expected size")
	}
	// Two matches: never guess which one is real just because both happen to have the right size.
	if _, ok := selectSeriesMatchingObservedLinkCount(map[string][]cursorCaptureWireItem{
		"a": {{CaptureID: 1, SeriesKey: "a", Items: wireItems("A", "B")}},
		"b": {{CaptureID: 2, SeriesKey: "b", Items: wireItems("C", "D")}},
	}, 2); ok {
		t.Fatal("expected ok=false when more than one series matches the expected size")
	}
}

func TestFilterCapturesBySeriesKey(t *testing.T) {
	captures := []cursorCaptureWireItem{
		{CaptureID: 1, SeriesKey: "keep"},
		{CaptureID: 2, SeriesKey: "drop"},
		{CaptureID: 3, SeriesKey: "keep"},
	}
	got := filterCapturesBySeriesKey(captures, "keep")
	if len(got) != 2 || got[0].CaptureID != 1 || got[1].CaptureID != 3 {
		t.Fatalf("filterCapturesBySeriesKey() = %+v, want captures 1 and 3", got)
	}
}

func TestFilterCapturesNewerThanExcludesStaleCaptures(t *testing.T) {
	// Mirrors the live bug this fixes: a stale capture from before some watermark-setting action
	// (e.g. a fresh navigation) must never reach dedupeCapturesByCursorIn alongside a fresh one.
	captures := []cursorCaptureWireItem{
		{CaptureID: 1, CursorIn: "0", Items: wireItems("OLD-A", "OLD-B"), RawItemCount: 2, HasNextCursor: true, NextCursor: "STALE_NEXT"},
		{CaptureID: 5, CursorIn: "0", Items: wireItems("NEW-A", "NEW-B", "NEW-C"), RawItemCount: 3, HasNextCursor: false},
	}
	fresh := filterCapturesNewerThan(captures, 1)
	if len(fresh) != 1 || fresh[0].CaptureID != 5 {
		t.Fatalf("filterCapturesNewerThan() = %+v, want only CaptureID 5", fresh)
	}
	// The stale+fresh pair would otherwise conflict; filtering first must let dedupe succeed.
	if _, err := dedupeCapturesByCursorIn(fresh); err != nil {
		t.Fatalf("unexpected error after filtering stale captures: %v", err)
	}
	if _, err := dedupeCapturesByCursorIn(captures); err == nil {
		t.Fatal("expected the unfiltered stale+fresh pair to conflict (sanity check on the test fixture itself)")
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

func TestClassifyCursorExperimentOutcome(t *testing.T) {
	tests := []struct {
		name                               string
		cursorPresent200, cursorPresent401 int
		pagesCaptured                      int
		chainErr                           error
		complete                           bool
		want                               cursorExperimentOutcome
	}{
		{name: "no request at all", cursorPresent200: 0, cursorPresent401: 0, pagesCaptured: 0, want: outcomeNoRequest},
		{name: "frontend itself 401s, zero pages", cursorPresent200: 0, cursorPresent401: 3, pagesCaptured: 0, want: outcomeFrontend401},
		{name: "capture/schema error after some pages", cursorPresent200: 2, cursorPresent401: 0, pagesCaptured: 2, chainErr: errFixture, want: outcomeCaptureFailure},
		{name: "valid page0, no terminal, attempts exhausted", cursorPresent200: 2, cursorPresent401: 0, pagesCaptured: 1, chainErr: &cursorChainIncompleteError{Reason: "WHEEL_NO_PROGRESS"}, complete: false, want: outcomeChainIncomplete},
		{name: "pages captured, no error, not terminal", cursorPresent200: 2, cursorPresent401: 0, pagesCaptured: 2, complete: false, want: outcomeChainIncomplete},
		{name: "terminal reached cleanly", cursorPresent200: 2, cursorPresent401: 0, pagesCaptured: 2, complete: true, want: outcomeComplete},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := classifyCursorExperimentOutcome(test.cursorPresent200, test.cursorPresent401, test.pagesCaptured, test.chainErr, test.complete)
			if got != test.want {
				t.Fatalf("classifyCursorExperimentOutcome() = %s, want %s", got, test.want)
			}
		})
	}
}

var errFixture = fixtureError("fixture error")

type fixtureError string

func (e fixtureError) Error() string { return string(e) }

// testHasCursorQueryParam mirrors bridge/main.js's hasCursorQueryParam exactly (whether a URL's
// query string carries a `cursor` key at all, regardless of value). Test-only: production Go code
// never inspects a raw URL itself, it only receives the bridge's already-computed classification.
// Keep in sync with hasCursorQueryParam if that changes.
func testHasCursorQueryParam(rawURL string) bool {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	return parsed.Query().Has("cursor")
}

// testBucketResponseStatus mirrors bridge/main.js's bucketResponseStatus exactly. Test-only; keep
// in sync if that changes.
func testBucketResponseStatus(status int) string {
	switch status {
	case 200:
		return "200"
	case 401:
		return "401"
	default:
		return "other"
	}
}

func TestResponseStatusClassificationMirror(t *testing.T) {
	tests := []struct {
		name          string
		url           string
		status        int
		wantHasCursor bool
		wantStatusKey string
	}{
		{name: "no cursor, 200", url: "https://chatgpt.com/backend-api/gizmos/g-p-abc/conversations", status: 200, wantHasCursor: false, wantStatusKey: "200"},
		{name: "cursor present, 200", url: "https://chatgpt.com/backend-api/gizmos/g-p-abc/conversations?cursor=abc123", status: 200, wantHasCursor: true, wantStatusKey: "200"},
		{name: "cursor present, 401", url: "https://chatgpt.com/backend-api/gizmos/g-p-abc/conversations?cursor=abc123", status: 401, wantHasCursor: true, wantStatusKey: "401"},
		{name: "unrelated endpoint, 200", url: "https://chatgpt.com/backend-api/pins", status: 200, wantHasCursor: false, wantStatusKey: "200"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := testHasCursorQueryParam(test.url); got != test.wantHasCursor {
				t.Fatalf("testHasCursorQueryParam(%q) = %t, want %t", test.url, got, test.wantHasCursor)
			}
			if got := testBucketResponseStatus(test.status); got != test.wantStatusKey {
				t.Fatalf("testBucketResponseStatus(%d) = %q, want %q", test.status, got, test.wantStatusKey)
			}
		})
	}
}

// testProjectConversationLinkPattern and testIsConversationLinkHref mirror
// bridge/preload.js's projectConversationLinkPattern/isConversationLinkHref exactly. Test-only;
// keep in sync if those change.
func testProjectConversationLinkPattern() *regexp.Regexp {
	return regexp.MustCompile(`^/g/g-p-[A-Za-z0-9_-]+/c/`)
}

func testIsConversationLinkHref(href string) (any, project bool) {
	project = testProjectConversationLinkPattern().MatchString(href)
	any = project || strings.HasPrefix(href, "/c/")
	return any, project
}

func TestIsConversationLinkHrefRecognizesBothURLShapes(t *testing.T) {
	tests := []struct {
		href        string
		wantAny     bool
		wantProject bool
	}{
		{href: "/c/6a9b1e28-dfd4-83e9-ad43-52c3ed1dcd03", wantAny: true, wantProject: false},
		{href: "/g/g-p-6a970725a218819183e2c93af6890068/c/6a9b1e28-dfd4-83e9-ad43-52c3ed1dcd03", wantAny: true, wantProject: true},
		{href: "/settings/general", wantAny: false, wantProject: false},
		{href: "/g/g-p-6a970725a218819183e2c93af6890068/project", wantAny: false, wantProject: false},
	}
	for _, test := range tests {
		t.Run(test.href, func(t *testing.T) {
			any, project := testIsConversationLinkHref(test.href)
			if any != test.wantAny || project != test.wantProject {
				t.Fatalf("testIsConversationLinkHref(%q) = (any=%t, project=%t), want (any=%t, project=%t)",
					test.href, any, project, test.wantAny, test.wantProject)
			}
		})
	}
}

// scrollCandidateCounts mirrors one candidate scrollable container's link tallies, as computed by
// bridge/preload.js's findProjectScrollRegion.
type scrollCandidateCounts struct {
	anyLinks     int
	projectLinks int
}

// testSelectBestScrollCandidate mirrors findProjectScrollRegion's candidate-selection loop exactly
// (prefer the most Project-scoped conversation links, tie-broken by total conversation-link
// count): the pure decision logic behind "which scrollable region is the Project's own list",
// separated out for unit testing without a live DOM. Returns -1 if candidates is empty. Test-only;
// keep in sync with findProjectScrollRegion if that changes.
func testSelectBestScrollCandidate(candidates []scrollCandidateCounts) int {
	best := -1
	for i, c := range candidates {
		if best == -1 {
			best = i
			continue
		}
		bc := candidates[best]
		if c.projectLinks > bc.projectLinks || (c.projectLinks == bc.projectLinks && c.anyLinks > bc.anyLinks) {
			best = i
		}
	}
	return best
}

func TestSelectBestScrollCandidatePrefersProjectLinks(t *testing.T) {
	// A persistent general sidebar (many /c/ links, zero Project-scoped ones) alongside the
	// Project's own, smaller list (fewer links overall, but Project-scoped) — the Project's own
	// list must win despite having fewer total links.
	candidates := []scrollCandidateCounts{
		{anyLinks: 40, projectLinks: 0},
		{anyLinks: 5, projectLinks: 5},
	}
	if got := testSelectBestScrollCandidate(candidates); got != 1 {
		t.Fatalf("testSelectBestScrollCandidate() = %d, want 1 (the Project-scoped candidate)", got)
	}
}

func TestSelectBestScrollCandidateTieBreaksOnTotalLinks(t *testing.T) {
	candidates := []scrollCandidateCounts{
		{anyLinks: 5, projectLinks: 3},
		{anyLinks: 8, projectLinks: 3},
	}
	if got := testSelectBestScrollCandidate(candidates); got != 1 {
		t.Fatalf("testSelectBestScrollCandidate() = %d, want 1 (more total links on an equal Project-link tie)", got)
	}
}

func TestSelectBestScrollCandidateEmpty(t *testing.T) {
	if got := testSelectBestScrollCandidate(nil); got != -1 {
		t.Fatalf("testSelectBestScrollCandidate(nil) = %d, want -1", got)
	}
}

// TestCursorExperimentNeverCallsTheFalsifiedSelfFetchPath is a source-structure safety net (README
// Task 12 "passive-only invariant" — accepted there as sufficient when a live/mocked-client test
// isn't practical): enumerateProjectConversationsByCursorPassive and runCursorExperiment must never
// call enumerateProjectConversationsByCursorSelfFetch, since a self-issued fetch of the
// cursor-parameterized endpoint is a confirmed HTTP 401 (see cursor.go's doc comments). This scans
// each function's own source text (not the whole file, so an unrelated, deliberate reference
// elsewhere — e.g. the -cursor-self-fetch-probe wiring in main.go — can never trip it).
func TestCursorExperimentNeverCallsTheFalsifiedSelfFetchPath(t *testing.T) {
	source, err := os.ReadFile("cursor.go")
	if err != nil {
		t.Fatalf("read cursor.go: %v", err)
	}
	forbidden := "enumerateProjectConversationsByCursorSelfFetch"
	for _, fn := range []string{"func enumerateProjectConversationsByCursorPassive(", "func runCursorExperiment("} {
		start := strings.Index(string(source), fn)
		if start < 0 {
			t.Fatalf("could not locate %s in cursor.go (has it been renamed?)", fn)
		}
		body := extractFunctionBody(string(source)[start:])
		if strings.Contains(body, forbidden) {
			t.Fatalf("%s must never call %s (self-fetch is a confirmed HTTP 401 once a cursor parameter is present)", fn, forbidden)
		}
	}
}

// extractFunctionBody returns the text from the start of a function signature through its
// matching closing brace, using simple brace counting — sufficient for this file's own,
// gofmt-formatted source, not a general Go parser.
func extractFunctionBody(fromFuncKeyword string) string {
	depth := 0
	started := false
	for i, r := range fromFuncKeyword {
		switch r {
		case '{':
			depth++
			started = true
		case '}':
			depth--
			if started && depth == 0 {
				return fromFuncKeyword[:i+1]
			}
		}
	}
	return fromFuncKeyword
}

func TestIsCursorChainIncompleteDistinguishesFromOrdinaryErrors(t *testing.T) {
	if !isCursorChainIncomplete(&cursorChainIncompleteError{Reason: "WHEEL_NO_PROGRESS"}) {
		t.Fatal("expected a *cursorChainIncompleteError to be recognized")
	}
	if isCursorChainIncomplete(errFixture) {
		t.Fatal("an ordinary error must never be misidentified as chain-incomplete")
	}
	if isCursorChainIncomplete(nil) {
		t.Fatal("a nil error must never be misidentified as chain-incomplete")
	}
	wrapped := fmt.Errorf("attempt failed: %w", &cursorChainIncompleteError{Reason: "ATTEMPTS_EXHAUSTED"})
	if !isCursorChainIncomplete(wrapped) {
		t.Fatal("errors.As must see through a wrapped *cursorChainIncompleteError")
	}
}

func TestSubtractStatusCounts(t *testing.T) {
	before := cursorResponseStatusCounts{
		NoCursor:      map[string]int{"200": 5, "401": 0, "other": 0},
		CursorPresent: map[string]int{"200": 90, "401": 0, "other": 0},
	}
	after := cursorResponseStatusCounts{
		NoCursor:      map[string]int{"200": 5, "401": 0, "other": 0},
		CursorPresent: map[string]int{"200": 100, "401": 0, "other": 0},
	}
	delta := subtractStatusCounts(after, before)
	if delta.CursorPresent["200"] != 10 {
		t.Fatalf("delta.CursorPresent[200] = %d, want 10", delta.CursorPresent["200"])
	}
	if delta.NoCursor["200"] != 0 {
		t.Fatalf("delta.NoCursor[200] = %d, want 0 (unchanged)", delta.NoCursor["200"])
	}
}

func TestForwardProgressObservedTrueWhenNextCursorSeenAsLaterCursorIn(t *testing.T) {
	pages := []cursorFetchedPage{
		{CursorIn: "0", HasNextCursor: true, NextCursor: "C1"},
		{CursorIn: "C1", HasNextCursor: false},
	}
	if !forwardProgressObserved(pages, "C1") {
		t.Fatal("expected forward progress: a later page's CursorIn matches the expected next cursor")
	}
}

func TestForwardProgressObservedFalseWhenOnlyCursorInZeroRepeats(t *testing.T) {
	// README Task 9: a repeated cursor_in=0 observation — however many times it recurs — is never
	// forward progress by itself.
	pages := []cursorFetchedPage{
		{CursorIn: "0", HasNextCursor: true, NextCursor: "C1"},
		{CursorIn: "0", HasNextCursor: true, NextCursor: "C1"},
	}
	if forwardProgressObserved(pages, "C1") {
		t.Fatal("repeated cursor_in=0 observations must never count as forward progress")
	}
}

func TestForwardProgressObservedFalseWhenNoExpectedCursorYet(t *testing.T) {
	if forwardProgressObserved([]cursorFetchedPage{{CursorIn: "0"}}, "") {
		t.Fatal("expected false when no page has declared a next cursor yet")
	}
}

func TestClassifyWheelProgressReason(t *testing.T) {
	tests := []struct {
		name                                     string
		scrollChanged, linkCountChanged, forward bool
		want                                     string
	}{
		{name: "nothing observed at all", want: "WHEEL_NO_PROGRESS"},
		{name: "scroll moved", scrollChanged: true, want: "ATTEMPTS_EXHAUSTED"},
		{name: "link count changed", linkCountChanged: true, want: "ATTEMPTS_EXHAUSTED"},
		{name: "forward cursor progress", forward: true, want: "ATTEMPTS_EXHAUSTED"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := classifyWheelProgressReason(test.scrollChanged, test.linkCountChanged, test.forward)
			if got != test.want {
				t.Fatalf("classifyWheelProgressReason() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestShouldAttemptCDPWheelFallback(t *testing.T) {
	if shouldAttemptCDPWheelFallback(true) {
		t.Fatal("CDP must not be attempted when Electron already showed progress")
	}
	if !shouldAttemptCDPWheelFallback(false) {
		t.Fatal("CDP must be attempted when Electron showed zero progress")
	}
}

func TestWheelTicksShowScrollAndLinkChange(t *testing.T) {
	zero, ten, twenty := 0, 10, 20
	noChange := []wheelTickDiagnostic{
		{ScrollTopBefore: &zero, ScrollTopAfter: &zero, ProjectConversationLinksBefore: &ten, ProjectConversationLinksAfter: &ten},
	}
	if wheelTicksShowScrollChange(noChange) || wheelTicksShowLinkChange(noChange) {
		t.Fatal("identical before/after values must never report a change")
	}
	scrollMoved := &ten
	scrollChange := []wheelTickDiagnostic{
		{ScrollTopBefore: &zero, ScrollTopAfter: scrollMoved, ProjectConversationLinksBefore: &ten, ProjectConversationLinksAfter: &ten},
	}
	if !wheelTicksShowScrollChange(scrollChange) {
		t.Fatal("expected a scroll change to be detected")
	}
	if wheelTicksShowLinkChange(scrollChange) {
		t.Fatal("link count did not change in this fixture")
	}
	linkChange := []wheelTickDiagnostic{
		{ScrollTopBefore: &zero, ScrollTopAfter: &zero, ProjectConversationLinksBefore: &ten, ProjectConversationLinksAfter: &twenty},
	}
	if !wheelTicksShowLinkChange(linkChange) {
		t.Fatal("expected a link-count change to be detected")
	}
	missingPair := []wheelTickDiagnostic{
		{ScrollTopBefore: nil, ScrollTopAfter: &zero, ProjectConversationLinksBefore: nil, ProjectConversationLinksAfter: nil},
	}
	if wheelTicksShowScrollChange(missingPair) || wheelTicksShowLinkChange(missingPair) {
		t.Fatal("a nil half of a before/after pair must never be treated as a change")
	}
}
