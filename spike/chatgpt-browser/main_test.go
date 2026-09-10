package main

import (
	"encoding/json"
	"fmt"
	"net"
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

// page builds a conversationPage fixture. rawItemCount/recognizedIDCount are independent of
// len(ids) on purpose: `ids` is the already Project-filtered subset the bridge returns, while
// rawItemCount/recognizedIDCount describe the full raw page (this Project's items plus everyone
// else's), which is exactly the distinction Requirement 5 cares about.
func page(seriesKey string, offset, limit, rawItemCount, recognizedIDCount int, digest string, ids ...string) conversationPage {
	return conversationPage{
		SeriesKey:         seriesKey,
		Offset:            offset,
		Limit:             limit,
		RawItemCount:      rawItemCount,
		RecognizedIDCount: recognizedIDCount,
		RawIdentityDigest: digest,
		Items:             conv(ids...),
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

func TestMergeProjectPagesNegativeOffsetFailsClosed(t *testing.T) {
	pages := []conversationPage{page("s", -1, 28, 1, 1, "d0", "a")}
	if _, _, err := mergeProjectPages(pages, 20); err == nil {
		t.Fatal("expected an error for a malformed/unrecognized (negative) offset")
	}
}

func TestMergeProjectPagesMissingIdentityFailsClosed(t *testing.T) {
	pages := []conversationPage{
		{SeriesKey: "s", Offset: 0, Limit: 28, RawItemCount: 1, RecognizedIDCount: 1, RawIdentityDigest: "d0", Items: []conversation{{ID: ""}}},
	}
	if _, _, err := mergeProjectPages(pages, 20); err == nil {
		t.Fatal("expected an error for a conversation missing a stable identity")
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
		{SeriesKey: "s", Offset: 0, Limit: 28, RawItemCount: 5, RecognizedIDCount: 5, RawIdentityDigest: "d0", KnownIDsMismatched: 1, Items: conv("a")},
	}
	if _, _, err := mergeProjectPages(pages, 20); err == nil {
		t.Fatal("expected an error when a known Project conversation ID no longer resolves to the configured Project")
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
