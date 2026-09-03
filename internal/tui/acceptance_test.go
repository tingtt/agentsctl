package tui

import (
	"context"
	"testing"
	"time"

	"github.com/tingtt/agentsctl/internal/session"
)

type journeyProvider struct {
	id       session.ProviderID
	rows     []session.Session
	archived []session.Session
}

func (p *journeyProvider) ID() session.ProviderID { return p.id }
func (p *journeyProvider) Available() error       { return nil }
func (p *journeyProvider) List(_ context.Context, archived bool) ([]session.Session, error) {
	if archived {
		return append([]session.Session(nil), p.archived...), nil
	}
	return append([]session.Session(nil), p.rows...), nil
}
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
func (p *journeyProvider) Rename(context.Context, session.Key, string) error { return nil }

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
