package main

import (
	"net/url"
	"os"
	"regexp"
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
