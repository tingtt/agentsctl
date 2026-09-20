package agentview

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/tingtt/agentsctl/internal/session"
	"github.com/tingtt/agentsctl/internal/sessionctl"
)

func requireSelected(t *testing.T, s State, want session.Key) {
	t.Helper()
	got, ok := s.SelectedRow()
	if !ok || got.Key != want {
		t.Fatalf("selection=%+v ok=%v, want %s", got, ok, want)
	}
}

func requirePending(t *testing.T, s State, want session.Key) {
	t.Helper()
	if !s.requested.pending || s.requested.key != want {
		t.Fatalf("requested=%+v, want pending %s", s.requested, want)
	}
}

// Snapshots from other providers arrive independently of the target
// provider's; until the requested identity is in the catalog the request
// must be inert and survive them.
func TestRequestedSelectionDoesNotMoveSelectionBeforeTargetAppears(t *testing.T) {
	s := NewState()
	s.SetRows([]session.Session{{Key: key("a"), CWD: "/work"}, {Key: key("b"), CWD: "/work"}})
	s.selectIndex(1)
	s.RequestSelection(codexKey("run-1"))

	s.SetRows([]session.Session{{Key: key("a"), CWD: "/work"}, {Key: key("b"), CWD: "/work"}, {Key: key("c"), CWD: "/work"}})
	s.SetRows([]session.Session{{Key: key("b"), CWD: "/work"}, {Key: key("c"), CWD: "/work"}})

	requireSelected(t, s, key("b"))
	requirePending(t, s, codexKey("run-1"))
}

func TestRequestedSelectionSelectsExactKeyAndIsConsumed(t *testing.T) {
	s := NewState()
	s.SetRows([]session.Session{{Key: key("a"), CWD: "/work"}, {Key: key("b"), CWD: "/work"}})
	s.selectIndex(0)
	s.RequestSelection(key("new"))

	s.SetRows([]session.Session{{Key: key("a"), CWD: "/work"}, {Key: key("new"), CWD: "/work"}, {Key: key("b"), CWD: "/work"}})
	requireSelected(t, s, key("new"))
	if s.requested.pending {
		t.Fatalf("request not consumed: %+v", s.requested)
	}

	// Later refreshes preserve it by ordinary identity rules; and a
	// consumed request cannot pull selection back after the user moves.
	s.selectIndex(0)
	s.SetRows([]session.Session{{Key: key("a"), CWD: "/work"}, {Key: key("new"), CWD: "/work"}, {Key: key("b"), CWD: "/work"}})
	requireSelected(t, s, key("a"))
}

func TestRequestedSelectionResolvesProvisionalKeyToCanonicalRowDirectly(t *testing.T) {
	s := NewState()
	s.SetRows([]session.Session{{Key: key("a"), CWD: "/work"}})
	s.selectIndex(0)
	s.RequestSelection(codexKey("run-1"))

	// The provisional row was never applied: the first Codex catalog
	// already carries the canonical row.
	s.SetRows([]session.Session{{Key: key("a"), CWD: "/work"}, boundRow("thread-1", "run-1")})
	requireSelected(t, s, codexKey("thread-1"))
	if s.requested.pending {
		t.Fatalf("request not consumed: %+v", s.requested)
	}
}

func TestRequestedSelectionProvisionalSelectionStillFollowsCanonicalTransition(t *testing.T) {
	s := NewState()
	s.SetRows([]session.Session{{Key: key("a"), CWD: "/work"}, {Key: key("z"), CWD: "/work"}})
	s.selectIndex(1)
	s.RequestSelection(codexKey("run-1"))

	// An unrelated snapshot first: nothing moves.
	s.SetRows([]session.Session{{Key: key("a"), CWD: "/work"}, {Key: key("z"), CWD: "/work"}})
	requireSelected(t, s, key("z"))

	s.SetRows([]session.Session{{Key: key("a"), CWD: "/work"}, startingRow("run-1"), {Key: key("z"), CWD: "/work"}})
	requireSelected(t, s, codexKey("run-1"))
	if s.requested.pending {
		t.Fatalf("request not consumed: %+v", s.requested)
	}

	s.SetRows([]session.Session{{Key: key("a"), CWD: "/work"}, boundRow("thread-1", "run-1"), {Key: key("z"), CWD: "/work"}})
	requireSelected(t, s, codexKey("thread-1"))
}

func TestRequestedSelectionAbsentKeyIsNeverGuessed(t *testing.T) {
	s := NewState()
	s.SetRows([]session.Session{{Key: key("a"), CWD: "/work"}})
	s.RequestSelection(codexKey("run-1"))

	// A new-looking Codex row without continuity to the requested key.
	s.SetRows([]session.Session{{Key: key("a"), CWD: "/work"}, boundRow("thread-9")})
	requireSelected(t, s, key("a"))
	requirePending(t, s, codexKey("run-1"))
}

func TestRequestedSelectionNewerRequestSupersedesOlder(t *testing.T) {
	s := NewState()
	s.SetRows([]session.Session{{Key: key("a"), CWD: "/work"}})
	s.RequestSelection(key("first"))
	s.RequestSelection(key("second"))

	s.SetRows([]session.Session{{Key: key("a"), CWD: "/work"}, {Key: key("first"), CWD: "/work"}, {Key: key("second"), CWD: "/work"}})
	requireSelected(t, s, key("second"))
	if s.requested.pending {
		t.Fatalf("request not consumed: %+v", s.requested)
	}
}

func TestRequestedSelectionWaitsForRenameToFinish(t *testing.T) {
	s := NewState()
	s.SetRows([]session.Session{{Key: key("a"), CWD: "/work"}})
	s.selectIndex(0)
	s.Rename.Active, s.Rename.Target = true, key("a")
	s.RequestSelection(key("new"))

	s.SetRows([]session.Session{{Key: key("a"), CWD: "/work"}, {Key: key("new"), CWD: "/work"}})
	requireSelected(t, s, key("a"))
	requirePending(t, s, key("new"))
}

func twoDirectoryRows(dirA, dirB string, count int) []session.Session {
	return append(foldingRows(count, dirA, false), foldingRows(count, dirB, false)...)
}

func groupIDFor(dir string) groupID { return groupID{directory: dir} }

func TestRequestedSelectionRevealsOnlyRequiredDirectoryBlocks(t *testing.T) {
	tests := []struct {
		position    int // zero-based position within its group
		wantVisible int
	}{
		{position: 0, wantVisible: 10},
		{position: 9, wantVisible: 10},
		{position: 10, wantVisible: 20},
		{position: 19, wantVisible: 20},
		{position: 20, wantVisible: 30},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("position_%d", tt.position), func(t *testing.T) {
			s := NewState()
			s.SetRows(twoDirectoryRows("/work/a", "/work/b", 25))
			target := key(fmt.Sprintf("/work/b-%02d", tt.position+1))
			s.RequestSelection(target)

			s.SetRows(twoDirectoryRows("/work/a", "/work/b", 25))

			requireSelected(t, s, target)
			if got := s.groupStates[groupIDFor("/work/b")]; got.folded || got.visibleCount != tt.wantVisible {
				t.Fatalf("target group state=%+v, want open with %d visible", got, tt.wantVisible)
			}
			if _, touched := s.groupStates[groupIDFor("/work/a")]; touched {
				t.Fatalf("unrelated group state changed: %+v", s.groupStates[groupIDFor("/work/a")])
			}
			if _, ok := s.selectableList().item(sessionItemID(target)); !ok {
				t.Fatalf("target %s is not in the visible selectable list", target)
			}
		})
	}
}

func TestRequestedSelectionUnfoldsOnlyTargetDirectoryGroup(t *testing.T) {
	s := NewState()
	rows := twoDirectoryRows("/work/a", "/work/b", 3)
	s.SetRows(rows)
	s.setGroupState(groupIDFor("/work/a"), groupDisplayState{folded: true})
	s.setGroupState(groupIDFor("/work/b"), groupDisplayState{folded: true})
	s.RequestSelection(key("/work/b-02"))

	s.SetRows(rows)

	requireSelected(t, s, key("/work/b-02"))
	if got := s.groupStates[groupIDFor("/work/b")]; got.folded {
		t.Fatalf("target group still folded: %+v", got)
	}
	if got := s.groupStates[groupIDFor("/work/a")]; !got.folded {
		t.Fatalf("unrelated group was unfolded: %+v", got)
	}
}

func TestRequestedSelectionUnfoldsPinnedGroup(t *testing.T) {
	s := NewState()
	rows := append(foldingRows(2, "/work/pin", true), foldingRows(2, "/work/a", false)...)
	s.SetRows(rows)
	pinned := groupID{pinned: true}
	s.setGroupState(pinned, groupDisplayState{folded: true})
	s.setGroupState(groupIDFor("/work/a"), groupDisplayState{folded: true})
	s.RequestSelection(key("/work/pin-02"))

	s.SetRows(rows)

	requireSelected(t, s, key("/work/pin-02"))
	if s.groupStates[pinned].folded {
		t.Fatal("Pinned group still folded")
	}
	if !s.groupStates[groupIDFor("/work/a")].folded {
		t.Fatal("unrelated group was unfolded")
	}
}

// --- Runtime: only a successful Dispatch registers a request. ---

// seqDispatchProvider gives every Dispatch a distinct key so successive
// dispatches are distinguishable, and can be made to fail.
// Its rows are mutex-guarded because a superseded background reload may
// still be listing while the next Dispatch appends.
type seqDispatchProvider struct {
	*fakeProvider
	mu        sync.Mutex
	n         int
	failing   bool
	renameErr error
}

func (p *seqDispatchProvider) Rename(ctx context.Context, key session.Key, name string) error {
	if p.renameErr != nil {
		return p.renameErr
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.fakeProvider.Rename(ctx, key, name)
}

func (p *seqDispatchProvider) List(ctx context.Context, archived bool) ([]session.Session, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.fakeProvider.List(ctx, archived)
}

func (p *seqDispatchProvider) Dispatch(_ context.Context, prompt, cwd string) (session.Session, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failing {
		return session.Session{}, errors.New("dispatch failed")
	}
	p.n++
	s := session.Session{Key: session.Key{Provider: p.id, ID: fmt.Sprintf("new-%d", p.n)}, Name: prompt, CWD: cwd}
	p.rows = append(p.rows, s)
	return s, nil
}

func newSeqRuntime(p *seqDispatchProvider) *Runtime {
	rt := &Runtime{
		Controller: sessionctl.Controller{Providers: []sessionctl.Source{p}, Pins: &fakePins{}},
		State:      NewState(),
		CWD:        "/work",
	}
	rt.syncReload(context.Background())
	return rt
}

func dispatchIntent(prompt string) Intent {
	return Intent{Kind: IntentDispatch, Provider: session.ProviderClaude, Prompt: prompt}
}

func TestRuntimeDispatchSelectsNewSessionOnceCatalogExposesIt(t *testing.T) {
	p := &seqDispatchProvider{fakeProvider: &fakeProvider{id: session.ProviderClaude, rows: []session.Session{
		{Key: key("a"), CWD: "/work"}, {Key: key("b"), CWD: "/work"},
	}}}
	rt := newSeqRuntime(p)
	rt.State.selectIndex(1)

	if err := rt.act(context.Background(), dispatchIntent("hello")); err != nil {
		t.Fatal(err)
	}
	requirePending(t, rt.State, key("new-1"))
	// Rows stay provider-owned: nothing is injected before the reload.
	for _, row := range rt.State.Rows {
		if row.Key == key("new-1") {
			t.Fatalf("dispatched row injected before catalog reload: %+v", rt.State.Rows)
		}
	}

	rt.drainCatalog(context.Background())
	requireSelected(t, rt.State, key("new-1"))
	if rt.State.requested.pending {
		t.Fatalf("request not consumed: %+v", rt.State.requested)
	}
}

func TestRuntimeFailedDispatchDoesNotRegisterOrReplaceRequest(t *testing.T) {
	p := &seqDispatchProvider{fakeProvider: &fakeProvider{id: session.ProviderClaude, rows: []session.Session{{Key: key("a"), CWD: "/work"}}}}
	rt := newSeqRuntime(p)

	p.failing = true
	if err := rt.act(context.Background(), dispatchIntent("boom")); err == nil {
		t.Fatal("dispatch error not returned")
	}
	if rt.State.requested.pending {
		t.Fatalf("failed dispatch registered a request: %+v", rt.State.requested)
	}

	rt.State.RequestSelection(key("earlier"))
	if err := rt.act(context.Background(), dispatchIntent("boom")); err == nil {
		t.Fatal("dispatch error not returned")
	}
	requirePending(t, rt.State, key("earlier"))
}

func TestRuntimeLatestDispatchSupersedesUnresolvedRequest(t *testing.T) {
	p := &seqDispatchProvider{fakeProvider: &fakeProvider{id: session.ProviderClaude, rows: []session.Session{{Key: key("a"), CWD: "/work"}}}}
	rt := newSeqRuntime(p)

	if err := rt.act(context.Background(), dispatchIntent("one")); err != nil {
		t.Fatal(err)
	}
	if err := rt.act(context.Background(), dispatchIntent("two")); err != nil {
		t.Fatal(err)
	}
	requirePending(t, rt.State, key("new-2"))

	rt.drainCatalog(context.Background())
	requireSelected(t, rt.State, key("new-2"))
}

func TestRuntimeOrdinaryOperationsDoNotRegisterRequest(t *testing.T) {
	p := &seqDispatchProvider{fakeProvider: &fakeProvider{id: session.ProviderClaude, rows: []session.Session{
		{Key: key("a"), CWD: "/work"}, {Key: key("b"), CWD: "/work"},
	}}}
	rt := newSeqRuntime(p)
	rt.State.selectIndex(0)
	selected, _ := rt.State.SelectedRow()

	for _, intent := range []Intent{
		{Kind: IntentRefresh},
		{Kind: IntentRename, Key: key("b"), Name: "renamed"},
		{Kind: IntentPin, Key: key("b")},
	} {
		if err := rt.act(context.Background(), intent); err != nil {
			t.Fatalf("%+v: %v", intent, err)
		}
		if rt.State.CatalogLoading {
			rt.drainCatalog(context.Background())
		}
		if rt.State.requested.pending {
			t.Fatalf("%+v registered a request: %+v", intent, rt.State.requested)
		}
	}
	// A row appearing outside any dispatch never steals selection.
	p.mu.Lock()
	p.rows = append(p.rows, session.Session{Key: key("fresh"), CWD: "/work"})
	p.mu.Unlock()
	rt.syncReload(context.Background())
	requireSelected(t, rt.State, selected.Key)
}

// --- Rename exit retries a request deferred while the editor was active. ---

var renameable = session.Actions{session.ActionRename: {Available: true}}

// renamingWithDeferredRequest returns a runtime whose row "a" is being
// renamed while a request for "new" has been deferred: the catalog already
// contains "new", but the rename editor kept the selection on "a".
func renamingWithDeferredRequest(t *testing.T) (*Runtime, *seqDispatchProvider) {
	t.Helper()
	p := &seqDispatchProvider{fakeProvider: &fakeProvider{id: session.ProviderClaude, rows: []session.Session{
		{Key: key("a"), Name: "old", CWD: "/work", Actions: renameable},
	}}}
	rt := newSeqRuntime(p)
	rt.State.selectIndex(0)
	rt.State.Handle(KeyEvent{Key: KeyCtrlR})
	rt.State.RequestSelection(key("new"))

	p.mu.Lock()
	p.rows = append(p.rows, session.Session{Key: key("new"), CWD: "/work"})
	p.mu.Unlock()
	rt.syncReload(context.Background())

	if !rt.State.Rename.Active {
		t.Fatal("rename editor closed unexpectedly")
	}
	requireSelected(t, rt.State, key("a"))
	requirePending(t, rt.State, key("new"))
	return rt, p
}

func TestRequestedSelectionResolvesImmediatelyWhenRenameIsCancelled(t *testing.T) {
	s := NewState()
	rows := []session.Session{{Key: key("a"), Name: "old", CWD: "/work", Actions: renameable}}
	s.SetRows(rows)
	s.selectIndex(0)
	s.Handle(KeyEvent{Key: KeyCtrlR})
	s.RequestSelection(key("new"))

	s.SetRows(append(rows, session.Session{Key: key("new"), CWD: "/work"}))
	requireSelected(t, s, key("a"))
	requirePending(t, s, key("new"))

	// No SetRows between Esc and the assertions.
	s.Handle(KeyEvent{Key: KeyEsc})
	if s.Rename.Active {
		t.Fatal("Esc did not close the rename editor")
	}
	requireSelected(t, s, key("new"))
	if s.requested.pending {
		t.Fatalf("request not consumed: %+v", s.requested)
	}
}

func TestRequestedSelectionRenameExitRevealsHiddenTargetAndFollowsTransition(t *testing.T) {
	s := NewState()
	rows := append([]session.Session{{Key: key("a"), Name: "old", CWD: "/work/a", Actions: renameable}}, foldingRows(25, "/work/b", false)...)
	s.SetRows(rows)
	s.selectIndex(0)
	s.Handle(KeyEvent{Key: KeyCtrlR})
	s.RequestSelection(codexKey("run-1"))

	canonical := boundRow("thread-1", "run-1")
	canonical.CWD = "/work/b"
	canonical.CreatedAt = rows[len(rows)-1].CreatedAt.Add(-1)
	s.SetRows(append(append([]session.Session(nil), rows...), canonical))
	requireSelected(t, s, key("a"))

	s.Handle(KeyEvent{Key: KeyEsc})
	requireSelected(t, s, codexKey("thread-1"))
	if got := s.groupStates[groupIDFor("/work/b")]; got.folded || got.visibleCount != 30 {
		t.Fatalf("target group state=%+v, want open with 30 visible", got)
	}
}

func TestRuntimeSuccessfulRenameResolvesDeferredRequestWithoutReload(t *testing.T) {
	rt, _ := renamingWithDeferredRequest(t)
	rt.State.Rename.Draft, rt.State.Rename.Cursor = "", 0
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
	if rt.State.CatalogLoading {
		t.Fatal("rename exit must not start a catalog reload")
	}
	requireSelected(t, rt.State, key("new"))
	if rt.State.requested.pending {
		t.Fatalf("request not consumed: %+v", rt.State.requested)
	}
	for _, row := range rt.State.Rows {
		if row.Key == key("a") && row.Name != "New" {
			t.Fatalf("rename patch not applied: %+v", row)
		}
	}
}

func TestRuntimeFailedRenameKeepsRequestDeferred(t *testing.T) {
	rt, p := renamingWithDeferredRequest(t)
	p.renameErr = errors.New("rename failed")
	rt.State.Rename.Draft, rt.State.Rename.Cursor = "New", 3
	intent := rt.State.Handle(KeyEvent{Key: KeyEnter})
	if intent.Kind != IntentRename {
		t.Fatalf("intent=%+v", intent)
	}
	if err := rt.act(context.Background(), intent); err == nil {
		t.Fatal("rename error not returned")
	}

	if !rt.State.Rename.Active {
		t.Fatal("rename editor must stay open after a failed rename")
	}
	requireSelected(t, rt.State, key("a"))
	requirePending(t, rt.State, key("new"))
}
