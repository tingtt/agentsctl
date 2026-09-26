package localstate

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestChatGPTCatalogRoundTripsAcrossReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	refreshedAt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	catalog := ChatGPTCatalog{
		RefreshedAt: refreshedAt,
		Conversations: []ChatGPTConversation{
			{ID: "11111111-1111-1111-1111-111111111111", Title: "A", CreatedAt: refreshedAt.Add(-time.Hour), UpdatedAt: refreshedAt},
			{ID: "22222222-2222-2222-2222-222222222222", Title: "B", CreatedAt: refreshedAt.Add(-2 * time.Hour), UpdatedAt: refreshedAt},
		},
	}
	s := New(path)
	if err := s.SaveChatGPTCatalog("g-p-project", catalog); err != nil {
		t.Fatal(err)
	}

	reopened := New(path)
	got, ok, err := reopened.ChatGPTCatalog("g-p-project")
	if err != nil || !ok {
		t.Fatalf("got=%+v ok=%v err=%v", got, ok, err)
	}
	if !got.RefreshedAt.Equal(refreshedAt) || len(got.Conversations) != 2 {
		t.Fatalf("catalog did not round-trip: %+v", got)
	}
	if got.Conversations[0] != catalog.Conversations[0] || got.Conversations[1] != catalog.Conversations[1] {
		t.Fatalf("conversations did not round-trip: %+v", got.Conversations)
	}
}

func TestChatGPTCatalogUnknownProjectReportsNotFound(t *testing.T) {
	s := New(filepath.Join(t.TempDir(), "state.json"))
	got, ok, err := s.ChatGPTCatalog("g-p-never-saved")
	if err != nil || ok || len(got.Conversations) != 0 {
		t.Fatalf("got=%+v ok=%v err=%v", got, ok, err)
	}
}

// TestChatGPTCatalogMultipleProjectsStayIsolated fixes the DesignDoc's
// project-scoped cache identity: a catalog saved under one Project ID
// must never be visible, or overwritten, under another.
func TestChatGPTCatalogMultipleProjectsStayIsolated(t *testing.T) {
	s := New(filepath.Join(t.TempDir(), "state.json"))
	at := time.Now().UTC().Truncate(time.Second)
	p1 := ChatGPTCatalog{RefreshedAt: at, Conversations: []ChatGPTConversation{{ID: "p1-conv", Title: "P1", CreatedAt: at, UpdatedAt: at}}}
	p2 := ChatGPTCatalog{RefreshedAt: at, Conversations: []ChatGPTConversation{{ID: "p2-conv", Title: "P2", CreatedAt: at, UpdatedAt: at}}}
	if err := s.SaveChatGPTCatalog("g-p-one", p1); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveChatGPTCatalog("g-p-two", p2); err != nil {
		t.Fatal(err)
	}

	got1, ok, err := s.ChatGPTCatalog("g-p-one")
	if err != nil || !ok || len(got1.Conversations) != 1 || got1.Conversations[0].ID != "p1-conv" {
		t.Fatalf("project one leaked/lost isolation: got=%+v ok=%v err=%v", got1, ok, err)
	}
	got2, ok, err := s.ChatGPTCatalog("g-p-two")
	if err != nil || !ok || len(got2.Conversations) != 1 || got2.Conversations[0].ID != "p2-conv" {
		t.Fatalf("project two leaked/lost isolation: got=%+v ok=%v err=%v", got2, ok, err)
	}
}

// TestSaveChatGPTCatalogPreservesUnrelatedState fixes that writing a
// ChatGPT catalog goes through the same read-modify-write transaction as
// every other domain write (see (*Store).update), so it must never clobber
// pins or Claude overlay state written by a concurrent or
// prior operation.
func TestSaveChatGPTCatalogPreservesUnrelatedState(t *testing.T) {
	s := New(filepath.Join(t.TempDir(), "state.json"))
	if _, err := s.TogglePinned("claude:session"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetClaudeArchived("c1"); err != nil {
		t.Fatal(err)
	}

	at := time.Now().UTC().Truncate(time.Second)
	if err := s.SaveChatGPTCatalog("g-p-project", ChatGPTCatalog{RefreshedAt: at, Conversations: []ChatGPTConversation{{ID: "conv", Title: "T", CreatedAt: at, UpdatedAt: at}}}); err != nil {
		t.Fatal(err)
	}

	pins, err := s.ListPinned()
	if err != nil || !pins["claude:session"] {
		t.Fatalf("pin lost after saving ChatGPT catalog: %v err=%v", pins, err)
	}
	archived, _, err := s.ClaudeState()
	if err != nil || !archived["c1"] {
		t.Fatalf("claude archive lost after saving ChatGPT catalog: %v err=%v", archived, err)
	}
}

// TestOldStateFileWithoutChatGPTCatalogsDecodesNormally fixes schema
// compatibility: a state.json written before this field existed must
// continue to decode normally, with ChatGPTCatalog reporting "not found"
// rather than erroring.
func TestOldStateFileWithoutChatGPTCatalogsDecodesNormally(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	content := `{"pinned":{"claude:session":true},"runs":{"r1":{"id":"r1","provider":"codex","cwd":"/work","state":"running","startedAt":"2026-01-01T00:00:00Z"}}}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	s := New(path)

	got, ok, err := s.ChatGPTCatalog("g-p-project")
	if err != nil || ok || len(got.Conversations) != 0 {
		t.Fatalf("got=%+v ok=%v err=%v", got, ok, err)
	}
	pins, err := s.ListPinned()
	if err != nil || !pins["claude:session"] {
		t.Fatalf("pre-existing pin did not survive decoding an old-schema file: %v err=%v", pins, err)
	}
}

// TestEmptyChatGPTCatalogRoundTrips fixes that a COMPLETE enumeration
// which legitimately observed zero conversations is a meaningful,
// persistable value -- distinct from "never saved" (ok == false) -- and
// must round-trip as such, not be normalized away to "not found".
func TestEmptyChatGPTCatalogRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	at := time.Now().UTC().Truncate(time.Second)
	s := New(path)
	if err := s.SaveChatGPTCatalog("g-p-project", ChatGPTCatalog{RefreshedAt: at}); err != nil {
		t.Fatal(err)
	}

	reopened := New(path)
	got, ok, err := reopened.ChatGPTCatalog("g-p-project")
	if err != nil || !ok {
		t.Fatalf("got=%+v ok=%v err=%v", got, ok, err)
	}
	if len(got.Conversations) != 0 {
		t.Fatalf("expected an empty (not absent) catalog: %+v", got)
	}
	if !got.RefreshedAt.Equal(at) {
		t.Fatalf("RefreshedAt did not round-trip: got=%v want=%v", got.RefreshedAt, at)
	}
}

// TestSaveChatGPTCatalogReplacesPreviousCatalog fixes full-replacement
// semantics: saving again for the same Project ID must overwrite, not
// merge with, whatever was previously persisted for it.
func TestSaveChatGPTCatalogReplacesPreviousCatalog(t *testing.T) {
	s := New(filepath.Join(t.TempDir(), "state.json"))
	at := time.Now().UTC().Truncate(time.Second)
	if err := s.SaveChatGPTCatalog("g-p-project", ChatGPTCatalog{RefreshedAt: at, Conversations: []ChatGPTConversation{{ID: "old", Title: "Old", CreatedAt: at, UpdatedAt: at}}}); err != nil {
		t.Fatal(err)
	}
	later := at.Add(time.Hour)
	if err := s.SaveChatGPTCatalog("g-p-project", ChatGPTCatalog{RefreshedAt: later, Conversations: []ChatGPTConversation{{ID: "new", Title: "New", CreatedAt: later, UpdatedAt: later}}}); err != nil {
		t.Fatal(err)
	}

	got, ok, err := s.ChatGPTCatalog("g-p-project")
	if err != nil || !ok || len(got.Conversations) != 1 || got.Conversations[0].ID != "new" {
		t.Fatalf("expected the second save to fully replace the first: %+v ok=%v err=%v", got, ok, err)
	}
}
