package tui

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/tingtt/agentsctl/internal/session"
	"github.com/tingtt/agentsctl/internal/state"
)

// erroringPinStore always fails TogglePinned, for testing that a
// persistence failure never leaves the Model's rows inconsistent with
// what was (not) actually persisted.
type erroringPinStore struct{ err error }

func (e *erroringPinStore) ListPinned() (map[string]bool, error) { return map[string]bool{}, nil }
func (e *erroringPinStore) TogglePinned(string) (bool, error)    { return false, e.err }

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

func TestPinDoesNotInvokeProviderList(t *testing.T) {
	store := state.New(filepath.Join(t.TempDir(), "state.json"))
	claude := &journeyProvider{id: session.ProviderClaude}
	codex := &journeyProvider{id: session.ProviderCodex}
	catalog := session.Catalog{Providers: []session.Provider{claude, codex}, Pins: store}
	key := session.Key{Provider: session.ProviderClaude, ID: "c"}
	m := NewModel()
	m.Rows = []session.Session{{Key: key, Name: "s", CreatedAt: time.Now()}}
	m.Selected = 0
	app := App{Catalog: catalog, Model: m, CWD: "/work"}

	action := app.Model.Update("pin")
	if action.Kind != ActionPin {
		t.Fatalf("action=%+v", action)
	}
	if err := app.act(context.Background(), action); err != nil {
		t.Fatal(err)
	}
	if claude.ListCallCount() != 0 || codex.ListCallCount() != 0 {
		t.Fatalf("pin invoked provider List: claude=%d codex=%d", claude.ListCallCount(), codex.ListCallCount())
	}
	if !app.Model.Rows[0].Pinned {
		t.Fatal("pin was not applied to the model row")
	}
	// Pin/unpin must not produce any composer-top notification — the
	// pinned/unpinned row moving between the "Pinned"/"Other" groups is
	// already visible in the rendered view.
	if app.Model.Error != "" {
		t.Fatalf("pin produced a redundant error notification: %q", app.Model.Error)
	}

	// Unpin: same guarantee, the other direction.
	action = app.Model.Update("pin")
	if err := app.act(context.Background(), action); err != nil {
		t.Fatal(err)
	}
	if claude.ListCallCount() != 0 || codex.ListCallCount() != 0 {
		t.Fatalf("unpin invoked provider List: claude=%d codex=%d", claude.ListCallCount(), codex.ListCallCount())
	}
	if app.Model.Rows[0].Pinned {
		t.Fatal("unpin did not clear Pinned on the model row")
	}
	if app.Model.Error != "" {
		t.Fatalf("unpin produced a redundant error notification: %q", app.Model.Error)
	}
}

func TestApplyPinReordersAndPreservesSelectionByKey(t *testing.T) {
	now := time.Now()
	a := session.Session{Key: session.Key{Provider: session.ProviderClaude, ID: "a"}, CreatedAt: now.Add(-1 * time.Hour)}
	b := session.Session{Key: session.Key{Provider: session.ProviderCodex, ID: "b"}, CreatedAt: now.Add(-2 * time.Hour)}
	c := session.Session{Key: session.Key{Provider: session.ProviderClaude, ID: "c"}, CreatedAt: now.Add(-30 * time.Minute), Pinned: true}
	m := NewModel()
	m.Rows = []session.Session{a, b, c}
	session.SortOverview(m.Rows) // rows are always kept in this order in practice (Catalog.Load already sorts).

	// Pin "b" (currently the third-newest, unpinned row in "Other").
	var target int
	for i, r := range m.Rows {
		if r.Key.ID == "b" {
			target = i
		}
	}
	m.Selected = target
	m.ApplyPin(session.Key{Provider: session.ProviderCodex, ID: "b"}, true)

	if !m.Rows[m.Selected].Pinned || m.Rows[m.Selected].Key.ID != "b" {
		t.Fatalf("selection did not follow the pinned session: rows=%+v selected=%d", m.Rows, m.Selected)
	}
	var pinnedIDs, otherIDs []string
	for _, r := range m.Rows {
		if r.Pinned {
			pinnedIDs = append(pinnedIDs, r.Key.ID)
		} else {
			otherIDs = append(otherIDs, r.Key.ID)
		}
	}
	// Pinned group: CreatedAt descending -> c (-30m) before b (-2h).
	if len(pinnedIDs) != 2 || pinnedIDs[0] != "c" || pinnedIDs[1] != "b" {
		t.Fatalf("pinned group order=%v, want [c b]", pinnedIDs)
	}
	if len(otherIDs) != 1 || otherIDs[0] != "a" {
		t.Fatalf("other group=%v, want [a]", otherIDs)
	}
	// Pinned group must precede Other in the flat row slice.
	for i, r := range m.Rows {
		if r.Pinned {
			continue
		}
		for _, later := range m.Rows[i:] {
			if later.Pinned {
				t.Fatalf("a pinned row appears after an unpinned row: %+v", m.Rows)
			}
		}
	}

	// Unpin "c": it must move back into Other, ordered by CreatedAt
	// alongside "a", and the selection must still follow it by key.
	m.Selected = 0 // currently pointing at "c" (first pinned row)
	m.ApplyPin(session.Key{Provider: session.ProviderClaude, ID: "c"}, false)
	if m.Rows[m.Selected].Pinned || m.Rows[m.Selected].Key.ID != "c" {
		t.Fatalf("selection did not follow the unpinned session: rows=%+v selected=%d", m.Rows, m.Selected)
	}
	// Only "b" remains pinned; Other is now "c" (-30m) then "a" (-1h).
	if len(m.Rows) != 3 || !m.Rows[0].Pinned || m.Rows[0].Key.ID != "b" {
		t.Fatalf("rows=%+v", m.Rows)
	}
	if m.Rows[1].Key.ID != "c" || m.Rows[2].Key.ID != "a" {
		t.Fatalf("other group after unpin=%v, want [c a]", []string{m.Rows[1].Key.ID, m.Rows[2].Key.ID})
	}
}

func TestPinPersistenceErrorLeavesConsistentState(t *testing.T) {
	key := session.Key{Provider: session.ProviderClaude, ID: "c"}
	claude := &journeyProvider{id: session.ProviderClaude}
	catalog := session.Catalog{Providers: []session.Provider{claude}, Pins: &erroringPinStore{err: errors.New("disk full")}}
	m := NewModel()
	m.Rows = []session.Session{{Key: key, Pinned: false, CreatedAt: time.Now()}}
	m.Selected = 0
	app := App{Catalog: catalog, Model: m, CWD: "/work"}

	action := app.Model.Update("pin")
	err := app.act(context.Background(), action)
	if err == nil {
		t.Fatal("expected a persistence error")
	}
	if app.Model.Rows[0].Pinned {
		t.Fatal("model row was updated despite the persistence failure — UI and persisted state now disagree")
	}
	if claude.ListCallCount() != 0 {
		t.Fatalf("pin error path invoked provider List: %d calls", claude.ListCallCount())
	}
}

// TestPinFrameUpdateDoesNotWaitForSlowProviderList drives the real App.Run
// event loop over a pty, with both providers' List() calls carrying an
// injected 400ms delay (standing in for a slow `claude`/`codex` CLI
// subprocess). It asserts the frame rendered immediately after the Ctrl+T
// key is processed reflects the pin — well before that delay could have
// elapsed — proving the fix (skipping the post-pin catalog refresh) and
// not just that persistence itself is fast.
func TestPinFrameUpdateDoesNotWaitForSlowProviderList(t *testing.T) {
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	defer slave.Close()
	if err := pty.Setsize(slave, &pty.Winsize{Rows: 24, Cols: 80}); err != nil {
		t.Fatal(err)
	}

	store := state.New(filepath.Join(t.TempDir(), "state.json"))
	const delay = 400 * time.Millisecond
	claude := &journeyProvider{id: session.ProviderClaude, listDelay: delay, rows: []session.Session{
		{Key: session.Key{Provider: session.ProviderClaude, ID: "c"}, Name: "s", CWD: "/work", CreatedAt: time.Now()},
	}}
	codex := &journeyProvider{id: session.ProviderCodex, listDelay: delay}
	catalog := session.Catalog{Providers: []session.Provider{claude, codex}, Pins: store}

	var output bytes.Buffer
	var writeTimes []time.Time
	trackedOutput := writerFunc(func(p []byte) (int, error) {
		writeTimes = append(writeTimes, time.Now())
		return output.Write(p)
	})

	var readTimes []time.Time
	keys := []string{"pin", "quit"}
	idx := 0
	readInput := func(*bufio.Reader) (string, error) {
		k := keys[idx]
		idx++
		readTimes = append(readTimes, time.Now())
		return k, nil
	}

	model := NewModel()
	model.Scope = session.ScopeAll
	app := App{Catalog: catalog, Model: model, Input: slave, Output: trackedOutput, CWD: "/work", ReadInput: readInput}
	if err := app.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	// writeTimes[0] is beginTerminal's own write (before the loop, and
	// before a.refresh's initial catalog load); writeTimes[1] is the
	// first loop iteration's frame (after that initial load); writeTimes[2]
	// is the frame rendered right after the "pin" key was processed — the
	// one this test measures.
	if len(writeTimes) < 3 || len(readTimes) < 1 {
		t.Fatalf("insufficient frames/reads captured: writes=%d reads=%d", len(writeTimes), len(readTimes))
	}
	elapsed := writeTimes[2].Sub(readTimes[0])
	t.Logf("pin input -> updated frame: %v (injected provider List delay: %v)", elapsed, delay)
	if elapsed > delay/2 {
		t.Fatalf("pin frame update took %v after the Ctrl+T key, wanted well under the injected %v provider List delay (proves it does not wait on provider refresh)", elapsed, delay)
	}
	// Pin/unpin produces no Status/Notice text (see
	// TestPinDoesNotInvokeProviderList); the pin is instead visible as the
	// row moving under the "Pinned" group heading.
	if !strings.Contains(latestFrame(output.String()), "Pinned") {
		t.Fatalf("rendered output never reflected the pin:\n%s", output.String())
	}
}
