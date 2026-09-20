//go:build darwin || linux

package agentview

import (
	"context"
	"io"
	"os"
	"testing"
	"time"

	"github.com/tingtt/agentsctl/internal/session"
	"github.com/tingtt/agentsctl/internal/sessionctl"
)

// fakeProvider implements every sessionctl capability interface, letting
// these tests drive Runtime.act through a real sessionctl.Controller
// without a real provider, subprocess, PTY, or terminal -- only the
// provider boundary is faked; Controller and Runtime are the genuine
// production types.
type fakeProvider struct {
	id      session.ProviderID
	rows    []session.Session
	opened  []session.Key
	stopped []session.Key
}

func (f *fakeProvider) ID() session.ProviderID { return f.id }

// List returns a copy of f.rows, not f.rows itself: sessionctl.Controller.
// Load's actionsFor mutates each returned session.Session's Actions field
// in place (rows[i].Actions = ...), and now that Runtime.requestReload
// runs Load in the background (see agentview_unix.go), that mutation can
// race with a test goroutine reading f.rows/p.rows directly right after
// an act() call -- exactly what a real provider's List (which never hands
// back a live reference into its own mutable state) never risks either.
func (f *fakeProvider) List(context.Context, bool) ([]session.Session, error) {
	rows := make([]session.Session, len(f.rows))
	copy(rows, f.rows)
	return rows, nil
}
func (f *fakeProvider) Dispatch(_ context.Context, prompt, cwd string) (session.Session, error) {
	s := session.Session{Key: session.Key{Provider: f.id, ID: "new"}, Name: prompt, CWD: cwd}
	f.rows = append(f.rows, s)
	return s, nil
}
func (f *fakeProvider) Open(_ context.Context, s session.Session, _ *os.File, _ io.Writer) error {
	f.opened = append(f.opened, s.Key)
	return nil
}
func (f *fakeProvider) Stop(_ context.Context, key session.Key) error {
	f.stopped = append(f.stopped, key)
	return nil
}
func (f *fakeProvider) Rename(_ context.Context, key session.Key, name string) error {
	for i := range f.rows {
		if f.rows[i].Key == key {
			f.rows[i].Name = name
		}
	}
	return nil
}
func (f *fakeProvider) Archive(_ context.Context, key session.Key) error {
	kept := f.rows[:0]
	for _, r := range f.rows {
		if r.Key != key {
			kept = append(kept, r)
		}
	}
	f.rows = kept
	return nil
}

type fakePins struct{ pinned map[string]bool }

func (p *fakePins) ListPinned() (map[string]bool, error) {
	out := make(map[string]bool, len(p.pinned))
	for k, v := range p.pinned {
		out[k] = v
	}
	return out, nil
}
func (p *fakePins) TogglePinned(k string) (bool, error) {
	if p.pinned == nil {
		p.pinned = map[string]bool{}
	}
	next := !p.pinned[k]
	if next {
		p.pinned[k] = true
	} else {
		delete(p.pinned, k)
	}
	return next, nil
}

func (p *fakePins) MigratePinned(from, to string) error {
	if p.pinned[from] {
		delete(p.pinned, from)
		p.pinned[to] = true
	}
	return nil
}

func newTestRuntime(provider *fakeProvider) *Runtime {
	rt := &Runtime{
		Controller: sessionctl.Controller{Providers: []sessionctl.Source{provider}, Pins: &fakePins{}},
		State:      NewState(),
		CWD:        "/work",
		terminal:   &fakeOverviewLifecycle{},
	}
	rt.syncReload(context.Background())
	return rt
}

func TestRuntimeCtrlTUnpinSelectsRemainingPinnedSession(t *testing.T) {
	a := session.Session{Key: key("a"), CWD: "/work", CreatedAt: time.Unix(2, 0)}
	b := session.Session{Key: key("b"), CWD: "/work", CreatedAt: time.Unix(1, 0)}
	pins := &fakePins{pinned: map[string]bool{a.Key.String(): true, b.Key.String(): true}}
	rt := &Runtime{
		Controller: sessionctl.Controller{Providers: []sessionctl.Source{&fakeProvider{id: session.ProviderClaude, rows: []session.Session{a, b}}}, Pins: pins},
		State:      NewState(),
		CWD:        "/work",
	}
	rt.syncReload(context.Background())
	rt.State.selectIndex(0)

	intent := rt.State.Handle(KeyEvent{Key: KeyCtrlT})
	if err := rt.act(context.Background(), intent); err != nil {
		t.Fatal(err)
	}

	got, ok := rt.State.SelectedRow()
	if !ok || got.Key != b.Key || !got.Pinned {
		t.Fatalf("selected row=%+v ok=%v, want remaining pinned session b", got, ok)
	}
}

// TestRuntimeDispatchReloadsAndShowsNewSession is an integration test
// through the real sessionctl.Controller and Runtime.act (only the
// provider is faked): dispatching must clear the composer and the new
// session must appear after the resulting reload.
func TestRuntimeDispatchReloadsAndShowsNewSession(t *testing.T) {
	p := &fakeProvider{id: session.ProviderClaude, rows: []session.Session{{Key: session.Key{Provider: session.ProviderClaude, ID: "a"}, CWD: "/work"}}}
	rt := newTestRuntime(p)
	rt.State.Composer.Prompt = "hello"
	intent := rt.State.Handle(KeyEvent{Key: KeyEnter})
	if intent.Kind != IntentDispatch {
		t.Fatalf("intent=%+v", intent)
	}
	if err := rt.act(context.Background(), intent); err != nil {
		t.Fatal(err)
	}
	if rt.State.Composer.Prompt != "" {
		t.Fatalf("composer not cleared: %q", rt.State.Composer.Prompt)
	}
	// Dispatch's Result.Reload starts a background reload cycle (see
	// Runtime.applyResult) rather than applying synchronously -- wait for
	// it to land before asserting on rt.State.Rows.
	rt.drainCatalog(context.Background())
	found := false
	for _, row := range rt.State.Rows {
		if row.Key.ID == "new" {
			found = true
		}
	}
	if !found {
		t.Fatalf("dispatched session missing after reload: %+v", rt.State.Rows)
	}
}

// TestRuntimeDispatchUsesSelectedSessionCWDNotStartupCWD fixes #14's
// runtime-context guarantee: dispatching a new prompt must target the
// selected session's own CWD, not Runtime.CWD (the directory agentsctl
// started in) -- display and dispatch context must never disagree (see
// the DesignDoc's composer cwd section).
func TestRuntimeDispatchUsesSelectedSessionCWDNotStartupCWD(t *testing.T) {
	p := &fakeProvider{id: session.ProviderClaude, rows: []session.Session{
		{Key: session.Key{Provider: session.ProviderClaude, ID: "a"}, CWD: "/work/repo-a"},
	}}
	rt := newTestRuntime(p)
	if rt.CWD != "/work" {
		t.Fatalf("test runtime CWD=%q, want /work (see newTestRuntime)", rt.CWD)
	}
	// The session's CWD ("/work/repo-a") differs from Runtime.CWD
	// ("/work"), so widen scope past ScopeSame's exact-match filter to
	// keep it selectable.
	rt.State.Scope = session.ScopeAll
	rt.syncReload(context.Background())
	rt.State.selectIndex(0)
	if got := rt.State.ComposerCWD(); got != "/work/repo-a" {
		t.Fatalf("ComposerCWD()=%q, want the selected row's own CWD", got)
	}
	rt.State.Composer.Prompt = "hello"
	intent := rt.State.Handle(KeyEvent{Key: KeyEnter})
	if intent.Kind != IntentDispatch {
		t.Fatalf("intent=%+v", intent)
	}
	if err := rt.act(context.Background(), intent); err != nil {
		t.Fatal(err)
	}
	var dispatched session.Session
	for _, row := range p.rows {
		if row.Key.ID == "new" {
			dispatched = row
		}
	}
	if dispatched.CWD != "/work/repo-a" {
		t.Fatalf("dispatched session CWD=%q, want the selected session's CWD /work/repo-a, not Runtime.CWD", dispatched.CWD)
	}
}

// TestRuntimeDispatchFallsBackToStartupCWDWithEmptyCatalog fixes the safe-
// fallback rule: with nothing to select, a new prompt must still dispatch
// using Runtime.CWD (StartupCWD) rather than an empty directory.
func TestRuntimeDispatchFallsBackToStartupCWDWithEmptyCatalog(t *testing.T) {
	p := &fakeProvider{id: session.ProviderClaude}
	rt := newTestRuntime(p)
	rt.State.Composer.Prompt = "hello"
	intent := rt.State.Handle(KeyEvent{Key: KeyEnter})
	if intent.Kind != IntentDispatch {
		t.Fatalf("intent=%+v", intent)
	}
	if err := rt.act(context.Background(), intent); err != nil {
		t.Fatal(err)
	}
	var dispatched session.Session
	for _, row := range p.rows {
		if row.Key.ID == "new" {
			dispatched = row
		}
	}
	if dispatched.CWD != rt.CWD {
		t.Fatalf("dispatched session CWD=%q, want fallback to Runtime.CWD %q", dispatched.CWD, rt.CWD)
	}
}

// TestRuntimeOpenMarksLastAttachedAndReloads covers the common Open
// intent end to end: it must reach the provider's Open, mark the session
// last-attached, and reload.
func TestRuntimeOpenMarksLastAttachedAndReloads(t *testing.T) {
	target := session.Key{Provider: session.ProviderChatGPT, ID: "s1"}
	p := &fakeProvider{id: session.ProviderChatGPT, rows: []session.Session{{Key: target, CWD: "/work", Actions: session.Actions{session.ActionOpen: {Available: true}}}}}
	rt := newTestRuntime(p)
	rt.State.selectIndex(0)
	intent := rt.State.Handle(KeyEvent{Key: KeyEnter})
	if intent.Kind != IntentOpen || intent.Key != target {
		t.Fatalf("intent=%+v", intent)
	}
	if err := rt.act(context.Background(), intent); err != nil {
		t.Fatal(err)
	}
	if len(p.opened) != 1 || p.opened[0] != target {
		t.Fatalf("provider Open not reached: %+v", p.opened)
	}
	if !rt.State.HasLastAttached || rt.State.LastAttachedKey != target {
		t.Fatal("Open must mark the session last-attached")
	}
}

// TestRuntimePinAppliesLocalPatchWithoutProviderRoundTrip covers the
// DesignDoc's "Pin は provider catalog を再取得せず" guarantee at the
// Runtime level: the fake provider's List must not be called again as
// part of handling the pin (verified indirectly: the row reflects the new
// pin state immediately, and reload would have reset f.rows to its
// original unpinned content since fakeProvider.List doesn't track pin
// state itself -- Pinned only ever comes from PinStore enrichment).
func TestRuntimePinAppliesLocalPatchWithoutProviderRoundTrip(t *testing.T) {
	target := session.Key{Provider: session.ProviderClaude, ID: "a"}
	p := &fakeProvider{id: session.ProviderClaude, rows: []session.Session{{Key: target, CWD: "/work"}}}
	rt := newTestRuntime(p)
	rt.State.selectIndex(0)
	intent := rt.State.Handle(KeyEvent{Key: KeyCtrlT})
	if err := rt.act(context.Background(), intent); err != nil {
		t.Fatal(err)
	}
	row, ok := rt.State.SelectedRow()
	if !ok || !row.Pinned {
		t.Fatalf("row not pinned: %+v ok=%v", row, ok)
	}
}

// TestRuntimeRenameAppliesPatchWithoutReload covers the DesignDoc's
// "Rename も provider List を再取得しない" guarantee: apply the
// already-confirmed name directly rather than reloading.
func TestRuntimeRenameAppliesPatchWithoutReload(t *testing.T) {
	target := session.Key{Provider: session.ProviderClaude, ID: "a"}
	p := &fakeProvider{id: session.ProviderClaude, rows: []session.Session{{Key: target, Name: "old", CWD: "/work", Actions: session.Actions{session.ActionRename: {Available: true}}}}}
	rt := newTestRuntime(p)
	rt.State.selectIndex(0)
	rt.State.Handle(KeyEvent{Key: KeyCtrlR})
	rt.State.Rename.Draft, rt.State.Rename.Cursor = "", 0 // clear the seeded "old" draft
	for _, r := range "New" {
		rt.State.Handle(KeyEvent{Key: KeyRune, Rune: r})
	}
	intent := rt.State.Handle(KeyEvent{Key: KeyEnter})
	if intent.Kind != IntentRename {
		t.Fatalf("intent=%+v", intent)
	}
	if err := rt.act(context.Background(), intent); err != nil {
		t.Fatal(err)
	}
	if rt.State.Rename.Active {
		t.Fatal("rename editor must close on success")
	}
	row, ok := rt.State.SelectedRow()
	if !ok || row.Name != "New" {
		t.Fatalf("row=%+v ok=%v", row, ok)
	}
}

// TestRuntimeStopAndArchiveReachProviderAndReload covers the two
// provider-mutating destructive actions end to end.
func TestRuntimeStopAndArchiveReachProviderAndReload(t *testing.T) {
	target := session.Key{Provider: session.ProviderClaude, ID: "a"}
	p := &fakeProvider{id: session.ProviderClaude, rows: []session.Session{{Key: target, CWD: "/work", Actions: session.Actions{session.ActionStop: {Available: true}}}}}
	rt := newTestRuntime(p)
	rt.State.selectIndex(0)
	intent := rt.State.Handle(KeyEvent{Key: KeyCtrlX})
	if intent.Kind != IntentStop {
		t.Fatalf("intent=%+v", intent)
	}
	if err := rt.act(context.Background(), intent); err != nil {
		t.Fatal(err)
	}
	if len(p.stopped) != 1 || p.stopped[0] != target {
		t.Fatalf("provider Stop not reached: %+v", p.stopped)
	}
	// Wait for Stop's own background reload (Result.Reload) to finish
	// reading p.rows before mutating it below -- otherwise the write races
	// the reload goroutine's concurrent List call under -race.
	rt.drainCatalog(context.Background())

	// Now archive it away (two presses).
	p.rows = []session.Session{{Key: target, CWD: "/work", Actions: session.Actions{session.ActionArchive: {Available: true}}}}
	rt.syncReload(context.Background())
	rt.State.selectIndex(0)
	rt.State.Handle(KeyEvent{Key: KeyCtrlX})
	archiveIntent := rt.State.Handle(KeyEvent{Key: KeyCtrlX})
	if archiveIntent.Kind != IntentArchive {
		t.Fatalf("intent=%+v", archiveIntent)
	}
	if err := rt.act(context.Background(), archiveIntent); err != nil {
		t.Fatal(err)
	}
	// Archive's Result.Reload starts a background reload cycle -- wait for
	// it to land before asserting the row is gone.
	rt.drainCatalog(context.Background())
	if len(rt.State.Rows) != 0 {
		t.Fatalf("archived session must be gone after reload: %+v", rt.State.Rows)
	}
}

// codexIdentityRuntime builds a Runtime over a Codex-shaped fake provider
// and the real Controller, starting with only the Starting row for run-1.
// bind swaps the provider's catalog to the bound thread row that names
// run-1 in PreviousKeys, exactly what a real Codex provider lists once the
// run-to-thread binding is confirmed.
func codexIdentityRuntime(t *testing.T) (rt *Runtime, pins *fakePins, bind func()) {
	t.Helper()
	runKey := session.Key{Provider: session.ProviderCodex, ID: "run-1"}
	threadKey := session.Key{Provider: session.ProviderCodex, ID: "thread-1"}
	starting := session.Session{Key: runKey, Name: "Starting", CWD: "/work", Activity: session.ActivityStarting, RunID: "run-1",
		Actions: session.Actions{session.ActionOpen: {Available: true}, session.ActionStop: {Available: true}}}
	bound := session.Session{Key: threadKey, CWD: "/work", Activity: session.ActivityWorking, RunID: "run-1", PreviousKeys: []session.Key{runKey},
		Actions: session.Actions{session.ActionOpen: {Available: true}, session.ActionStop: {Available: true}}}
	other := session.Session{Key: session.Key{Provider: session.ProviderCodex, ID: "other"}, CWD: "/work", Activity: session.ActivityIdle}
	p := &fakeProvider{id: session.ProviderCodex, rows: []session.Session{starting, other}}
	pins = &fakePins{}
	rt = &Runtime{
		Controller: sessionctl.Controller{Providers: []sessionctl.Source{p}, Pins: pins},
		State:      NewState(),
		CWD:        "/work",
		terminal:   &fakeOverviewLifecycle{},
	}
	rt.syncReload(context.Background())
	return rt, pins, func() { p.rows = []session.Session{bound, other} }
}

// TestRuntimeStartingRowSelectionSurvivesBinding is Issue #44's core
// regression through the real Runtime and Controller: the Starting row is
// selected, its run binds to a thread, the catalog reloads, and the
// selection is still the same session (now under its thread key).
func TestRuntimeStartingRowSelectionSurvivesBinding(t *testing.T) {
	rt, _, bind := codexIdentityRuntime(t)
	for i, r := range rt.State.Rows {
		if r.Key.ID == "run-1" {
			rt.State.selectIndex(i)
		}
	}
	if got, _ := rt.State.SelectedRow(); got.Activity != session.ActivityStarting {
		t.Fatalf("setup: selected=%+v", got)
	}

	bind()
	rt.syncReload(context.Background())

	got, ok := rt.State.SelectedRow()
	if !ok || got.Key.ID != "thread-1" || got.Activity != session.ActivityWorking {
		t.Fatalf("selected=%+v ok=%v, want the bound codex:thread-1 row", got, ok)
	}
	for _, r := range rt.State.Rows {
		if r.Key.ID == "run-1" {
			t.Fatalf("provisional row still listed: %+v", rt.State.Rows)
		}
	}
}

// TestRuntimeStartingRowPinSurvivesBinding pins the Starting row through
// the real key handling, then binds: the row stays pinned, the persisted
// pin lives only under the thread key, and it can then be unpinned like
// any other session.
func TestRuntimeStartingRowPinSurvivesBinding(t *testing.T) {
	rt, pins, bind := codexIdentityRuntime(t)
	for i, r := range rt.State.Rows {
		if r.Key.ID == "run-1" {
			rt.State.selectIndex(i)
		}
	}
	if err := rt.act(context.Background(), rt.State.Handle(KeyEvent{Key: KeyCtrlT})); err != nil {
		t.Fatal(err)
	}
	if !pins.pinned["codex:run-1"] {
		t.Fatalf("setup: pins=%v", pins.pinned)
	}

	bind()
	rt.syncReload(context.Background())

	got, ok := rt.State.SelectedRow()
	if !ok || got.Key.ID != "thread-1" || !got.Pinned {
		t.Fatalf("selected=%+v ok=%v, want pinned codex:thread-1", got, ok)
	}
	if len(pins.pinned) != 1 || !pins.pinned["codex:thread-1"] {
		t.Fatalf("pins=%v, want only codex:thread-1", pins.pinned)
	}

	if err := rt.act(context.Background(), rt.State.Handle(KeyEvent{Key: KeyCtrlT})); err != nil {
		t.Fatal(err)
	}
	rt.syncReload(context.Background())
	for _, r := range rt.State.Rows {
		if r.Pinned {
			t.Fatalf("still pinned after unpin: %+v pins=%v", r, pins.pinned)
		}
	}
	if len(pins.pinned) != 0 {
		t.Fatalf("pins=%v, want none", pins.pinned)
	}
}

// TestRuntimeLastAttachedFollowsBinding: opening the Starting row and
// returning to the overview, then a catalog reload that shows the binding,
// keeps the "last attached" marker on the same session.
func TestRuntimeLastAttachedFollowsBinding(t *testing.T) {
	rt, _, bind := codexIdentityRuntime(t)
	for i, r := range rt.State.Rows {
		if r.Key.ID == "run-1" {
			rt.State.selectIndex(i)
		}
	}
	if err := rt.act(context.Background(), rt.State.Handle(KeyEvent{Key: KeyEnter})); err != nil {
		t.Fatal(err)
	}
	// Open's Result.Reload started a background reload; let it finish
	// before mutating the fake provider's catalog.
	rt.drainCatalog(context.Background())
	bind()
	rt.syncReload(context.Background())
	if !rt.State.HasLastAttached || rt.State.LastAttachedKey.ID != "thread-1" {
		t.Fatalf("LastAttachedKey=%v", rt.State.LastAttachedKey)
	}
}
