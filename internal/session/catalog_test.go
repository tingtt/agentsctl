package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

type fakeProvider struct {
	id   ProviderID
	rows []Session
	err  error
}

func (f fakeProvider) ID() ProviderID                                { return f.id }
func (f fakeProvider) Available() error                              { return nil }
func (f fakeProvider) List(context.Context, bool) ([]Session, error) { return f.rows, f.err }
func (f fakeProvider) Dispatch(context.Context, string, string) (Session, error) {
	return Session{}, nil
}
func (f fakeProvider) Stop(context.Context, Key) error           { return nil }
func (f fakeProvider) Archive(context.Context, Key) error        { return nil }
func (f fakeProvider) Unarchive(context.Context, Key) error      { return nil }
func (f fakeProvider) Rename(context.Context, Key, string) error { return nil }

func TestCatalogKeepsHealthyProviderWhenPeerFails(t *testing.T) {
	now := time.Now()
	c := Catalog{Providers: []Provider{
		fakeProvider{id: ProviderClaude, err: errors.New("bad json")},
		fakeProvider{id: ProviderCodex, rows: []Session{{Key: Key{Provider: ProviderCodex, ID: "2"}, Activity: ActivityIdle, UpdatedAt: now}}},
	}}
	s := c.Load(context.Background(), Scope{Directory: ScopeAll})
	if len(s.Sessions) != 1 || s.Sessions[0].Key.Provider != ProviderCodex {
		t.Fatalf("sessions=%+v", s.Sessions)
	}
	if s.Warnings[ProviderClaude] == nil {
		t.Fatal("missing isolated provider warning")
	}
}

type memoryPins struct{ values map[string]bool }

func (p *memoryPins) ListPinned() (map[string]bool, error) {
	result := make(map[string]bool, len(p.values))
	for key, value := range p.values {
		result[key] = value
	}
	return result, nil
}

func (p *memoryPins) TogglePinned(key string) (bool, error) {
	p.values[key] = !p.values[key]
	return p.values[key], nil
}

func TestCatalogOrdersPinnedAndOtherByCreationTime(t *testing.T) {
	now := time.Now()
	pins := &memoryPins{values: map[string]bool{
		Key{Provider: ProviderCodex, ID: "pinned-old"}.String():  true,
		Key{Provider: ProviderClaude, ID: "pinned-new"}.String(): true,
	}}
	c := Catalog{Pins: pins, Providers: []Provider{fakeProvider{id: ProviderCodex, rows: []Session{
		{Key: Key{Provider: ProviderCodex, ID: "other-old"}, CreatedAt: now.Add(-4 * time.Hour), UpdatedAt: now, Activity: ActivityNeedsInput},
		{Key: Key{Provider: ProviderCodex, ID: "pinned-old"}, CreatedAt: now.Add(-3 * time.Hour), UpdatedAt: now.Add(3 * time.Hour), Activity: ActivityWorking},
		{Key: Key{Provider: ProviderClaude, ID: "pinned-new"}, CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-3 * time.Hour), Activity: ActivityCompleted},
		{Key: Key{Provider: ProviderClaude, ID: "other-new"}, CreatedAt: now.Add(-2 * time.Hour), UpdatedAt: now.Add(4 * time.Hour), Activity: ActivityFailed},
	}}}}
	rows := c.Load(context.Background(), Scope{Directory: ScopeAll}).Sessions
	want := []string{"pinned-new", "pinned-old", "other-new", "other-old"}
	for i := range want {
		if rows[i].Key.ID != want[i] {
			t.Fatalf("row %d=%s, want %s; rows=%+v", i, rows[i].Key.ID, want[i], rows)
		}
	}
	if !rows[0].Pinned || !rows[1].Pinned || rows[2].Pinned || rows[3].Pinned {
		t.Fatalf("pin grouping=%+v", rows)
	}
}

func TestCatalogOrderDoesNotChangeWithActivityOrUpdateTime(t *testing.T) {
	now := time.Now()
	provider := fakeProvider{id: ProviderCodex, rows: []Session{
		{Key: Key{Provider: ProviderCodex, ID: "new"}, CreatedAt: now},
		{Key: Key{Provider: ProviderCodex, ID: "old"}, CreatedAt: now.Add(-time.Hour)},
	}}
	c := Catalog{Providers: []Provider{provider}}
	before := c.Load(context.Background(), Scope{Directory: ScopeAll}).Sessions
	provider.rows[0].Activity, provider.rows[0].UpdatedAt = ActivityCompleted, now.Add(-24*time.Hour)
	provider.rows[1].Activity, provider.rows[1].UpdatedAt = ActivityNeedsInput, now.Add(24*time.Hour)
	c.Providers[0] = provider
	after := c.Load(context.Background(), Scope{Directory: ScopeAll}).Sessions
	if before[0].Key.ID != "new" || after[0].Key.ID != "new" {
		t.Fatalf("order changed: before=%+v after=%+v", before, after)
	}
}

func TestPinIdentityIncludesProvider(t *testing.T) {
	pins := &memoryPins{values: map[string]bool{}}
	c := Catalog{Pins: pins}
	claude := Key{Provider: ProviderClaude, ID: "same"}
	codex := Key{Provider: ProviderCodex, ID: "same"}
	if pinned, err := c.TogglePin(claude); err != nil || !pinned {
		t.Fatalf("toggle Claude: pinned=%v err=%v", pinned, err)
	}
	if pins.values[codex.String()] {
		t.Fatal("Codex session with the same native ID was pinned")
	}
}

func TestCatalogDirectoryScope(t *testing.T) {
	root := t.TempDir()
	current := filepath.Join(root, "project")
	if err := os.MkdirAll(filepath.Join(current, "child"), 0o700); err != nil {
		t.Fatal(err)
	}
	sibling := filepath.Join(root, "sibling")
	parent := root
	rows := []Session{
		{Key: Key{Provider: ProviderCodex, ID: "exact"}, CWD: current},
		{Key: Key{Provider: ProviderCodex, ID: "normalized"}, CWD: current + "/./"},
		{Key: Key{Provider: ProviderCodex, ID: "sibling"}, CWD: sibling},
		{Key: Key{Provider: ProviderCodex, ID: "child"}, CWD: filepath.Join(current, "child")},
		{Key: Key{Provider: ProviderCodex, ID: "parent"}, CWD: parent},
	}
	catalog := Catalog{Providers: []Provider{fakeProvider{id: ProviderCodex, rows: rows}}}

	// Default zero value (ScopeCWD): exact match only, cleaned-path comparison.
	got := catalog.Load(context.Background(), Scope{CurrentDirectory: current + "/."}).Sessions
	if ids := sessionIDs(got); len(ids) != 2 || ids[0] != "exact" || ids[1] != "normalized" {
		t.Fatalf("current-directory sessions=%v", ids)
	}
	all := catalog.Load(context.Background(), Scope{CurrentDirectory: current, Directory: ScopeAll}).Sessions
	if len(all) != len(rows) {
		t.Fatalf("all-directory sessions=%d, want %d", len(all), len(rows))
	}
	subtree := catalog.Load(context.Background(), Scope{CurrentDirectory: current, Directory: ScopeSubtree}).Sessions
	if ids := sessionIDs(subtree); len(ids) != 3 || ids[0] != "child" || ids[1] != "exact" || ids[2] != "normalized" {
		t.Fatalf("subtree sessions=%v, want [child exact normalized] (cwd itself + descendants, sibling/parent excluded)", ids)
	}
}

// TestCatalogSubtreeScopeDistinguishesSamePrefixSiblings guards the
// specific pitfall a strings.HasPrefix-based subtree check would fall
// into: "/foo/bar-other" starts with the string "/foo/bar" but is not a
// descendant of it. isWithinSubtree must use path-boundary-aware
// comparison (filepath.Rel), not a raw string prefix.
func TestCatalogSubtreeScopeDistinguishesSamePrefixSiblings(t *testing.T) {
	root := t.TempDir()
	bar := filepath.Join(root, "bar")
	barOther := filepath.Join(root, "bar-other")
	if err := os.MkdirAll(bar, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(barOther, 0o700); err != nil {
		t.Fatal(err)
	}
	rows := []Session{
		{Key: Key{Provider: ProviderCodex, ID: "bar"}, CWD: bar},
		{Key: Key{Provider: ProviderCodex, ID: "bar-other"}, CWD: barOther},
	}
	catalog := Catalog{Providers: []Provider{fakeProvider{id: ProviderCodex, rows: rows}}}
	got := catalog.Load(context.Background(), Scope{CurrentDirectory: bar, Directory: ScopeSubtree}).Sessions
	if ids := sessionIDs(got); len(ids) != 1 || ids[0] != "bar" {
		t.Fatalf("subtree scope leaked a same-prefix sibling: %v", ids)
	}
}

func TestCatalogDoesNotResolveSymlinks(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	link := filepath.Join(root, "link")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	catalog := Catalog{Providers: []Provider{fakeProvider{id: ProviderCodex, rows: []Session{
		{Key: Key{Provider: ProviderCodex, ID: "logical"}, CWD: link},
		{Key: Key{Provider: ProviderCodex, ID: "physical"}, CWD: real},
	}}}}
	got := catalog.Load(context.Background(), Scope{CurrentDirectory: link}).Sessions
	if ids := sessionIDs(got); len(ids) != 1 || ids[0] != "logical" {
		t.Fatalf("symlink scope resolved unexpectedly: %v", ids)
	}
}

func sessionIDs(rows []Session) []string {
	ids := make([]string, len(rows))
	for i := range rows {
		ids[i] = rows[i].Key.ID
	}
	sort.Strings(ids)
	return ids
}

func TestActiveSessionCannotArchive(t *testing.T) {
	s := Session{Activity: ActivityWorking, Capabilities: Capabilities{Archive: true, Stop: true}}
	c := CapabilitiesFor(s)
	if c.Archive || !c.Stop || c.Reason == "" {
		t.Fatalf("capabilities=%+v", c)
	}
}
