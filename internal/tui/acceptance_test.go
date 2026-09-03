package tui

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tingtt/agentsctl/internal/session"
)

type journeyProvider struct {
	id        session.ProviderID
	rows      []session.Session
	archived  []session.Session
	renames   []renameCall
	renameErr error
	// listDelay simulates a slow provider CLI subprocess in latency tests.
	listDelay time.Duration
	// listCalls counts List invocations so tests can assert a local-only
	// operation (pin/unpin) never triggers a provider refresh.
	listCalls int32
}

type renameCall struct {
	key  session.Key
	name string
}

func (p *journeyProvider) ID() session.ProviderID { return p.id }
func (p *journeyProvider) Available() error       { return nil }
func (p *journeyProvider) List(_ context.Context, archived bool) ([]session.Session, error) {
	atomic.AddInt32(&p.listCalls, 1)
	if p.listDelay > 0 {
		time.Sleep(p.listDelay)
	}
	if archived {
		return append([]session.Session(nil), p.archived...), nil
	}
	return append([]session.Session(nil), p.rows...), nil
}

func (p *journeyProvider) ListCallCount() int { return int(atomic.LoadInt32(&p.listCalls)) }
func (p *journeyProvider) Dispatch(_ context.Context, prompt, cwd string) (session.Session, error) {
	id := string(p.id) + "-new"
	r := session.Session{Key: session.Key{Provider: p.id, ID: id}, Summary: prompt, CWD: cwd, UpdatedAt: time.Now(), Activity: session.ActivityWorking, Runtime: session.RuntimeDetached, Capabilities: session.Capabilities{Attach: true, Stop: true}}
	p.rows = append(p.rows, r)
	return r, nil
}
func (p *journeyProvider) Stop(_ context.Context, k session.Key) error {
	for i := range p.rows {
		if p.rows[i].Key == k {
			p.rows[i].Activity = session.ActivityCompleted
			p.rows[i].Runtime = session.RuntimeStopped
			p.rows[i].Capabilities = session.Capabilities{Archive: true}
		}
	}
	return nil
}
func (p *journeyProvider) Archive(_ context.Context, k session.Key) error {
	for i, r := range p.rows {
		if r.Key == k {
			r.Archived = true
			r.Capabilities = session.Capabilities{Unarchive: true}
			p.archived = append(p.archived, r)
			p.rows = append(p.rows[:i], p.rows[i+1:]...)
			break
		}
	}
	return nil
}
func (p *journeyProvider) Unarchive(_ context.Context, k session.Key) error {
	for i, r := range p.archived {
		if r.Key == k {
			r.Archived = false
			r.Capabilities = session.Capabilities{Archive: true}
			p.rows = append(p.rows, r)
			p.archived = append(p.archived[:i], p.archived[i+1:]...)
			break
		}
	}
	return nil
}
func (p *journeyProvider) Rename(_ context.Context, key session.Key, name string) error {
	p.renames = append(p.renames, renameCall{key: key, name: name})
	if p.renameErr != nil {
		return p.renameErr
	}
	for i := range p.rows {
		if p.rows[i].Key == key {
			p.rows[i].Name = name
		}
	}
	return nil
}

func TestRenameActionSuccessAndFailure(t *testing.T) {
	target := session.Key{Provider: session.ProviderCodex, ID: "target"}
	other := session.Key{Provider: session.ProviderCodex, ID: "other"}
	for _, tc := range []struct {
		name      string
		renameErr error
		wantOpen  bool
	}{
		{name: "success"},
		{name: "failure preserves editor", renameErr: errors.New("rename rejected"), wantOpen: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := &journeyProvider{id: session.ProviderCodex, renameErr: tc.renameErr, rows: []session.Session{
				{Key: other, Name: "other", CWD: "/work", Activity: session.ActivityIdle, Capabilities: session.Capabilities{Rename: true}},
				{Key: target, Name: "old", CWD: "/work", Activity: session.ActivityIdle, Capabilities: session.Capabilities{Rename: true}},
			}}
			m := NewModel()
			m.Prompt, m.Stash = "composer", "stash"
			m.Rows = append([]session.Session(nil), provider.rows...)
			m.Selected = 1
			m.Update("rename")
			m.RenameDraft = "new"
			action := m.Update("enter")
			app := App{Catalog: session.Catalog{Providers: []session.Provider{provider}}, Model: m, CWD: "/work"}
			err := app.act(context.Background(), action)
			if (err != nil) != (tc.renameErr != nil) {
				t.Fatalf("err=%v", err)
			}
			if err != nil {
				app.Model.Status = "error: " + err.Error()
			}
			provider.rows[0], provider.rows[1] = provider.rows[1], provider.rows[0]
			app.refresh(context.Background())
			if len(provider.renames) != 1 || provider.renames[0] != (renameCall{key: target, name: "new"}) {
				t.Fatalf("rename calls=%+v", provider.renames)
			}
			if app.Model.Renaming != tc.wantOpen || app.Model.Prompt != "composer" || app.Model.Stash != "stash" {
				t.Fatalf("model=%+v", app.Model)
			}
			if tc.renameErr != nil {
				if app.Model.Rows[app.Model.Selected].Key != target || app.Model.Rows[app.Model.Selected].Name != "old" || app.Model.RenameDraft != "new" || app.Model.Status != "error: rename rejected" {
					t.Fatalf("failed rename mutated catalog/editor: %+v", app.Model)
				}
				return
			}
			if app.Model.Rows[app.Model.Selected].Key != target || app.Model.Rows[app.Model.Selected].Name != "new" {
				t.Fatalf("selection/name after refresh: %+v", app.Model)
			}
		})
	}
}

func TestMVPJourneyAcrossProviders(t *testing.T) {
	ctx := context.Background()
	claude := &journeyProvider{id: session.ProviderClaude, rows: []session.Session{{Key: session.Key{Provider: session.ProviderClaude, ID: "c0"}, Activity: session.ActivityIdle}}}
	codex := &journeyProvider{id: session.ProviderCodex, rows: []session.Session{{Key: session.Key{Provider: session.ProviderCodex, ID: "x0"}, Activity: session.ActivityIdle}}}
	catalog := session.Catalog{Providers: []session.Provider{claude, codex}}
	m := NewModel()
	scope := session.Scope{CurrentDirectory: "/work", AllDirectories: true}
	m.SetRows(catalog.Load(ctx, scope).Sessions)
	if len(m.Rows) != 2 {
		t.Fatal("mixed catalog missing")
	}
	m.Prompt = "codex prompt"
	m.Update("shift+tab")
	a := m.Update("enter")
	if a.Provider != session.ProviderCodex {
		t.Fatal("provider toggle failed")
	}
	created, err := codex.Dispatch(ctx, a.Prompt, "/work")
	if err != nil {
		t.Fatal(err)
	}
	created.Runtime = session.RuntimeAttached
	created.Runtime = session.RuntimeDetached // Ctrl+] returns to overview without stopping.
	if created.Activity != session.ActivityWorking {
		t.Fatal("detach stopped child")
	}
	m.Prompt = "claude prompt"
	m.Update("shift+tab")
	a = m.Update("enter")
	createdClaude, _ := claude.Dispatch(ctx, a.Prompt, "/work")
	createdClaude.Runtime = session.RuntimeAttached
	createdClaude.Runtime = session.RuntimeDetached
	if len(catalog.Load(ctx, scope).Sessions) != 4 {
		t.Fatal("sessions did not survive overview refresh")
	}
	if err := codex.Stop(ctx, created.Key); err != nil {
		t.Fatal(err)
	}
	if err := codex.Archive(ctx, created.Key); err != nil {
		t.Fatal(err)
	}
	if len(catalog.Load(ctx, scope).Sessions) != 3 {
		t.Fatal("archived row remained in the normal catalog")
	}
}
