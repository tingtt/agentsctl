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

func TestMergeProjectPagesSinglePage(t *testing.T) {
	pages := []conversationPage{
		{Sequence: 0, RawItemCount: 2, Limit: 28, Items: conv("a", "b")},
	}
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

func TestMergeProjectPagesTwoPages(t *testing.T) {
	pages := []conversationPage{
		{Sequence: 0, RawItemCount: 2, Limit: 2, Items: conv("a", "b")},
		{Sequence: 1, RawItemCount: 1, Limit: 2, Items: conv("c")},
	}
	got, exhausted, err := mergeProjectPages(pages, 20)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !exhausted {
		t.Fatal("the highest-sequence page was short of its limit and should be treated as final")
	}
	if len(got) != 3 {
		t.Fatalf("got %d conversations, want 3", len(got))
	}
}

func TestMergeProjectPagesEmptyFinalPage(t *testing.T) {
	pages := []conversationPage{
		{Sequence: 0, RawItemCount: 2, Limit: 2, Items: conv("a", "b")},
		{Sequence: 1, RawItemCount: 0, Limit: 2, Items: nil},
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

func TestMergeProjectPagesNotExhaustedWhenLastPageIsFull(t *testing.T) {
	// This is the live-observed case: a page exactly as long as its limit does not, by itself,
	// prove no further page exists. Real ChatGPT accounts observed this shape at offset 0 with
	// limit 28 while a genuine next page was never reachable, so this must stay incomplete.
	pages := []conversationPage{
		{Sequence: 0, RawItemCount: 28, Limit: 28, Items: conv("a", "b")},
	}
	_, exhausted, err := mergeProjectPages(pages, 20)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if exhausted {
		t.Fatal("a full page must not be treated as proof of exhaustion")
	}
}

func TestMergeProjectPagesDuplicateIDsAcrossPages(t *testing.T) {
	pages := []conversationPage{
		{Sequence: 0, RawItemCount: 2, Limit: 2, Items: conv("a", "b")},
		{Sequence: 1, RawItemCount: 1, Limit: 2, Items: conv("b")},
	}
	got, _, err := mergeProjectPages(pages, 20)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d conversations, want 2 after deduping the repeated ID", len(got))
	}
}

func TestMergeProjectPagesRepeatedIdenticalSequenceIsIdempotent(t *testing.T) {
	pages := []conversationPage{
		{Sequence: 0, RawItemCount: 2, Limit: 28, Items: conv("a", "b")},
		{Sequence: 0, RawItemCount: 2, Limit: 28, Items: conv("a", "b")},
	}
	got, _, err := mergeProjectPages(pages, 20)
	if err != nil {
		t.Fatalf("unexpected error re-observing the same sequence with identical content: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d conversations, want 2", len(got))
	}
}

func TestMergeProjectPagesRepeatedSequenceWithDifferentContentFailsClosed(t *testing.T) {
	pages := []conversationPage{
		{Sequence: 0, RawItemCount: 2, Limit: 28, Items: conv("a", "b")},
		{Sequence: 0, RawItemCount: 2, Limit: 28, Items: conv("c", "d")},
	}
	if _, _, err := mergeProjectPages(pages, 20); err == nil {
		t.Fatal("expected an error when the same sequence reports different conversations")
	}
}

func TestMergeProjectPagesMalformedPageFailsClosed(t *testing.T) {
	pages := []conversationPage{
		{Sequence: 0, RawItemCount: 2, Limit: 0, Items: conv("a", "b")},
	}
	if _, _, err := mergeProjectPages(pages, 20); err == nil {
		t.Fatal("expected an error for a page with a non-positive limit")
	}
}

func TestMergeProjectPagesMissingIdentityFailsClosed(t *testing.T) {
	pages := []conversationPage{
		{Sequence: 0, RawItemCount: 1, Limit: 28, Items: []conversation{{ID: ""}}},
	}
	if _, _, err := mergeProjectPages(pages, 20); err == nil {
		t.Fatal("expected an error for a conversation missing a stable identity")
	}
}

func TestMergeProjectPagesRejectsHideSnorlaxPage(t *testing.T) {
	pages := []conversationPage{
		{Sequence: 0, HideSnorlax: true, RawItemCount: 0, Limit: 28, Items: nil},
	}
	if _, _, err := mergeProjectPages(pages, 20); err == nil {
		t.Fatal("expected an error when a Project-excluding page reaches the merge step")
	}
}

func TestMergeProjectPagesMaxPageGuard(t *testing.T) {
	pages := make([]conversationPage, 21)
	for i := range pages {
		pages[i] = conversationPage{Sequence: i, RawItemCount: 28, Limit: 28, Items: conv(fmt.Sprintf("id-%d", i))}
	}
	if _, _, err := mergeProjectPages(pages, 20); err == nil {
		t.Fatal("expected an error once the page count exceeds the defensive bound")
	}
}

func TestMergeProjectPagesFiltersToConfiguredProjectAcrossPages(t *testing.T) {
	// globalConversationsFrom (JS side) already restricts items to the requested Project before
	// they cross the bridge; this test documents that mergeProjectPages trusts and preserves that
	// filtering rather than re-deriving it, so a conversation from a different Project passed in
	// by mistake is not silently dropped or merged away — it is just data mergeProjectPages moves.
	other := conversation{ID: "z", ProjectID: "g-p-other"}
	pages := []conversationPage{
		{Sequence: 0, RawItemCount: 3, Limit: 28, Items: append(conv("a", "b"), other)},
	}
	got, _, err := mergeProjectPages(pages, 20)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d conversations, want 3 (mergeProjectPages does not re-filter by Project)", len(got))
	}
}
