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
