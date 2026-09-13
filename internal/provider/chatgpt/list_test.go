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

func TestSelectSeriesFailsClosedOnAmbiguity(t *testing.T) {
	captures := []capture{
		capturePage(1, "small", "0", "", item(conversationA, "A", "2026-01-01T00:00:00Z")),
		capturePage(2, "project", "0", "", item(conversationA, "A", "2026-01-01T00:00:00Z"), item(conversationB, "B", "2026-02-01T00:00:00Z")),
	}
	if got, err := selectSeries(captures, 2); err != nil || got != "project" {
		t.Fatalf("series=%q err=%v", got, err)
	}
	captures = append(captures, capturePage(3, "also-project", "0", "", item(conversationA, "A", "2026-01-01T00:00:00Z"), item(conversationB, "B", "2026-02-01T00:00:00Z")))
	if _, err := selectSeries(captures, 2); err == nil || !strings.Contains(err.Error(), "ambiguity") {
		t.Fatalf("ambiguity err=%v", err)
	}
}

func TestSelectSeriesAcceptsUniqueEmptyTerminalProject(t *testing.T) {
	captures := []capture{capturePage(1, "empty-project", "0", "")}
	if got, err := selectSeries(captures, 0); err != nil || got != "empty-project" {
		t.Fatalf("series=%q err=%v", got, err)
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
