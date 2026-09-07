package sessionctl

import (
	"context"
	"testing"
	"time"

	"github.com/tingtt/agentsctl/internal/session"
)

func TestLoadKeepsHealthyProviderWhenPeerFails(t *testing.T) {
	now := time.Now()
	c := Controller{Providers: []Source{
		fakeSource{id: session.ProviderClaude, err: errBoom},
		fakeSource{id: session.ProviderCodex, rows: []session.Session{{Key: session.Key{Provider: session.ProviderCodex, ID: "2"}, Activity: session.ActivityIdle, UpdatedAt: now}}},
	}}
	got := c.Load(context.Background(), session.Scope{Directory: session.ScopeAll})
	if len(got.Sessions) != 1 || got.Sessions[0].Key.Provider != session.ProviderCodex {
		t.Fatalf("sessions=%+v", got.Sessions)
	}
	if got.Warnings[session.ProviderClaude] == nil {
		t.Fatal("missing isolated provider warning")
	}
}

func TestLoadMergesMultipleProviders(t *testing.T) {
	c := Controller{Providers: []Source{
		fakeSource{id: session.ProviderClaude, rows: []session.Session{{Key: session.Key{Provider: session.ProviderClaude, ID: "a"}, CreatedAt: time.Now()}}},
		fakeSource{id: session.ProviderCodex, rows: []session.Session{{Key: session.Key{Provider: session.ProviderCodex, ID: "b"}, CreatedAt: time.Now()}}},
	}}
	got := c.Load(context.Background(), session.Scope{Directory: session.ScopeAll})
	if len(got.Sessions) != 2 {
		t.Fatalf("want both providers merged, got %+v", got.Sessions)
	}
}

func TestLoadDeniesUnsupportedActionForPartialProvider(t *testing.T) {
	// Architecture acceptance A: a fake provider implementing only
	// Source+Opener (the initial ChatGPT integration shape) must have
	// Stop/Rename/Archive fail closed even if it mistakenly reports them
	// available, without Controller ever switching on ProviderID.
	p := &fakeOpenerSource{fakeSource: fakeSource{id: "chatgpt", rows: []session.Session{{
		Key:     session.Key{Provider: "chatgpt", ID: "1"},
		Actions: session.Actions{session.ActionOpen: {Available: true}, session.ActionStop: {Available: true}},
	}}}}
	c := Controller{Providers: []Source{p}}
	got := c.Load(context.Background(), session.Scope{Directory: session.ScopeAll})
	if len(got.Sessions) != 1 {
		t.Fatalf("sessions=%+v", got.Sessions)
	}
	row := got.Sessions[0]
	if !row.Actions.Available(session.ActionOpen) {
		t.Fatal("Open must remain available: the provider implements Opener")
	}
	if row.Actions.Available(session.ActionStop) {
		t.Fatal("Stop must fail closed: the provider does not implement Stopper, regardless of what it reported")
	}
	if row.Actions.Available(session.ActionRename) || row.Actions.Available(session.ActionArchive) {
		t.Fatalf("unimplemented capabilities must fail closed: %+v", row.Actions)
	}
}

func TestLoadEnrichesPinnedState(t *testing.T) {
	c := Controller{
		Providers: []Source{fakeSource{id: session.ProviderClaude, rows: []session.Session{{Key: session.Key{Provider: session.ProviderClaude, ID: "a"}}}}},
		Pins:      &fakePinStore{pinned: map[string]bool{"claude:a": true}},
	}
	got := c.Load(context.Background(), session.Scope{Directory: session.ScopeAll})
	if len(got.Sessions) != 1 || !got.Sessions[0].Pinned {
		t.Fatalf("want pinned session, got %+v", got.Sessions)
	}
}

func TestLoadAppliesScopeAndOrder(t *testing.T) {
	now := time.Now()
	c := Controller{
		Providers: []Source{fakeSource{id: session.ProviderClaude, rows: []session.Session{
			{Key: session.Key{Provider: session.ProviderClaude, ID: "in"}, CWD: "/proj", CreatedAt: now.Add(-time.Minute)},
			{Key: session.Key{Provider: session.ProviderClaude, ID: "out"}, CWD: "/other", CreatedAt: now},
			{Key: session.Key{Provider: session.ProviderClaude, ID: "pinned"}, CWD: "/proj", CreatedAt: now.Add(-time.Hour)},
		}}},
		Pins: &fakePinStore{pinned: map[string]bool{"claude:pinned": true}},
	}
	got := c.Load(context.Background(), session.Scope{CurrentDirectory: "/proj", Directory: session.ScopeSame})
	if len(got.Sessions) != 2 {
		t.Fatalf("scope must exclude /other: %+v", got.Sessions)
	}
	if got.Sessions[0].Key.ID != "pinned" {
		t.Fatalf("pinned session must sort first: %+v", got.Sessions)
	}
}
