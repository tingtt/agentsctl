package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"strings"
	"testing"
)

func TestCallFailsClosed(t *testing.T) {
	tests := []struct {
		name     string
		response string
		want     string
	}{
		{name: "remote rejection", response: `{"id":1,"ok":false,"error":"schema changed"}` + "\n", want: "schema changed"},
		{name: "missing result", response: `{"id":1,"ok":true}` + "\n", want: "no result"},
		{name: "wrong identity", response: `{"id":2,"ok":true,"result":{}}` + "\n", want: "ID mismatch"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client, server := net.Pipe()
			defer client.Close()
			go func() {
				defer server.Close()
				_ = json.NewDecoder(server).Decode(&request{})
				_, _ = server.Write([]byte(test.response))
			}()
			_, err := call(client, request{ID: 1, Method: "ping"})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("call() error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestSameIDsIgnoresOrderButRejectsChanges(t *testing.T) {
	left := []conversation{{ID: "a"}, {ID: "b"}}
	if !sameIDs(left, []conversation{{ID: "b"}, {ID: "a"}}) {
		t.Fatal("sameIDs() rejected the same identities in a different order")
	}
	if sameIDs(left, []conversation{{ID: "a"}, {ID: "c"}}) {
		t.Fatal("sameIDs() accepted a changed identity")
	}
}

func conv(ids ...string) []conversation {
	items := make([]conversation, len(ids))
	for i, id := range ids {
		items[i] = conversation{ID: id, ProjectID: "g-p-test"}
	}
	return items
}

// page builds a conversationPage fixture with a recognized collection (the common case for a
// valid page; TestMergeProjectPagesUnrecognizedCollectionFailsClosed builds an unrecognized one
// directly). rawItemCount/recognizedIDCount are independent of len(ids) on purpose: `ids` is the
// already Project-filtered subset the bridge returns, while rawItemCount/recognizedIDCount describe
// the full raw page (this Project's items plus everyone else's), which is exactly the distinction
// Requirement 5 cares about.
func page(seriesKey string, offset, limit, rawItemCount, recognizedIDCount int, digest string, ids ...string) conversationPage {
	return conversationPage{
		SeriesKey:            seriesKey,
		Offset:               offset,
		Limit:                limit,
		RecognizedCollection: true,
		RawItemCount:         rawItemCount,
		RecognizedIDCount:    recognizedIDCount,
		RawIdentityDigest:    digest,
		Items:                conv(ids...),
	}
}

func TestMergeProjectPagesSinglePage(t *testing.T) {
	pages := []conversationPage{page("s", 0, 28, 16, 16, "d0", "a", "b")}
	got, exhausted, err := mergeProjectPages(pages, 20)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !exhausted {
		t.Fatal("a page shorter than its limit must be treated as the final page")
	}
	if len(got) != 2 {
		t.Fatalf("got %d conversations, want 2", len(got))
	}
}

func TestMergeProjectPagesTwoContiguousPages(t *testing.T) {
	pages := []conversationPage{
		page("s", 0, 2, 2, 2, "d0", "a", "b"),
		page("s", 2, 2, 1, 1, "d1", "c"),
	}
	got, exhausted, err := mergeProjectPages(pages, 20)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !exhausted {
		t.Fatal("a contiguous chain ending in a short page must be treated as exhausted")
	}
	if len(got) != 3 {
		t.Fatalf("got %d conversations, want 3", len(got))
	}
}

func TestMergeProjectPagesEmptyFinalPage(t *testing.T) {
	pages := []conversationPage{
		page("s", 0, 2, 2, 2, "d0", "a", "b"),
		page("s", 2, 2, 0, 0, "d1"),
	}
	got, exhausted, err := mergeProjectPages(pages, 20)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !exhausted {
		t.Fatal("a zero-item final page must be treated as exhaustion")
	}
	if len(got) != 2 {
		t.Fatalf("got %d conversations, want 2", len(got))
	}
}

func TestMergeProjectPagesNotExhaustedWhenFirstPageIsFull(t *testing.T) {
	// This is the live-observed case: a page exactly as long as its limit does not, by itself,
	// prove no further page exists. Real ChatGPT accounts observed this shape at offset 0 with
	// limit 28 while a genuine next page was never reachable, so this must stay incomplete.
	pages := []conversationPage{page("s", 0, 28, 28, 28, "d0", "a", "b")}
	_, exhausted, err := mergeProjectPages(pages, 20)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if exhausted {
		t.Fatal("a full page must not be treated as proof of exhaustion")
	}
}

func TestMergeProjectPagesGapIsNeverExhausted(t *testing.T) {
	// offset 28 (the next expected step after a full 0..27 page) was never observed; offset 56
	// existing does not fill that gap and must never be treated as continuing the chain.
	pages := []conversationPage{
		page("s", 0, 28, 28, 28, "d0", "a"),
		page("s", 56, 28, 5, 5, "d1", "z"),
	}
	_, exhausted, err := mergeProjectPages(pages, 20)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if exhausted {
		t.Fatal("a gap in the offset chain must never be reported as exhausted, regardless of what a later offset looks like")
	}
}

func TestMergeProjectPagesChainIsOrderIndependent(t *testing.T) {
	// Captures can arrive in any order (scroll simulation harvests whatever the UI produced); the
	// chain must be built from SeriesKey+Offset, not arrival order.
	pages := []conversationPage{
		page("s", 28, 28, 5, 5, "d1", "c"), // arrives first in the slice, but is the *second* page
		page("s", 0, 28, 28, 28, "d0", "a", "b"),
	}
	got, exhausted, err := mergeProjectPages(pages, 20)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !exhausted {
		t.Fatal("expected the chain (offset 0 full, offset 28 short) to be recognized as exhausted regardless of arrival order")
	}
	if len(got) != 3 {
		t.Fatalf("got %d conversations, want 3", len(got))
	}
}

func TestMergeProjectPagesRepeatedIdenticalOffsetIsIdempotent(t *testing.T) {
	pages := []conversationPage{
		page("s", 0, 28, 16, 16, "d0", "a", "b"),
		page("s", 0, 28, 16, 16, "d0", "a", "b"),
	}
	got, exhausted, err := mergeProjectPages(pages, 20)
	if err != nil {
		t.Fatalf("unexpected error re-observing the same offset with identical raw identity: %v", err)
	}
	if !exhausted || len(got) != 2 {
		t.Fatalf("got exhausted=%t len=%d, want exhausted=true len=2", exhausted, len(got))
	}
}

func TestMergeProjectPagesRepeatedOffsetWithDifferentRawIdentityFailsClosed(t *testing.T) {
	// Same SeriesKey+Offset, same Project-filtered items, but a different RawIdentityDigest — the
	// underlying raw page changed even though the filtered subset happens to look the same. This is
	// exactly the Requirement 5 case: Project-filtered equality must not paper over raw inconsistency.
	pages := []conversationPage{
		page("s", 0, 28, 16, 16, "d0", "a", "b"),
		page("s", 0, 28, 16, 16, "d1", "a", "b"),
	}
	if _, _, err := mergeProjectPages(pages, 20); err == nil {
		t.Fatal("expected an error when the same series+offset reports a different raw page identity")
	}
}

func TestMergeProjectPagesDoesNotMergeDifferentQuerySeries(t *testing.T) {
	pages := []conversationPage{
		page("is_archived=false&limit=28", 0, 28, 28, 28, "d0", "a"),
		page("is_archived=true&limit=28", 28, 28, 5, 5, "d1", "b"),
	}
	_, exhausted, err := mergeProjectPages(pages, 20)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if exhausted {
		t.Fatal("two different filter combinations must never be combined into one offset chain")
	}
}

func TestMergeProjectPagesLimitChangeIsADifferentSeries(t *testing.T) {
	// A limit change is encoded as a different SeriesKey (limit is part of the normalized query),
	// so it is naturally never treated as a continuation of the same series.
	pages := []conversationPage{
		page("limit=28&order=updated", 0, 28, 28, 28, "d0", "a"),
		page("limit=50&order=updated", 28, 50, 10, 10, "d1", "b"),
	}
	_, exhausted, err := mergeProjectPages(pages, 20)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if exhausted {
		t.Fatal("a mid-series limit change must never produce a false COMPLETE")
	}
}

func TestMergeProjectPagesRejectsHideSnorlaxPage(t *testing.T) {
	pages := []conversationPage{
		{SeriesKey: "s", HideSnorlax: true, Offset: 0, Limit: 28, RawItemCount: 0, RecognizedIDCount: 0, RawIdentityDigest: "d0"},
	}
	if _, _, err := mergeProjectPages(pages, 20); err == nil {
		t.Fatal("expected an error when a Project-excluding page reaches the merge step")
	}
}

func TestMergeProjectPagesMalformedLimitFailsClosed(t *testing.T) {
	pages := []conversationPage{page("s", 0, 0, 2, 2, "d0", "a", "b")}
	if _, _, err := mergeProjectPages(pages, 20); err == nil {
		t.Fatal("expected an error for a page with a non-positive limit")
	}
}

func TestMergeProjectPagesMissingSeriesKeyFailsClosed(t *testing.T) {
	// An empty SeriesKey means the bridge couldn't compute a pagination identity for this capture
	// (e.g. an unparseable URL) — it must never be silently treated as belonging to some series.
	pages := []conversationPage{page("", 0, 28, 1, 1, "d0", "a")}
	if _, _, err := mergeProjectPages(pages, 20); err == nil {
		t.Fatal("expected an error for a page with no pagination series identity")
	}
}

func TestMergeProjectPagesNegativeOffsetFailsClosed(t *testing.T) {
	pages := []conversationPage{page("s", -1, 28, 1, 1, "d0", "a")}
	if _, _, err := mergeProjectPages(pages, 20); err == nil {
		t.Fatal("expected an error for a malformed/unrecognized (negative) offset")
	}
}

func TestMergeProjectPagesMissingIdentityFailsClosed(t *testing.T) {
	pages := []conversationPage{
		{SeriesKey: "s", Offset: 0, Limit: 28, RecognizedCollection: true, RawItemCount: 1, RecognizedIDCount: 1, RawIdentityDigest: "d0", Items: []conversation{{ID: ""}}},
	}
	if _, _, err := mergeProjectPages(pages, 20); err == nil {
		t.Fatal("expected an error for a conversation missing a stable identity")
	}
}

func TestMergeProjectPagesUnrecognizedCollectionFailsClosed(t *testing.T) {
	// The item-collection shape itself couldn't be recognized (e.g. the top-level `items`/
	// `conversations` field was renamed). This must never be treated as an empty page, even though
	// it looks identical to one on every other field (rawItemCount 0, well short of limit).
	pages := []conversationPage{
		{SeriesKey: "s", Offset: 0, Limit: 28, RecognizedCollection: false, RawItemCount: 0, RecognizedIDCount: 0, RawIdentityDigest: "d0"},
	}
	if _, _, err := mergeProjectPages(pages, 20); err == nil {
		t.Fatal("expected an error when the item-collection shape was not recognized")
	}
}

func TestMergeProjectPagesRecognizedEmptyCollectionIsAValidShortPage(t *testing.T) {
	// The mirror-image case: a *recognized* collection that legitimately contains zero items (e.g.
	// `"items": []`) is a perfectly normal final page, not schema drift.
	pages := []conversationPage{page("s", 0, 28, 0, 0, "d0")}
	got, exhausted, err := mergeProjectPages(pages, 20)
	if err != nil {
		t.Fatalf("unexpected error for a recognized, legitimately empty collection: %v", err)
	}
	if !exhausted {
		t.Fatal("a recognized empty collection shorter than its limit must be treated as exhaustion")
	}
	if len(got) != 0 {
		t.Fatalf("got %d conversations, want 0", len(got))
	}
}

func TestMergeProjectPagesUnrecognizedRawItemsFailClosed(t *testing.T) {
	// rawItemCount=5 but only 3 could be recognized as conversation-like objects: the schema for
	// the other 2 has drifted (e.g. the ID field was renamed) and must not be silently treated as
	// "0 Project items on this page".
	pages := []conversationPage{page("s", 0, 28, 5, 3, "d0", "a", "b", "c")}
	if _, _, err := mergeProjectPages(pages, 20); err == nil {
		t.Fatal("expected an error when not every raw item's identity could be recognized")
	}
}

func TestMergeProjectPagesKnownIDMismatchFailsClosed(t *testing.T) {
	// A conversation ID already known (from the project-scoped endpoint) appeared in this raw page
	// but its association no longer resolves to the configured Project — concrete evidence the
	// association field itself has drifted, not just an ordinary non-Project chat.
	pages := []conversationPage{
		{SeriesKey: "s", Offset: 0, Limit: 28, RecognizedCollection: true, RawItemCount: 5, RecognizedIDCount: 5, RawIdentityDigest: "d0", KnownIDsMismatched: 1, Items: conv("a")},
	}
	if _, _, err := mergeProjectPages(pages, 20); err == nil {
		t.Fatal("expected an error when a known Project conversation ID no longer resolves to the configured Project")
	}
}

func TestMergeProjectPagesUnrecognizedAssociationFailsClosed(t *testing.T) {
	// Live evidence (see bridge/preload.js's sanitizeGlobalConversationsCaptureItems comment) showed
	// every raw item, Project-associated or not, carries gizmo_id or project_id as an own property.
	// A raw item exposing neither is schema drift, detectable even on a page containing only
	// conversations the harness didn't already know about (unlike the known-ID cross-check above).
	pages := []conversationPage{
		{SeriesKey: "s", Offset: 0, Limit: 28, RecognizedCollection: true, RawItemCount: 5, RecognizedIDCount: 5, RawIdentityDigest: "d0", UnrecognizedAssociationCount: 1, Items: conv("a")},
	}
	if _, _, err := mergeProjectPages(pages, 20); err == nil {
		t.Fatal("expected an error when a raw item exposed no recognizable association field at all")
	}
}

func TestMergeProjectPagesZeroUnrecognizedAssociationIsFine(t *testing.T) {
	// The mirror-image case: every raw item had a recognizable (even if null, or another Project's)
	// association field, so UnrecognizedAssociationCount is 0 and the page is accepted normally.
	pages := []conversationPage{
		{SeriesKey: "s", Offset: 0, Limit: 28, RecognizedCollection: true, RawItemCount: 5, RecognizedIDCount: 5, RawIdentityDigest: "d0", UnrecognizedAssociationCount: 0, Items: conv("a")},
	}
	_, exhausted, err := mergeProjectPages(pages, 20)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !exhausted {
		t.Fatal("a short page with no association drift should be treated as exhaustion")
	}
}

func TestMergeProjectPagesMaxPageGuard(t *testing.T) {
	pages := make([]conversationPage, 21)
	for i := range pages {
		pages[i] = page("s", i*28, 28, 28, 28, fmt.Sprintf("d%d", i), fmt.Sprintf("id-%d", i))
	}
	if _, _, err := mergeProjectPages(pages, 20); err == nil {
		t.Fatal("expected an error once the page count exceeds the defensive bound")
	}
}

func TestMergeProjectPagesDuplicateIDsAcrossPagesDedupe(t *testing.T) {
	pages := []conversationPage{
		page("s", 0, 2, 2, 2, "d0", "a", "b"),
		page("s", 2, 2, 1, 1, "d1", "b"),
	}
	got, _, err := mergeProjectPages(pages, 20)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d conversations, want 2 after deduping the repeated ID", len(got))
	}
}

func TestMergeProjectPagesFiltersToConfiguredProjectAcrossPages(t *testing.T) {
	// globalConversationsFrom (JS side) already restricts items to the requested Project before
	// they cross the bridge; this test documents that mergeProjectPages trusts and preserves that
	// filtering rather than re-deriving it, so a conversation from a different Project passed in
	// by mistake is not silently dropped or merged away — it is just data mergeProjectPages moves.
	other := conversation{ID: "z", ProjectID: "g-p-other"}
	p := page("s", 0, 28, 3, 3, "d0", "a", "b")
	p.Items = append(p.Items, other)
	pages := []conversationPage{p}
	got, _, err := mergeProjectPages(pages, 20)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d conversations, want 3 (mergeProjectPages does not re-filter by Project)", len(got))
	}
}

// testCanonicalSeriesKey mirrors bridge/main.js's canonicalSeriesKeyFrom exactly (offset excluded,
// remaining pairs sorted by key then value, JSON-encoded, SHA-256'd) so the algorithm's properties
// can be verified deterministically without a live browser. It is test-only — production Go code
// never computes a SeriesKey itself, it only receives the digest the bridge already computed; keep
// this in sync with canonicalSeriesKeyFrom if that changes.
func testCanonicalSeriesKey(pairs [][2]string) string {
	filtered := make([][2]string, 0, len(pairs))
	for _, pair := range pairs {
		if pair[0] == "offset" {
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

func TestCanonicalSeriesKeyIgnoresParameterOrder(t *testing.T) {
	a := testCanonicalSeriesKey([][2]string{{"limit", "28"}, {"order", "updated"}, {"offset", "0"}})
	b := testCanonicalSeriesKey([][2]string{{"offset", "0"}, {"order", "updated"}, {"limit", "28"}})
	if a != b {
		t.Fatalf("expected the same key regardless of parameter order, got %q and %q", a, b)
	}
}

func TestCanonicalSeriesKeyIgnoresOffsetValue(t *testing.T) {
	a := testCanonicalSeriesKey([][2]string{{"limit", "28"}, {"offset", "0"}})
	b := testCanonicalSeriesKey([][2]string{{"limit", "28"}, {"offset", "28"}})
	if a != b {
		t.Fatalf("expected offset's own value to be irrelevant to the series key, got %q and %q", a, b)
	}
}

func TestCanonicalSeriesKeyDiffersOnFilterChange(t *testing.T) {
	a := testCanonicalSeriesKey([][2]string{{"is_archived", "false"}, {"limit", "28"}})
	b := testCanonicalSeriesKey([][2]string{{"is_archived", "true"}, {"limit", "28"}})
	if a == b {
		t.Fatal("expected a different key when a filter parameter (is_archived) differs")
	}
}

func TestCanonicalSeriesKeyDistinguishesSameLengthOpaqueValues(t *testing.T) {
	// This is the exact collision the redacted diagnostics representation is vulnerable to: two
	// different opaque values of the same length both display as "<redacted:36ch>", but the
	// canonical series key must still tell them apart.
	same36CharsA := strings.Repeat("a", 36)
	same36CharsB := strings.Repeat("b", 36)
	if describeQueryValueForTest(same36CharsA) != describeQueryValueForTest(same36CharsB) {
		t.Fatal("test setup invalid: expected these two values to redact identically")
	}
	a := testCanonicalSeriesKey([][2]string{{"cursor", same36CharsA}, {"limit", "28"}})
	b := testCanonicalSeriesKey([][2]string{{"cursor", same36CharsB}, {"limit", "28"}})
	if a == b {
		t.Fatal("expected different series keys for different opaque values, even though their redacted diagnostic representation collides")
	}
}

// describeQueryValueForTest mirrors bridge/main.js's describeQueryValue redaction rule closely
// enough to demonstrate the collision TestCanonicalSeriesKeyDistinguishesSameLengthOpaqueValues
// guards against: any value that isn't a plain integer/boolean/short token redacts to a
// length-only placeholder.
func describeQueryValueForTest(value string) string {
	if len(value) <= 20 {
		return value
	}
	return fmt.Sprintf("<redacted:%dch>", len(value))
}

// pageWithDescriptor builds on page() with the coverage-layer descriptor fields
// (IsArchived/IsStarred/Order/HasUnknownParameters) that mergeProjectPages itself ignores but
// evaluateTargetPagination/evaluateActiveListCompleteness use to decide which series is relevant.
// Order defaults to "updated" (the only value ever observed live) since none of the existing
// call sites are specifically testing order; tests that need a different Order build a
// conversationPage literal directly.
func pageWithDescriptor(seriesKey string, offset, limit, rawItemCount, recognizedIDCount int, digest string, isArchived, isStarred *bool, hasUnknownParameters bool, ids ...string) conversationPage {
	p := page(seriesKey, offset, limit, rawItemCount, recognizedIDCount, digest, ids...)
	p.IsArchived = recognizedBoolPtrString(isArchived)
	p.IsStarred = recognizedBoolPtrString(isStarred)
	p.Order = "updated"
	p.HasUnknownParameters = hasUnknownParameters
	return p
}

// recognizedBoolPtrString converts the *bool convenience form (nil = "absent") used throughout
// this test file into the recognized-value string conversationPage.IsArchived/IsStarred actually
// use.
func recognizedBoolPtrString(b *bool) string {
	if b == nil {
		return "absent"
	}
	return recognizedBoolString(*b)
}

func TestEvaluateTargetPaginationRestrictedSeriesAloneIsIncomplete(t *testing.T) {
	// Only an is_starred=true series was observed; the requirement is the canonical
	// is_starred=false active series. A restricted subset must never be mistaken for the default.
	pages := []conversationPage{
		pageWithDescriptor("starred-only", 0, 28, 3, 3, "d0", boolPtr(false), boolPtr(true), false, "a", "b", "c"),
	}
	_, exhausted, err := evaluateTargetPagination(pages, activeListRequiredSeries, 20)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if exhausted {
		t.Fatal("an is_starred=true-only observation must not satisfy an is_starred=false requirement")
	}
}

func TestEvaluateTargetPaginationArchivedOnlySeriesIsIncomplete(t *testing.T) {
	pages := []conversationPage{
		pageWithDescriptor("archived-only", 0, 28, 2, 2, "d0", boolPtr(true), boolPtr(false), false, "a"),
	}
	_, exhausted, err := evaluateTargetPagination(pages, activeListRequiredSeries, 20)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if exhausted {
		t.Fatal("an is_archived=true-only observation must not satisfy the active (is_archived=false) requirement")
	}
}

func TestEvaluateTargetPaginationCanonicalSeriesExhausted(t *testing.T) {
	pages := []conversationPage{
		pageWithDescriptor("canonical", 0, 2, 2, 2, "d0", boolPtr(false), boolPtr(false), false, "a", "b"),
		pageWithDescriptor("canonical", 2, 2, 1, 1, "d1", boolPtr(false), boolPtr(false), false, "c"),
	}
	got, exhausted, err := evaluateTargetPagination(pages, activeListRequiredSeries, 20)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !exhausted {
		t.Fatal("expected the canonical series' contiguous chain to be recognized as exhausted")
	}
	if len(got) != 3 {
		t.Fatalf("got %d conversations, want 3", len(got))
	}
}

func TestEvaluateTargetPaginationCanonicalSeriesNotExhausted(t *testing.T) {
	pages := []conversationPage{
		pageWithDescriptor("canonical", 0, 28, 28, 28, "d0", boolPtr(false), boolPtr(false), false, "a"),
	}
	_, exhausted, err := evaluateTargetPagination(pages, activeListRequiredSeries, 20)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if exhausted {
		t.Fatal("a full first page alone must not satisfy the requirement")
	}
}

func TestEvaluateTargetPaginationIrrelevantSeriesDoesNotBlockCompleteness(t *testing.T) {
	canonical := pageWithDescriptor("canonical", 0, 2, 1, 1, "d0", boolPtr(false), boolPtr(false), false, "a")
	starredPartial := pageWithDescriptor("starred", 0, 28, 28, 28, "d1", boolPtr(false), boolPtr(true), false, "b")
	pages := []conversationPage{canonical, starredPartial}

	// Config A: is_starred is not part of the requirement — the starred series is irrelevant.
	irrelevantConfig := []seriesRequirement{
		{RequireIsArchived: boolPtr(false), RequireIsStarred: boolPtr(false), ForbidUnknownParameters: true},
	}
	_, exhausted, err := evaluateTargetPagination(pages, irrelevantConfig, 20)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !exhausted {
		t.Fatal("an irrelevant, partially-observed series must not block completeness of the required series")
	}

	// Config B: both series are required — the starred series' incompleteness now matters.
	bothRequiredConfig := []seriesRequirement{
		{RequireIsArchived: boolPtr(false), RequireIsStarred: boolPtr(false), ForbidUnknownParameters: true},
		{RequireIsArchived: boolPtr(false), RequireIsStarred: boolPtr(true), ForbidUnknownParameters: true},
	}
	_, exhausted, err = evaluateTargetPagination(pages, bothRequiredConfig, 20)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if exhausted {
		t.Fatal("when both series are required, the incomplete starred series must block completeness")
	}
}

func TestEvaluateTargetPaginationMultipleRequiredSeries(t *testing.T) {
	required := []seriesRequirement{
		{RequireIsArchived: boolPtr(false), RequireIsStarred: boolPtr(false), ForbidUnknownParameters: true},
		{RequireIsArchived: boolPtr(false), RequireIsStarred: boolPtr(true), ForbidUnknownParameters: true},
	}
	aOnly := []conversationPage{
		pageWithDescriptor("a", 0, 2, 1, 1, "d0", boolPtr(false), boolPtr(false), false, "x"),
	}
	if _, exhausted, err := evaluateTargetPagination(aOnly, required, 20); err != nil {
		t.Fatalf("unexpected error: %v", err)
	} else if exhausted {
		t.Fatal("required series B missing entirely must make the result incomplete")
	}

	both := append(append([]conversationPage{}, aOnly...),
		pageWithDescriptor("b", 0, 2, 1, 1, "d1", boolPtr(false), boolPtr(true), false, "y"),
	)
	got, exhausted, err := evaluateTargetPagination(both, required, 20)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !exhausted {
		t.Fatal("A and B both exhausted must make the result complete")
	}
	if len(got) != 2 {
		t.Fatalf("got %d conversations, want 2", len(got))
	}
}

func TestEvaluateTargetPaginationUnknownParameterExcludesSeries(t *testing.T) {
	// Otherwise-canonical-looking series, but it carries a query parameter this bridge doesn't
	// understand — it must never be trusted as representing the known target universe.
	pages := []conversationPage{
		pageWithDescriptor("canonical-but-unknown", 0, 2, 1, 1, "d0", boolPtr(false), boolPtr(false), true, "a"),
	}
	_, exhausted, err := evaluateTargetPagination(pages, activeListRequiredSeries, 20)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if exhausted {
		t.Fatal("a series carrying an unrecognized query parameter must never satisfy a requirement that forbids it")
	}
}

func TestEvaluateTargetPaginationIgnoresSchemaDriftInIrrelevantSeries(t *testing.T) {
	// The core coverage-layer guarantee: an irrelevant series (here, archived — out of scope for the
	// active-list requirement) is excluded BEFORE mergeProjectPages ever validates it, so even a
	// completely malformed irrelevant series (unrecognized collection, here) cannot block or error
	// out completeness of the series that actually matters.
	canonical := pageWithDescriptor("canonical", 0, 2, 1, 1, "d0", boolPtr(false), boolPtr(false), false, "a")
	brokenIrrelevant := conversationPage{
		SeriesKey: "archived-broken", IsArchived: "true", IsStarred: "false",
		Offset: 0, Limit: 28, RecognizedCollection: false, RawItemCount: 0,
	}
	pages := []conversationPage{canonical, brokenIrrelevant}
	_, exhausted, err := evaluateTargetPagination(pages, activeListRequiredSeries, 20)
	if err != nil {
		t.Fatalf("schema drift in an irrelevant (archived) series must not reach the merge step: %v", err)
	}
	if !exhausted {
		t.Fatal("expected the canonical series alone to satisfy the requirement")
	}
}

func TestEvaluateTargetPaginationPropagatesSchemaDriftWithinRequiredSeries(t *testing.T) {
	pages := []conversationPage{
		{
			SeriesKey: "canonical", IsArchived: "false", IsStarred: "false", Order: "updated",
			Offset: 0, Limit: 28, RecognizedCollection: false, RawItemCount: 0, RawIdentityDigest: "d0",
		},
	}
	if _, _, err := evaluateTargetPagination(pages, activeListRequiredSeries, 20); err == nil {
		t.Fatal("expected schema drift within a required series to still fail closed")
	}
}

func TestEvaluateActiveListCompletenessCoverageIsUnknownEvenWhenPaginationCompletes(t *testing.T) {
	// Live evidence in this account never exercised is_starred=true, so even a fully
	// pagination-exhausted canonical series must not be reported as full COVERAGE completeness —
	// only pagination completeness. See activeListCoverageCaveat.
	pages := []conversationPage{
		pageWithDescriptor("canonical", 0, 2, 1, 1, "d0", boolPtr(false), boolPtr(false), false, "a"),
	}
	_, pagination, coverage, err := evaluateActiveListCompleteness(pages, nil, 20)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pagination.Status != "COMPLETE" {
		t.Fatalf("pagination = %s, want COMPLETE", pagination.Status)
	}
	if coverage.Status != "UNKNOWN" {
		t.Fatalf("coverage = %s, want UNKNOWN (is_starred semantics unconfirmed)", coverage.Status)
	}
}

func TestClassifyForActiveListCanonicalMatch(t *testing.T) {
	d := globalConversationsPageDiagnostic{HideSnorlax: "false", IsArchived: "false", IsStarred: "false", Order: "updated"}
	classification, reason := classifyForActiveList(d)
	if classification != seriesMatch {
		t.Fatalf("classification = %s (%s), want MATCH", classification, reason)
	}
}

func TestClassifyForActiveListCanonicalMatchWithHideSnorlaxAbsent(t *testing.T) {
	// Real evidence: every target-relevant capture ever observed live carries hide_snorlax absent,
	// never explicitly "false" — this must classify identically to the explicit "false" case.
	d := globalConversationsPageDiagnostic{HideSnorlax: "absent", IsArchived: "false", IsStarred: "false", Order: "updated"}
	classification, reason := classifyForActiveList(d)
	if classification != seriesMatch {
		t.Fatalf("classification = %s (%s), want MATCH", classification, reason)
	}
}

func TestClassifyForActiveListProjectExcluding(t *testing.T) {
	d := globalConversationsPageDiagnostic{HideSnorlax: "true", IsArchived: "false", IsStarred: "false", Order: "updated"}
	classification, _ := classifyForActiveList(d)
	if classification != seriesDefinitelyIrrelevant {
		t.Fatalf("classification = %s, want DEFINITELY_IRRELEVANT", classification)
	}
}

func TestClassifyForActiveListDefinitelyIrrelevantMalformedSeriesIsNeverBlamed(t *testing.T) {
	// is_archived=true is proven out of scope regardless of anything else about the response —
	// including a malformed collection shape, which classification must not even look at.
	d := globalConversationsPageDiagnostic{HideSnorlax: "false", IsArchived: "true", IsStarred: "false", Order: "updated", RecognizedCollection: false}
	classification, _ := classifyForActiveList(d)
	if classification != seriesDefinitelyIrrelevant {
		t.Fatalf("classification = %s, want DEFINITELY_IRRELEVANT (recognizedCollection must be irrelevant to this decision)", classification)
	}
}

func TestClassifyForActiveListRelevantMalformedSeriesStillMatches(t *testing.T) {
	// The mirror-image case: a genuinely relevant series that happens to be malformed still
	// classifies MATCH — the malformation is caught downstream (mergeProjectPages), not here.
	d := globalConversationsPageDiagnostic{HideSnorlax: "false", IsArchived: "false", IsStarred: "false", Order: "updated", RecognizedCollection: false}
	classification, reason := classifyForActiveList(d)
	if classification != seriesMatch {
		t.Fatalf("classification = %s (%s), want MATCH", classification, reason)
	}
}

func TestClassifyForActiveListUnknownSemanticValues(t *testing.T) {
	tests := []struct {
		name string
		d    globalConversationsPageDiagnostic
	}{
		{"hide_snorlax unexpected", globalConversationsPageDiagnostic{HideSnorlax: "unexpected", IsArchived: "false", IsStarred: "false", Order: "updated"}},
		{"is_archived yes", globalConversationsPageDiagnostic{HideSnorlax: "false", IsArchived: "yes", IsStarred: "false", Order: "updated"}},
		{"is_archived absent", globalConversationsPageDiagnostic{HideSnorlax: "false", IsArchived: "absent", IsStarred: "false", Order: "updated"}},
		{"is_starred 1", globalConversationsPageDiagnostic{HideSnorlax: "false", IsArchived: "false", IsStarred: "1", Order: "updated"}},
		{"is_starred absent", globalConversationsPageDiagnostic{HideSnorlax: "false", IsArchived: "false", IsStarred: "absent", Order: "updated"}},
		{"order alphabetical", globalConversationsPageDiagnostic{HideSnorlax: "false", IsArchived: "false", IsStarred: "false", Order: "alphabetical"}},
		{"order absent", globalConversationsPageDiagnostic{HideSnorlax: "false", IsArchived: "false", IsStarred: "false", Order: "absent"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			classification, reason := classifyForActiveList(test.d)
			if classification != seriesUnknown {
				t.Fatalf("classification = %s (%s), want UNKNOWN", classification, reason)
			}
		})
	}
}

func TestClassifyForActiveListUnknownParameterIsUnknown(t *testing.T) {
	d := globalConversationsPageDiagnostic{HideSnorlax: "false", IsArchived: "false", IsStarred: "false", Order: "updated", HasUnknownParameters: true}
	classification, reason := classifyForActiveList(d)
	if classification != seriesUnknown {
		t.Fatalf("classification = %s (%s), want UNKNOWN", classification, reason)
	}
}

func TestEvaluateTargetPaginationOrderUpdatedMatchesTarget(t *testing.T) {
	p := pageWithDescriptor("canonical", 0, 2, 1, 1, "d0", boolPtr(false), boolPtr(false), false, "a")
	_, exhausted, err := evaluateTargetPagination([]conversationPage{p}, activeListRequiredSeries, 20)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !exhausted {
		t.Fatal("order=updated must satisfy the target requirement")
	}
}

func TestEvaluateTargetPaginationOtherOrderDoesNotMatchTarget(t *testing.T) {
	p := pageWithDescriptor("other-order", 0, 28, 5, 5, "d0", boolPtr(false), boolPtr(false), false, "a")
	p.Order = "other"
	_, exhausted, err := evaluateTargetPagination([]conversationPage{p}, activeListRequiredSeries, 20)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if exhausted {
		t.Fatal("a different order value must not satisfy the target requirement")
	}
}

func TestEvaluateTargetPaginationAbsentOrderDoesNotMatchTarget(t *testing.T) {
	p := pageWithDescriptor("absent-order", 0, 28, 5, 5, "d0", boolPtr(false), boolPtr(false), false, "a")
	p.Order = "absent"
	_, exhausted, err := evaluateTargetPagination([]conversationPage{p}, activeListRequiredSeries, 20)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if exhausted {
		t.Fatal("an absent order value must not be assumed safe for the target requirement")
	}
}

func TestEvaluateActiveListCompletenessComposesUnknownObservations(t *testing.T) {
	pages := []conversationPage{
		pageWithDescriptor("canonical", 0, 2, 1, 1, "d0", boolPtr(false), boolPtr(false), false, "a"),
	}
	extra := []string{`capture 7: hide_snorlax value not recognized ("unexpected")`}
	_, pagination, coverage, err := evaluateActiveListCompleteness(pages, extra, 20)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pagination.Status != "COMPLETE" {
		t.Fatalf("pagination = %s, want COMPLETE", pagination.Status)
	}
	if coverage.Status != "UNKNOWN" {
		t.Fatalf("coverage = %s, want UNKNOWN", coverage.Status)
	}
	if !strings.Contains(coverage.Reason, extra[0]) {
		t.Fatalf("coverage.Reason = %q, want it to include the live-observed unknown-semantics note", coverage.Reason)
	}
}

func TestMergeProjectPagesNoObservedSeriesIsIncompleteNotError(t *testing.T) {
	got, exhausted, err := mergeProjectPages(nil, 20)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if exhausted {
		t.Fatal("no observed series must never be reported as exhausted")
	}
	if len(got) != 0 {
		t.Fatalf("got %d conversations, want 0", len(got))
	}
}

// --- Phase 5 is_starred coverage experiment: controlled membership evidence ---

func TestConversationFingerprintIsStableForSameID(t *testing.T) {
	id := "11111111-2222-3333-4444-555555555555"
	if conversationFingerprint(id) != conversationFingerprint(id) {
		t.Fatal("expected the same raw ID to always produce the same fingerprint")
	}
}

func TestConversationFingerprintDiffersForDifferentIDs(t *testing.T) {
	a := conversationFingerprint("11111111-2222-3333-4444-555555555555")
	b := conversationFingerprint("66666666-7777-8888-9999-000000000000")
	if a == b {
		t.Fatal("expected different raw IDs to produce different fingerprints")
	}
}

func TestConversationFingerprintNeverEqualsTheRawID(t *testing.T) {
	id := "11111111-2222-3333-4444-555555555555"
	fp := conversationFingerprint(id)
	if fp == id {
		t.Fatal("fingerprint must never equal the raw ID it was derived from")
	}
	if strings.Contains(fp, id) || strings.Contains(id, fp) {
		t.Fatal("fingerprint must not contain, or be contained by, the raw ID")
	}
	if len(fp) != 12 {
		t.Fatalf("fingerprint length = %d, want 12 (the diagnostic-safe short form)", len(fp))
	}
}

func TestMembershipEvidenceNeverLeaksRawIDs(t *testing.T) {
	before := conv("11111111-2222-3333-4444-555555555555", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
	after := conv("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "99999999-8888-7777-6666-555555555555")
	ev := compareMembership(before, after)
	rendered := fmt.Sprintf("%+v", ev)
	for _, id := range []string{
		"11111111-2222-3333-4444-555555555555",
		"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		"99999999-8888-7777-6666-555555555555",
	} {
		if strings.Contains(rendered, id) {
			t.Fatalf("membershipEvidence rendering leaked a raw conversation ID: %s", rendered)
		}
	}
	if len(ev.DisappearedDigests) != 1 || ev.DisappearedDigests[0] != conversationFingerprint("11111111-2222-3333-4444-555555555555") {
		t.Fatalf("DisappearedDigests = %v, want exactly the fingerprint of the removed ID", ev.DisappearedDigests)
	}
	if len(ev.AppearedDigests) != 1 || ev.AppearedDigests[0] != conversationFingerprint("99999999-8888-7777-6666-555555555555") {
		t.Fatalf("AppearedDigests = %v, want exactly the fingerprint of the added ID", ev.AppearedDigests)
	}
	if ev.BeforeCount != 2 || ev.AfterCount != 2 {
		t.Fatalf("BeforeCount/AfterCount = %d/%d, want 2/2", ev.BeforeCount, ev.AfterCount)
	}
}

func TestCompareMembershipNoChangeIsOutcomeA(t *testing.T) {
	// The Outcome A signal (README Phase D): the target series is unchanged across an observation
	// pair, evidence that whatever happened did not exclude any previously-present member.
	set := conv("a", "b", "c")
	ev := compareMembership(set, set)
	if len(ev.DisappearedDigests) != 0 || len(ev.AppearedDigests) != 0 {
		t.Fatalf("expected no membership change, got disappeared=%v appeared=%v", ev.DisappearedDigests, ev.AppearedDigests)
	}
}

func TestCompareMembershipBaselinePinnedRestoredCycle(t *testing.T) {
	// Simulates the full Phase B/D/H cycle this spike's runPinExperiment drives interactively: a
	// pin action removes one conversation from the target series (Outcome B signal), and restoring
	// it afterward should bring the series back to exactly its original membership.
	baseline := conv("a", "b", "c")
	afterPin := conv("a", "c") // "b" disappeared — the pinned conversation
	afterRestore := conv("a", "b", "c")

	pinEvidence := compareMembership(baseline, afterPin)
	if len(pinEvidence.DisappearedDigests) != 1 || pinEvidence.DisappearedDigests[0] != conversationFingerprint("b") {
		t.Fatalf("pin evidence disappeared = %v, want exactly fingerprint(b)", pinEvidence.DisappearedDigests)
	}
	if len(pinEvidence.AppearedDigests) != 0 {
		t.Fatalf("pin evidence appeared = %v, want none", pinEvidence.AppearedDigests)
	}

	restoreEvidence := compareMembership(baseline, afterRestore)
	if len(restoreEvidence.DisappearedDigests) != 0 || len(restoreEvidence.AppearedDigests) != 0 {
		t.Fatalf("restore evidence should match baseline exactly, got disappeared=%v appeared=%v",
			restoreEvidence.DisappearedDigests, restoreEvidence.AppearedDigests)
	}
}

func TestCompareMembershipAmbiguousChangeIsNotSingleAttributable(t *testing.T) {
	// More than one member changing between two observations can't be cleanly attributed to a single
	// controlled pin action — runPinExperiment reports this as NOT VERIFIED rather than guessing.
	baseline := conv("a", "b", "c")
	after := conv("a") // both "b" and "c" disappeared
	ev := compareMembership(baseline, after)
	if len(ev.DisappearedDigests) != 2 {
		t.Fatalf("expected 2 disappeared entries for an ambiguous multi-member change, got %v", ev.DisappearedDigests)
	}
}

// diag builds a minimal globalConversationsPageDiagnostic fixture. isStarred/hideSnorlax/isArchived
// default to the values that make classifyForActiveList return MATCH; override via the returned
// value's fields for a specific test.
func diag(captureID int) globalConversationsPageDiagnostic {
	return globalConversationsPageDiagnostic{
		CaptureID: captureID, HideSnorlax: "absent", IsArchived: "false", IsStarred: "false", Order: "updated",
	}
}

func TestMaxMatchCaptureIDIgnoresIrrelevantSeries(t *testing.T) {
	// An irrelevant series (hide_snorlax=true) being freshly captured, even with a HIGHER CaptureID
	// than the last real MATCH capture, must never be mistaken for the target series being refetched.
	matchCapture := diag(5)
	irrelevant := diag(9)
	irrelevant.HideSnorlax = "true"
	got := maxMatchCaptureID([]globalConversationsPageDiagnostic{matchCapture, irrelevant})
	if got != 5 {
		t.Fatalf("maxMatchCaptureID = %d, want 5 (the irrelevant capture's higher ID must not count)", got)
	}
}

func TestMaxMatchCaptureIDAdvancesOnFreshMatch(t *testing.T) {
	got := maxMatchCaptureID([]globalConversationsPageDiagnostic{diag(5), diag(9)})
	if got != 9 {
		t.Fatalf("maxMatchCaptureID = %d, want 9", got)
	}
}

func TestSelectFreshMatchDiagnosticsExcludesStaleAndIrrelevant(t *testing.T) {
	stale := diag(5)      // <= watermark: must be excluded regardless of classification
	irrelevant := diag(9) // > watermark, but not MATCH: must be excluded
	irrelevant.IsArchived = "true"
	fresh := diag(12) // > watermark and MATCH: must be selected
	got := selectFreshMatchDiagnostics([]globalConversationsPageDiagnostic{stale, irrelevant, fresh}, 5)
	if len(got) != 1 || got[0].CaptureID != 12 {
		t.Fatalf("selectFreshMatchDiagnostics = %+v, want exactly the fresh MATCH capture (12)", got)
	}
}

func TestSelectFreshMatchDiagnosticsPhaseIsolation(t *testing.T) {
	// The exact scenario the task describes: a baseline capture (SeriesKey S, offset 0, digest A)
	// and a post-pin capture of the SAME SeriesKey+Offset (digest B) are two legitimate, different
	// point-in-time snapshots. Treating them as one enumeration's input would trip mergeProjectPages'
	// "same page reported a different raw identity" schema-drift check. Two SEPARATE
	// selectFreshMatchDiagnostics calls (one per phase) must each see only their own capture, so
	// merging each phase's own result independently never errors.
	baselineCapture := diag(5)
	baselineCapture.SeriesKey = stringPtr("S")
	baselineCapture.Offset, baselineCapture.Limit = intPtr(0), intPtr(28)
	baselineCapture.RecognizedCollection = true
	baselineCapture.RawIdentityDigest = "digest-A"

	postPinCapture := diag(9)
	postPinCapture.SeriesKey = stringPtr("S")
	postPinCapture.Offset, postPinCapture.Limit = intPtr(0), intPtr(28)
	postPinCapture.RecognizedCollection = true
	postPinCapture.RawIdentityDigest = "digest-B"

	// At baseline time, the bridge has only observed the baseline capture — the post-pin one does
	// not exist yet. all is what the bridge has accumulated BY THE TIME the after-pin phase runs
	// (both, since capture history is never discarded).
	all := []globalConversationsPageDiagnostic{baselineCapture, postPinCapture}

	// Baseline phase: watermark 0 sees only the baseline capture (the only one observed so far).
	baselinePhase := selectFreshMatchDiagnostics([]globalConversationsPageDiagnostic{baselineCapture}, 0)
	if len(baselinePhase) != 1 || baselinePhase[0].RawIdentityDigest != "digest-A" {
		t.Fatalf("baseline phase selected %+v, want exactly the baseline capture", baselinePhase)
	}
	baselinePage := conversationPage{
		SeriesKey: *baselinePhase[0].SeriesKey, Offset: *baselinePhase[0].Offset, Limit: *baselinePhase[0].Limit,
		RecognizedCollection: true, RawIdentityDigest: baselinePhase[0].RawIdentityDigest,
	}
	if _, _, err := mergeProjectPages([]conversationPage{baselinePage}, 20); err != nil {
		t.Fatalf("baseline phase alone must merge without error: %v", err)
	}

	// After-pin phase: watermark 5 (baseline's own CaptureID) sees only the post-pin capture.
	afterPinPhase := selectFreshMatchDiagnostics(all, 5)
	if len(afterPinPhase) != 1 || afterPinPhase[0].RawIdentityDigest != "digest-B" {
		t.Fatalf("after-pin phase selected %+v, want exactly the post-pin capture", afterPinPhase)
	}
	afterPinPage := conversationPage{
		SeriesKey: *afterPinPhase[0].SeriesKey, Offset: *afterPinPhase[0].Offset, Limit: *afterPinPhase[0].Limit,
		RecognizedCollection: true, RawIdentityDigest: afterPinPhase[0].RawIdentityDigest,
	}
	if _, _, err := mergeProjectPages([]conversationPage{afterPinPage}, 20); err != nil {
		t.Fatalf("after-pin phase alone must merge without error: %v", err)
	}

	// Mixing both phases' captures into ONE mergeProjectPages call, by contrast, must still fail
	// closed — this documents why phase isolation via the watermark is necessary, not optional.
	if _, _, err := mergeProjectPages([]conversationPage{baselinePage, afterPinPage}, 20); err == nil {
		t.Fatal("mixing two phases' captures of the same series+offset must fail closed (existing invariant, not weakened)")
	}
}

func intPtr(v int) *int { return &v }

func TestControlledSampleFromPinsDeltaCleanSingleAddition(t *testing.T) {
	before := pinsSnapshot{IDs: []string{"a"}}
	after := pinsSnapshot{IDs: []string{"a", "b"}}
	id, ok, reason := controlledSampleFromPinsDelta(true, before, after)
	if !ok || id != "b" {
		t.Fatalf("controlledSampleFromPinsDelta = (%q, %t, %q), want (\"b\", true, \"\")", id, ok, reason)
	}
}

func TestControlledSampleFromPinsDeltaAmbiguousAdditionsNotVerified(t *testing.T) {
	before := pinsSnapshot{IDs: []string{"a"}}
	after := pinsSnapshot{IDs: []string{"a", "b", "c"}}
	_, ok, _ := controlledSampleFromPinsDelta(true, before, after)
	if ok {
		t.Fatal("expected NOT VERIFIED (ok=false) for more than one added pin")
	}
}

func TestControlledSampleFromPinsDeltaNotFreshIsNotVerified(t *testing.T) {
	before := pinsSnapshot{IDs: []string{"a"}}
	after := pinsSnapshot{IDs: []string{"a", "b"}}
	_, ok, _ := controlledSampleFromPinsDelta(false, before, after)
	if ok {
		t.Fatal("expected NOT VERIFIED (ok=false) when the pins capture was not confirmed fresh")
	}
}

func TestControlledSampleFromPinsDeltaWithRemovalIsNotVerified(t *testing.T) {
	before := pinsSnapshot{IDs: []string{"a"}}
	after := pinsSnapshot{IDs: []string{"b"}} // "a" removed, "b" added — not a clean single addition
	_, ok, _ := controlledSampleFromPinsDelta(true, before, after)
	if ok {
		t.Fatal("expected NOT VERIFIED (ok=false) when the delta also removed an existing pin")
	}
}

func TestPinExperimentOutcomeA(t *testing.T) {
	got := pinExperimentOutcome(true, "", true, true, true)
	if !strings.HasPrefix(got, "A:") {
		t.Fatalf("pinExperimentOutcome = %q, want an A result", got)
	}
}

func TestPinExperimentOutcomeB(t *testing.T) {
	got := pinExperimentOutcome(true, "", true, true, false)
	if !strings.HasPrefix(got, "B:") {
		t.Fatalf("pinExperimentOutcome = %q, want a B result", got)
	}
}

func TestPinExperimentOutcomeNotVerifiedWithoutControlledSample(t *testing.T) {
	got := pinExperimentOutcome(false, "no fresh pins capture", true, true, true)
	if !strings.Contains(got, "NOT VERIFIED") {
		t.Fatalf("pinExperimentOutcome = %q, want NOT VERIFIED", got)
	}
}

func TestPinExperimentOutcomeNotVerifiedWhenNotPresentBefore(t *testing.T) {
	got := pinExperimentOutcome(true, "", false, true, true)
	if !strings.Contains(got, "NOT VERIFIED") || !strings.Contains(got, "baseline") {
		t.Fatalf("pinExperimentOutcome = %q, want NOT VERIFIED citing baseline invalidity", got)
	}
}

func TestPinExperimentOutcomeNotVerifiedWhenTargetStale(t *testing.T) {
	// The exact "stale target" case: membership appears unchanged (presentAfter=true) but no fresh
	// target capture was observed — this must never be read as Outcome A.
	got := pinExperimentOutcome(true, "", true, false, true)
	if !strings.Contains(got, "NOT VERIFIED") {
		t.Fatalf("pinExperimentOutcome = %q, want NOT VERIFIED when the target snapshot is not fresh", got)
	}
}

func TestPinExperimentRestoreOutcomePass(t *testing.T) {
	got := pinExperimentRestoreOutcome(true, true, true, true)
	if !strings.HasPrefix(got, "PASS") {
		t.Fatalf("pinExperimentRestoreOutcome = %q, want PASS", got)
	}
}

func TestPinExperimentRestoreOutcomeNotVerifiedCases(t *testing.T) {
	tests := []struct {
		name                                                        string
		controlledOK, pinsRemoved, restoreTargetFresh, presentAfter bool
	}{
		{"no controlled sample", false, true, true, true},
		{"pins did not confirm removal", true, false, true, true},
		{"restore target snapshot stale", true, true, false, true},
		{"controlled sample absent after restore", true, true, true, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := pinExperimentRestoreOutcome(test.controlledOK, test.pinsRemoved, test.restoreTargetFresh, test.presentAfter)
			if !strings.Contains(got, "NOT VERIFIED") {
				t.Fatalf("pinExperimentRestoreOutcome(%v) = %q, want NOT VERIFIED", test, got)
			}
		})
	}
}

func TestDiffConversationIDsIgnoresOrder(t *testing.T) {
	before := conv("a", "b", "c")
	after := conv("c", "b", "a")
	disappeared, appeared := diffConversationIDs(before, after)
	if len(disappeared) != 0 || len(appeared) != 0 {
		t.Fatalf("expected no diff for a reordered but identical set, got disappeared=%v appeared=%v", disappeared, appeared)
	}
}
