package chatgpt

import (
	"strings"
	"testing"
)

const (
	conversationA = "11111111-1111-1111-1111-111111111111"
	conversationB = "22222222-2222-2222-2222-222222222222"
	conversationC = "33333333-3333-3333-3333-333333333333"
)

func TestAssembleChainRequiresExplicitTerminalPage(t *testing.T) {
	terminal := capturePage(1, "series", "0", "", item(conversationA, "A", "2026-01-01T00:00:00Z"))
	got, complete, err := assembleChain([]capture{terminal}, "series", 10)
	if err != nil || !complete || len(got) != 1 || got[0].ID != conversationA {
		t.Fatalf("conversations=%+v complete=%t err=%v", got, complete, err)
	}

	incomplete := terminal
	incomplete.HasNextCursor = true
	incomplete.NextCursor = "opaque-next"
	got, complete, err = assembleChain([]capture{incomplete}, "series", 10)
	if err != nil || complete || len(got) != 1 {
		t.Fatalf("partial conversations=%+v complete=%t err=%v", got, complete, err)
	}
}

func TestAssembleChainForwardsOpaqueCursorsAndSortsByCreation(t *testing.T) {
	captures := []capture{
		capturePage(1, "series", "0", "opaque-a", item(conversationA, "A", "2026-01-01T00:00:00Z")),
		capturePage(2, "series", "opaque-a", "opaque-z", item(conversationC, "C", "2026-03-01T00:00:00Z")),
		capturePage(3, "series", "opaque-z", "", item(conversationB, "B", "2026-02-01T00:00:00Z")),
	}
	got, complete, err := assembleChain(captures, "series", 10)
	if err != nil || !complete {
		t.Fatalf("complete=%t err=%v", complete, err)
	}
	if len(got) != 3 || got[0].ID != conversationC || got[1].ID != conversationB || got[2].ID != conversationA {
		t.Fatalf("creation order=%+v", got)
	}
}

func TestAssembleChainDoesNotUseUpdateTimeForOrdering(t *testing.T) {
	older := item(conversationA, "A", "2026-01-01T00:00:00Z")
	older.UpdatedAt = "2026-04-01T00:00:00Z"
	newer := item(conversationB, "B", "2026-02-01T00:00:00Z")
	newer.UpdatedAt = "2026-03-01T00:00:00Z"

	got, complete, err := assembleChain([]capture{capturePage(1, "series", "0", "", older, newer)}, "series", 10)
	if err != nil || !complete {
		t.Fatalf("complete=%t err=%v", complete, err)
	}
	if len(got) != 2 || got[0].ID != conversationB || got[1].ID != conversationA {
		t.Fatalf("creation order=%+v", got)
	}
}

func TestAssembleChainRejectsCycleAndBrokenChain(t *testing.T) {
	cycle := []capture{
		capturePage(1, "series", "0", "opaque", item(conversationA, "A", "2026-01-01T00:00:00Z")),
		capturePage(2, "series", "opaque", "0", item(conversationB, "B", "2026-02-01T00:00:00Z")),
	}
	if _, _, err := assembleChain(cycle, "series", 10); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("cycle err=%v", err)
	}

	broken := []capture{
		capturePage(1, "series", "0", "expected", item(conversationA, "A", "2026-01-01T00:00:00Z")),
		capturePage(2, "series", "different", "", item(conversationB, "B", "2026-02-01T00:00:00Z")),
	}
	if _, _, err := assembleChain(broken, "series", 10); err == nil || !strings.Contains(err.Error(), "broken") {
		t.Fatalf("broken-chain err=%v", err)
	}
}

func TestAssembleChainDeduplicatesConversationIDsAcrossPages(t *testing.T) {
	captures := []capture{
		capturePage(1, "series", "0", "next", item(conversationA, "A", "2026-01-01T00:00:00Z")),
		capturePage(2, "series", "next", "", item(conversationA, "A", "2026-01-01T00:00:00Z"), item(conversationB, "B", "2026-02-01T00:00:00Z")),
	}
	got, complete, err := assembleChain(captures, "series", 10)
	if err != nil || !complete || len(got) != 2 {
		t.Fatalf("conversations=%+v complete=%t err=%v", got, complete, err)
	}
}

func TestRepeatedCursorKeepsLatestTokenOnlyWhenIdentitySetIsStable(t *testing.T) {
	captures := []capture{
		capturePage(1, "series", "0", "stale-token", item(conversationA, "A", "2026-01-01T00:00:00Z")),
		capturePage(2, "series", "0", "fresh-token", item(conversationA, "A", "2026-01-01T00:00:00Z")),
		capturePage(3, "series", "fresh-token", "", item(conversationB, "B", "2026-02-01T00:00:00Z")),
	}
	if got, complete, err := assembleChain(captures, "series", 10); err != nil || !complete || len(got) != 2 {
		t.Fatalf("conversations=%+v complete=%t err=%v", got, complete, err)
	}

	captures[1].Items = []capturedItem{item(conversationB, "B", "2026-02-01T00:00:00Z")}
	if _, _, err := assembleChain(captures, "series", 10); err == nil || !strings.Contains(err.Error(), "different conversation set") {
		t.Fatalf("changed identity set err=%v", err)
	}
}

func TestSelectInitialSeries(t *testing.T) {
	page := func(id int, series string, count int) capture {
		items := make([]capturedItem, count)
		for index := range items {
			items[index] = item(conversationA, "A", "2026-01-01T00:00:00Z")
		}
		return capturePage(id, series, "0", "", items...)
	}
	tests := []struct {
		name     string
		captures []capture
		links    int
		want     string
		wantErr  bool
	}{
		{name: "zero series"},
		{name: "single matching DOM", captures: []capture{page(1, "A", 10)}, links: 10, want: "A"},
		{name: "single mismatching DOM", captures: []capture{page(1, "A", 10)}, links: 7, want: "A"},
		{name: "multiple unique match", captures: []capture{page(1, "A", 5), page(2, "B", 10)}, links: 10, want: "B"},
		{name: "multiple zero matches", captures: []capture{page(1, "A", 5), page(2, "B", 7)}, links: 10, wantErr: true},
		{name: "multiple matching series", captures: []capture{page(1, "A", 10), page(2, "B", 10)}, links: 10, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := selectInitialSeries(tt.captures, scrollRegion{Found: true, ProjectLinkCount: tt.links})
			if got != tt.want || (err != nil) != tt.wantErr {
				t.Fatalf("series=%q err=%v", got, err)
			}
		})
	}
}

func TestPinnedSeriesIgnoresLaterCapturesFromAnotherSeries(t *testing.T) {
	captures := []capture{
		capturePage(1, "project", "0", "", item(conversationA, "A", "2026-01-01T00:00:00Z")),
		capturePage(2, "other", "0", "", item(conversationB, "B", "2026-02-01T00:00:00Z")),
	}
	got, complete, err := assembleChain(captures, "project", 10)
	if err != nil || !complete || len(got) != 1 || got[0].ID != conversationA {
		t.Fatalf("conversations=%+v complete=%t err=%v", got, complete, err)
	}
}

func TestAssembleChainRejectsMalformedItemAndMissingTerminalWithinBound(t *testing.T) {
	malformed := capturePage(1, "series", "0", "", item(conversationA, "", "2026-01-01T00:00:00Z"))
	if _, _, err := assembleChain([]capture{malformed}, "series", 10); err == nil || !strings.Contains(err.Error(), "title") {
		t.Fatalf("malformed err=%v", err)
	}

	bounded := []capture{
		capturePage(1, "series", "0", "next", item(conversationA, "A", "2026-01-01T00:00:00Z")),
		capturePage(2, "series", "next", "more", item(conversationB, "B", "2026-02-01T00:00:00Z")),
	}
	if _, complete, err := assembleChain(bounded, "series", 2); err == nil || complete || !strings.Contains(err.Error(), "maximum page count") {
		t.Fatalf("complete=%t err=%v", complete, err)
	}
}

func TestAssembleChainRejectsMissingResponseCursorState(t *testing.T) {
	page := capturePage(1, "series", "0", "", item(conversationA, "A", "2026-01-01T00:00:00Z"))
	page.CursorObserved = false
	if _, complete, err := assembleChain([]capture{page}, "series", 10); err == nil || complete || !strings.Contains(err.Error(), "explicit response cursor") {
		t.Fatalf("complete=%t err=%v", complete, err)
	}
}

func capturePage(captureID int, series, cursor, next string, items ...capturedItem) capture {
	return capture{
		CaptureID:      captureID,
		CursorIn:       cursor,
		SeriesKey:      series,
		Items:          items,
		CursorObserved: true,
		HasNextCursor:  next != "",
		NextCursor:     next,
	}
}

func item(id, title, createdAt string) capturedItem {
	return capturedItem{ID: id, Title: title, CreatedAt: createdAt, UpdatedAt: createdAt}
}
