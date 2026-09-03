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
	s := c.Load(context.Background(), Scope{AllDirectories: true})
	if len(s.Sessions) != 1 || s.Sessions[0].Key.Provider != ProviderCodex {
		t.Fatalf("sessions=%+v", s.Sessions)
	}
	if s.Warnings[ProviderClaude] == nil {
		t.Fatal("missing isolated provider warning")
	}
}

func TestCatalogOrdersByActivityThenRecency(t *testing.T) {
	now := time.Now()
	c := Catalog{Providers: []Provider{fakeProvider{id: ProviderCodex, rows: []Session{
		{Key: Key{Provider: ProviderCodex, ID: "idle"}, Activity: ActivityIdle, UpdatedAt: now},
		{Key: Key{Provider: ProviderCodex, ID: "need"}, Activity: ActivityNeedsInput, UpdatedAt: now.Add(-time.Hour)},
	}}}}
	rows := c.Load(context.Background(), Scope{AllDirectories: true}).Sessions
	if rows[0].Key.ID != "need" {
		t.Fatalf("first=%s", rows[0].Key.ID)
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

	got := catalog.Load(context.Background(), Scope{CurrentDirectory: current + "/."}).Sessions
	if ids := sessionIDs(got); len(ids) != 2 || ids[0] != "exact" || ids[1] != "normalized" {
		t.Fatalf("current-directory sessions=%v", ids)
	}
	all := catalog.Load(context.Background(), Scope{CurrentDirectory: current, AllDirectories: true}).Sessions
	if len(all) != len(rows) {
		t.Fatalf("all-directory sessions=%d, want %d", len(all), len(rows))
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
