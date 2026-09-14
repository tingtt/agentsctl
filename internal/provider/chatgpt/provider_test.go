package chatgpt

import (
	"context"
	"io"
	"os"
	"testing"
	"time"

	"github.com/tingtt/agentsctl/internal/session"
	"github.com/tingtt/agentsctl/internal/sessionctl"
)

type fakeBrowser struct {
	conversations []conversation
	projectID     string
	openedID      string
	closed        bool
}

func (f *fakeBrowser) List(_ context.Context, projectID string) ([]conversation, error) {
	f.projectID = projectID
	return f.conversations, nil
}
func (f *fakeBrowser) Open(_ context.Context, conversationID string, _ *os.File, _ io.Writer) error {
	f.openedID = conversationID
	return nil
}
func (f *fakeBrowser) Close() error { f.closed = true; return nil }

func TestProviderNormalizesProjectConversationForAgentView(t *testing.T) {
	created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	updated := created.Add(time.Hour)
	browser := &fakeBrowser{conversations: []conversation{{ID: conversationA, Title: "Conversation", CreatedAt: created, UpdatedAt: updated}}}
	provider := &Provider{config: Config{ProjectID: "g-p-project", Root: "/work/project"}, browser: browser}

	// List serves the cache, which is empty before any refresh has
	// completed -- it also lazily starts the first background refresh
	// (see Provider.List's doc comment), whose result this test waits for
	// on Observe rather than List's own (immediate, empty) return value.
	rows, err := provider.List(context.Background(), false)
	if err != nil || len(rows) != 0 {
		t.Fatalf("first List before any refresh: rows=%+v err=%v, want empty successful snapshot", rows, err)
	}
	updates := provider.Observe(context.Background())
	upd := waitForUpdate(t, updates)
	if upd.Err != nil || len(upd.Sessions) != 1 {
		t.Fatalf("observed update=%+v", upd)
	}
	if browser.projectID != "g-p-project" {
		t.Fatalf("projectID=%q", browser.projectID)
	}
	rows, err = provider.List(context.Background(), false)
	if err != nil || len(rows) != 1 {
		t.Fatalf("List after refresh: rows=%+v err=%v", rows, err)
	}
	row := rows[0]
	if browser.projectID != "g-p-project" || row.Key.String() != "chatgpt:"+conversationA {
		t.Fatalf("projectID=%q row=%+v", browser.projectID, row)
	}
	if row.Name != "Conversation" || row.CWD != "/work/project" || !row.CreatedAt.Equal(created) || !row.UpdatedAt.Equal(updated) {
		t.Fatalf("metadata=%+v", row)
	}
	if row.Activity != session.ActivityUnknown || row.Runtime != session.RuntimeNone || row.Archived || row.Pinned {
		t.Fatalf("provider-neutral state=%+v", row)
	}
	if !row.Actions.Available(session.ActionOpen) || row.Actions.Available(session.ActionStop) || row.Actions.Available(session.ActionRename) || row.Actions.Available(session.ActionArchive) {
		t.Fatalf("actions=%+v", row.Actions)
	}
}

func TestProviderExposesOnlySourceAndOpenCapabilities(t *testing.T) {
	provider := any(&Provider{})
	if _, ok := provider.(sessionctl.Source); !ok {
		t.Fatal("Provider must implement Source")
	}
	if _, ok := provider.(sessionctl.Opener); !ok {
		t.Fatal("Provider must implement Opener")
	}
	if _, ok := provider.(sessionctl.Observer); !ok {
		t.Fatal("Provider must implement Observer")
	}
	if _, ok := provider.(sessionctl.Refresher); !ok {
		t.Fatal("Provider must implement Refresher")
	}
	if _, ok := provider.(sessionctl.Dispatcher); ok {
		t.Fatal("Provider must not implement Dispatcher")
	}
	if _, ok := provider.(sessionctl.Stopper); ok {
		t.Fatal("Provider must not implement Stopper")
	}
	if _, ok := provider.(sessionctl.Renamer); ok {
		t.Fatal("Provider must not implement Renamer")
	}
	if _, ok := provider.(sessionctl.Archiver); ok {
		t.Fatal("Provider must not implement Archiver")
	}
}

func TestProviderOpenDelegatesConversationWithoutLifecycleMutation(t *testing.T) {
	browser := &fakeBrowser{}
	provider := &Provider{browser: browser}
	row := session.Session{Key: session.Key{Provider: session.ProviderChatGPT, ID: conversationA}}
	if err := provider.Open(context.Background(), row, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	if browser.openedID != conversationA {
		t.Fatalf("opened conversation=%q", browser.openedID)
	}
}

func TestUnavailableProviderReportsConfigErrorWithoutBrowser(t *testing.T) {
	provider := NewUnavailable(os.ErrInvalid)
	if _, err := provider.List(context.Background(), false); err == nil {
		t.Fatal("invalid explicit configuration must surface as a provider error")
	}
}

// waitForUpdate blocks until updates delivers one sessionctl.ProviderUpdate
// or fails the test after a bounded timeout -- test-only convenience for
// asserting on an Observe subscription's next publication without a real
// background browser.
func waitForUpdate(t *testing.T, updates <-chan sessionctl.ProviderUpdate) sessionctl.ProviderUpdate {
	t.Helper()
	select {
	case upd, ok := <-updates:
		if !ok {
			t.Fatal("Observe channel closed before publishing an update")
		}
		return upd
	case <-time.After(2 * time.Second):
		t.Fatal("no ProviderUpdate published within 2s")
		return sessionctl.ProviderUpdate{}
	}
}
