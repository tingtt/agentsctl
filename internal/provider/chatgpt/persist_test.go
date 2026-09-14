package chatgpt

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/tingtt/agentsctl/internal/localstate"
)

// fakeCatalogStore is an in-memory CatalogStore double: production wiring
// uses *localstate.Store (see internal/localstate/chatgpt.go and its own
// dedicated tests), but Provider's own tests need to control load/save
// outcomes (including deliberate failures) without touching disk.
type fakeCatalogStore struct {
	mu       sync.Mutex
	catalogs map[string]localstate.ChatGPTCatalog
	loadErr  error
	saveErr  error
	saveN    int
}

func (f *fakeCatalogStore) ChatGPTCatalog(projectID string) (localstate.ChatGPTCatalog, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.loadErr != nil {
		return localstate.ChatGPTCatalog{}, false, f.loadErr
	}
	c, ok := f.catalogs[projectID]
	return c, ok, nil
}

func (f *fakeCatalogStore) SaveChatGPTCatalog(projectID string, catalog localstate.ChatGPTCatalog) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.saveN++
	if f.saveErr != nil {
		return f.saveErr
	}
	if f.catalogs == nil {
		f.catalogs = map[string]localstate.ChatGPTCatalog{}
	}
	f.catalogs[projectID] = catalog
	return nil
}

// TestNewHydratesFromPersistedCatalogWithoutBrowserOrNetwork fixes the
// core startup acceptance criterion: New synchronously hydrates the cache
// from CatalogStore before it ever returns, so the very first List (e.g.
// from Agent View's initial LoadStream call) already has rows to serve --
// entirely from local state, never a real browser process or network.
func TestNewHydratesFromPersistedCatalogWithoutBrowserOrNetwork(t *testing.T) {
	at := time.Now().UTC().Truncate(time.Second)
	store := &fakeCatalogStore{catalogs: map[string]localstate.ChatGPTCatalog{
		"g-p-project": {RefreshedAt: at, Conversations: []localstate.ChatGPTConversation{
			{ID: conversationA, Title: "A", CreatedAt: at, UpdatedAt: at},
		}},
	}}
	p := New(Config{ProjectID: "g-p-project", Root: "/work"}, store)
	defer p.Close()

	rows, err := p.List(context.Background(), false)
	if err != nil || len(rows) != 1 || rows[0].Key.ID != conversationA || rows[0].CWD != "/work" {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
}

func TestNewWithNoPersistedCatalogHydratesEmpty(t *testing.T) {
	p := New(Config{ProjectID: "g-p-project", Root: "/work"}, &fakeCatalogStore{})
	defer p.Close()
	rows, err := p.List(context.Background(), false)
	if err != nil || len(rows) != 0 {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
}

// TestHydrationIsolatedByProjectID fixes project-scoped cache identity at
// the provider boundary: a persisted catalog under a different Project ID
// must never hydrate this Provider's cache.
func TestHydrationIsolatedByProjectID(t *testing.T) {
	at := time.Now().UTC().Truncate(time.Second)
	store := &fakeCatalogStore{catalogs: map[string]localstate.ChatGPTCatalog{
		"g-p-one": {RefreshedAt: at, Conversations: []localstate.ChatGPTConversation{{ID: conversationA, Title: "A", CreatedAt: at, UpdatedAt: at}}},
	}}
	p := New(Config{ProjectID: "g-p-two", Root: "/work"}, store)
	defer p.Close()
	rows, err := p.List(context.Background(), false)
	if err != nil || len(rows) != 0 {
		t.Fatalf("a different Project's persisted catalog must not leak in: rows=%+v err=%v", rows, err)
	}
}

// TestHydrationUsesCurrentConfigRootNotAnyPersistedCWD fixes that CWD is
// rehydrated from the CURRENT Config.Root, never a persisted value (there
// is none -- see localstate.ChatGPTConversation's doc comment): the same
// persisted conversation hydrated under two different Config.Root values
// (a moved checkout, or a second local clone of the same Project) gets
// each Provider's own current CWD.
func TestHydrationUsesCurrentConfigRootNotAnyPersistedCWD(t *testing.T) {
	at := time.Now().UTC().Truncate(time.Second)
	store := &fakeCatalogStore{catalogs: map[string]localstate.ChatGPTCatalog{
		"g-p-project": {RefreshedAt: at, Conversations: []localstate.ChatGPTConversation{{ID: conversationA, Title: "A", CreatedAt: at, UpdatedAt: at}}},
	}}

	p1 := New(Config{ProjectID: "g-p-project", Root: "/repo/one"}, store)
	defer p1.Close()
	rows1, err := p1.List(context.Background(), false)
	if err != nil || len(rows1) != 1 || rows1[0].CWD != "/repo/one" {
		t.Fatalf("rows1=%+v err=%v", rows1, err)
	}

	p2 := New(Config{ProjectID: "g-p-project", Root: "/repo/two"}, store)
	defer p2.Close()
	rows2, err := p2.List(context.Background(), false)
	if err != nil || len(rows2) != 1 || rows2[0].CWD != "/repo/two" {
		t.Fatalf("rows2=%+v err=%v", rows2, err)
	}
}

// TestHydrationRejectsMalformedPersistedConversation fixes data
// validation on load: local persisted state may be stale or corrupt, and
// a single invalid row must fail the whole catalog load rather than
// admitting a partially-corrupt catalog (e.g. an unparseable conversation
// ID reaching a session.Key).
func TestHydrationRejectsMalformedPersistedConversation(t *testing.T) {
	now := time.Now()
	store := &fakeCatalogStore{catalogs: map[string]localstate.ChatGPTCatalog{
		"g-p-project": {RefreshedAt: now, Conversations: []localstate.ChatGPTConversation{{ID: "not-a-valid-conversation-id", Title: "A", CreatedAt: now, UpdatedAt: now}}},
	}}
	p := New(Config{ProjectID: "g-p-project", Root: "/work"}, store)
	defer p.Close()
	rows, err := p.List(context.Background(), false)
	if err != nil || len(rows) != 0 {
		t.Fatalf("a malformed persisted row must be rejected wholesale, not partially admitted: rows=%+v err=%v", rows, err)
	}
}

// TestHydrationLoadErrorDoesNotPreventProviderFromWorking fixes that a
// CatalogStore read/decode error is diagnostic-only: Provider must still
// construct successfully and behave exactly as if nothing had ever been
// persisted, not fail List or panic.
func TestHydrationLoadErrorDoesNotPreventProviderFromWorking(t *testing.T) {
	p := New(Config{ProjectID: "g-p-project", Root: "/work"}, &fakeCatalogStore{loadErr: errors.New("disk read failed")})
	defer p.Close()
	rows, err := p.List(context.Background(), false)
	if err != nil || len(rows) != 0 {
		t.Fatalf("a persisted-catalog load error must not surface as a remote List failure: rows=%+v err=%v", rows, err)
	}
}

// TestOpenSucceedsFromHydratedCacheBeforeAnyRefresh is the restart
// regression test the DesignDoc calls for: a session hydrated purely from
// CatalogStore (no refresh has run yet) must be immediately openable.
func TestOpenSucceedsFromHydratedCacheBeforeAnyRefresh(t *testing.T) {
	at := time.Now().UTC().Truncate(time.Second)
	store := &fakeCatalogStore{catalogs: map[string]localstate.ChatGPTCatalog{
		"g-p-project": {RefreshedAt: at, Conversations: []localstate.ChatGPTConversation{{ID: conversationA, Title: "A", CreatedAt: at, UpdatedAt: at}}},
	}}
	browser := &fakeBrowser{}
	p := &Provider{config: Config{ProjectID: "g-p-project", Root: "/work"}, browser: browser, store: store}
	p.hydrate()

	rows, err := p.List(context.Background(), false)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
	if err := p.Open(context.Background(), rows[0], nil, io.Discard); err != nil {
		t.Fatalf("Open from a hydrated (pre-refresh) cache failed: %v", err)
	}
	if browser.openedID != conversationA {
		t.Fatalf("opened=%q", browser.openedID)
	}
}

// TestSuccessfulRefreshPersistsCatalog fixes the write half of
// persistence: a COMPLETE enumeration must be saved through CatalogStore,
// not just the in-memory cache.
func TestSuccessfulRefreshPersistsCatalog(t *testing.T) {
	at := time.Now()
	browser := &sequenceBrowser{results: []browserResult{{conversations: []conversation{conv(conversationA, "A", at), conv(conversationB, "B", at)}}}}
	store := &fakeCatalogStore{}
	p := &Provider{config: Config{ProjectID: "g-p-project", Root: "/work"}, browser: browser, store: store}
	updates := p.Observe(context.Background())
	p.Refresh(context.Background())
	waitForUpdate(t, updates)

	persisted, ok, err := store.ChatGPTCatalog("g-p-project")
	if err != nil || !ok || len(persisted.Conversations) != 2 {
		t.Fatalf("persisted=%+v ok=%v err=%v", persisted, ok, err)
	}
}

// TestFailedRefreshDoesNotTouchPersistedCatalog mirrors the memory-cache
// failure guarantee for the persisted catalog: a refresh failure after a
// prior successful persist must leave the persisted catalog untouched.
func TestFailedRefreshDoesNotTouchPersistedCatalog(t *testing.T) {
	at := time.Now()
	browser := &sequenceBrowser{results: []browserResult{
		{conversations: []conversation{conv(conversationA, "A", at)}},
		{err: errors.New("timeout")},
	}}
	store := &fakeCatalogStore{}
	p := &Provider{config: Config{ProjectID: "g-p-project", Root: "/work"}, browser: browser, store: store}
	updates := p.Observe(context.Background())

	p.Refresh(context.Background())
	waitForUpdate(t, updates)
	first, ok, err := store.ChatGPTCatalog("g-p-project")
	if err != nil || !ok || len(first.Conversations) != 1 {
		t.Fatalf("first persisted=%+v ok=%v err=%v", first, ok, err)
	}

	p.Refresh(context.Background())
	second := waitForUpdate(t, updates)
	if second.Err == nil {
		t.Fatal("expected the second refresh's failure to be published")
	}
	after, ok, err := store.ChatGPTCatalog("g-p-project")
	if err != nil || !ok || len(after.Conversations) != 1 || after.Conversations[0].ID != first.Conversations[0].ID {
		t.Fatalf("a failed refresh must not touch the persisted catalog: before=%+v after=%+v", first, after)
	}
}

// TestCompleteEmptyProjectPersistsEmptyCatalog mirrors the memory-cache
// guarantee: a COMPLETE enumeration that legitimately observes zero
// conversations must persist as an empty (not absent, not skipped)
// catalog, overwriting whatever non-empty catalog was persisted before.
func TestCompleteEmptyProjectPersistsEmptyCatalog(t *testing.T) {
	at := time.Now()
	browser := &sequenceBrowser{results: []browserResult{
		{conversations: []conversation{conv(conversationA, "A", at)}},
		{conversations: []conversation{}},
	}}
	store := &fakeCatalogStore{}
	p := &Provider{config: Config{ProjectID: "g-p-project", Root: "/work"}, browser: browser, store: store}
	updates := p.Observe(context.Background())
	p.Refresh(context.Background())
	waitForUpdate(t, updates)
	p.Refresh(context.Background())
	waitForUpdate(t, updates)

	persisted, ok, err := store.ChatGPTCatalog("g-p-project")
	if err != nil || !ok || len(persisted.Conversations) != 0 {
		t.Fatalf("a COMPLETE empty enumeration must persist an empty catalog: %+v ok=%v err=%v", persisted, ok, err)
	}
}

// TestPersistenceWriteFailureDoesNotDiscardFreshMemoryCache is the
// central persistence-failure guarantee (an explicit DesignDoc stop
// condition): a durability failure must never cause a valid, just-
// completed remote result to be discarded from memory or withheld from
// Observer subscribers.
func TestPersistenceWriteFailureDoesNotDiscardFreshMemoryCache(t *testing.T) {
	at := time.Now()
	browser := &sequenceBrowser{results: []browserResult{{conversations: []conversation{conv(conversationB, "B", at)}}}}
	store := &fakeCatalogStore{saveErr: errors.New("disk full")}
	p := &Provider{config: Config{ProjectID: "g-p-project", Root: "/work"}, browser: browser, store: store}
	updates := p.Observe(context.Background())

	p.Refresh(context.Background())
	upd := waitForUpdate(t, updates)
	if upd.Err != nil || len(upd.Sessions) != 1 || upd.Sessions[0].Key.ID != conversationB {
		t.Fatalf("a persistence write failure must not suppress the Observer publication of a valid remote result: %+v", upd)
	}

	rows, err := p.List(context.Background(), false)
	if err != nil || len(rows) != 1 || rows[0].Key.ID != conversationB {
		t.Fatalf("a persistence write failure must not discard the fresh memory cache: rows=%+v err=%v", rows, err)
	}
	if store.saveN != 1 {
		t.Fatalf("expected exactly one (failed) save attempt, got %d", store.saveN)
	}
}

// TestNilCatalogStoreBehavesAsIfPersistenceIsAbsent fixes that
// persistence is optional: a Provider with no store at all must hydrate
// to empty and still refresh/publish normally, purely in memory.
func TestNilCatalogStoreBehavesAsIfPersistenceIsAbsent(t *testing.T) {
	at := time.Now()
	browser := &sequenceBrowser{results: []browserResult{{conversations: []conversation{conv(conversationA, "A", at)}}}}
	p := New(Config{ProjectID: "g-p-project", Root: "/work"}, nil)
	defer p.Close()
	p.browser = browser

	updates := p.Observe(context.Background())
	p.Refresh(context.Background())
	upd := waitForUpdate(t, updates)
	if upd.Err != nil || len(upd.Sessions) != 1 {
		t.Fatalf("update=%+v", upd)
	}
}
