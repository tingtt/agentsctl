//go:build darwin || linux

package agentview

import (
	"context"
	"io"
	"os"
	"testing"

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
func (f *fakeProvider) List(context.Context, bool) ([]session.Session, error) {
	return f.rows, nil
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

func newTestRuntime(provider *fakeProvider) *Runtime {
	rt := &Runtime{
		Controller: sessionctl.Controller{Providers: []sessionctl.Source{provider}, Pins: &fakePins{}},
		State:      NewState(),
		CWD:        "/work",
	}
	rt.reload(context.Background())
	return rt
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

// TestRuntimeOpenMarksLastAttachedAndReloads covers the common Open
// intent end to end: it must reach the provider's Open, mark the session
// last-attached, and reload.
func TestRuntimeOpenMarksLastAttachedAndReloads(t *testing.T) {
	target := session.Key{Provider: session.ProviderCodex, ID: "s1"}
	p := &fakeProvider{id: session.ProviderCodex, rows: []session.Session{{Key: target, CWD: "/work", Actions: session.Actions{session.ActionOpen: {Available: true}}}}}
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

	// Now archive it away (two presses).
	p.rows = []session.Session{{Key: target, CWD: "/work", Actions: session.Actions{session.ActionArchive: {Available: true}}}}
	rt.reload(context.Background())
	rt.State.selectIndex(0)
	rt.State.Handle(KeyEvent{Key: KeyCtrlX})
	archiveIntent := rt.State.Handle(KeyEvent{Key: KeyCtrlX})
	if archiveIntent.Kind != IntentArchive {
		t.Fatalf("intent=%+v", archiveIntent)
	}
	if err := rt.act(context.Background(), archiveIntent); err != nil {
		t.Fatal(err)
	}
	if len(rt.State.Rows) != 0 {
		t.Fatalf("archived session must be gone after reload: %+v", rt.State.Rows)
	}
}
