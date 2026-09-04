package tui

import (
	"context"
	"errors"
	"testing"

	"github.com/tingtt/agentsctl/internal/session"
)

// TestActionAttachMarksLastAttachedOnAnySuccessfulReturn exercises
// App.act's ActionAttach path via the AttachFunc test seam (see
// app_unix.go), covering exactly what app_unix.go's act() actually
// decides on: Model.MarkAttached fires whenever attach() returns with no
// error, regardless of *how* the attachment ended (an explicit Ctrl+]
// detach and a natural attached-process exit both return nil from
// attachpty.AttachClaude/AttachCodex — see internal/pty's tests for that),
// and never fires when attach() itself failed.
func TestActionAttachMarksLastAttachedOnAnySuccessfulReturn(t *testing.T) {
	aKey := session.Key{Provider: session.ProviderClaude, ID: "a"}
	bKey := session.Key{Provider: session.ProviderCodex, ID: "b"}
	cKey := session.Key{Provider: session.ProviderClaude, ID: "c"}
	rows := []session.Session{
		{Key: aKey, Name: "A", Capabilities: session.Capabilities{Attach: true}},
		{Key: bKey, Name: "B", Capabilities: session.Capabilities{Attach: true}},
		{Key: cKey, Name: "C", Capabilities: session.Capabilities{Attach: true}},
	}
	claude := &journeyProvider{id: session.ProviderClaude, rows: rows}
	codex := &journeyProvider{id: session.ProviderCodex, rows: rows}
	catalog := session.Catalog{Providers: []session.Provider{claude, codex}}

	t.Run("explicit Ctrl+] return marks last attached", func(t *testing.T) {
		m := NewModel()
		m.Rows = rows
		m.Selected = 0
		app := App{Catalog: catalog, Model: m, CWD: "/work", AttachFunc: func(context.Context, session.Provider, session.Session) error {
			return nil // models AttachClaude/AttachCodex returning nil after an explicit Ctrl+] detach
		}}
		action := app.Model.Update("open")
		if err := app.act(context.Background(), action); err != nil {
			t.Fatal(err)
		}
		if !app.Model.HasLastAttached || app.Model.LastAttachedKey != aKey {
			t.Fatalf("explicit-detach-return attach did not mark A as last attached: has=%v key=%+v", app.Model.HasLastAttached, app.Model.LastAttachedKey)
		}
	})

	t.Run("natural exit also marks last attached", func(t *testing.T) {
		m := NewModel()
		m.Rows = rows
		m.Selected = 1 // "b"
		app := App{Catalog: catalog, Model: m, CWD: "/work", AttachFunc: func(context.Context, session.Provider, session.Session) error {
			return nil // models the attached process exiting naturally, which also returns nil
		}}
		action := app.Model.Update("open")
		if err := app.act(context.Background(), action); err != nil {
			t.Fatal(err)
		}
		if !app.Model.HasLastAttached || app.Model.LastAttachedKey != bKey {
			t.Fatalf("natural-exit attach did not mark B as last attached: has=%v key=%+v", app.Model.HasLastAttached, app.Model.LastAttachedKey)
		}
	})

	t.Run("attach error leaves last attached unchanged", func(t *testing.T) {
		m := NewModel()
		m.Rows = rows
		m.Selected = 0
		m.MarkAttached(aKey) // pre-existing state, from an earlier successful attach
		m.Selected = 2       // now select "c", whose attach will fail
		app := App{Catalog: catalog, Model: m, CWD: "/work", AttachFunc: func(context.Context, session.Provider, session.Session) error {
			return errors.New("attach client failed to start")
		}}
		action := app.Model.Update("open")
		if err := app.act(context.Background(), action); err == nil {
			t.Fatal("expected the attach error to propagate")
		}
		if app.Model.LastAttachedKey != aKey {
			t.Fatalf("a failed attach changed LastAttachedKey: got=%+v, want unchanged aKey=%+v", app.Model.LastAttachedKey, aKey)
		}
	})

	t.Run("next successful attach replaces the previous key", func(t *testing.T) {
		m := NewModel()
		m.Rows = rows
		m.Selected = 0
		app := App{Catalog: catalog, Model: m, CWD: "/work", AttachFunc: func(context.Context, session.Provider, session.Session) error {
			return nil
		}}
		if err := app.act(context.Background(), app.Model.Update("open")); err != nil { // attach A
			t.Fatal(err)
		}
		app.Model.Selected = 1 // now attach B
		if err := app.act(context.Background(), app.Model.Update("open")); err != nil {
			t.Fatal(err)
		}
		if app.Model.LastAttachedKey != bKey {
			t.Fatalf("LastAttachedKey=%+v, want only B (bKey=%+v) after A then B", app.Model.LastAttachedKey, bKey)
		}
	})
}
