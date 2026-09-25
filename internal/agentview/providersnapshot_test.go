//go:build darwin || linux

package agentview

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/tingtt/agentsctl/internal/session"
	"github.com/tingtt/agentsctl/internal/sessionctl"
)

// This file covers Runtime's provider snapshot store (see agentview_unix.go's
// providerSnapshots/applyLoadSnapshot/applyObserverUpdate/recomputeRows): retaining a
// provider's last-known rows across reload cycles and Observer
// publications, so a still-refreshing or failed provider never disappears
// from State.Rows -- the Agent View half of the DesignDoc's ChatGPT
// last-known-good catalog (the provider-cache half is covered by
// internal/provider/chatgpt's own tests).

// failableProvider wraps fakeProvider to make List return a configurable
// error instead of delegating, without touching fakeProvider.rows itself
// -- letting a test simulate a provider whose remote refresh started
// failing while its previously-known rows stay exactly as they were.
type failableProvider struct {
	*fakeProvider
	err error
}

func (f *failableProvider) List(ctx context.Context, archived bool) ([]session.Session, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.fakeProvider.List(ctx, archived)
}

// observerFakeProvider augments fakeProvider with a test-driven
// sessionctl.Observer subscription: the test sends ProviderUpdate values
// on updates and Observe forwards them to whichever subscriber (Runtime,
// via sessionctl.Controller.Observe) is currently listening -- the same
// shape a real Observer-capable provider (e.g. chatgpt.Provider) exposes,
// without a real background refresh.
type observerFakeProvider struct {
	*fakeProvider
	updates chan sessionctl.ProviderUpdate
}

func newObserverFakeProvider(fp *fakeProvider) *observerFakeProvider {
	return &observerFakeProvider{fakeProvider: fp, updates: make(chan sessionctl.ProviderUpdate, 8)}
}

func (o *observerFakeProvider) Observe(ctx context.Context) <-chan sessionctl.ProviderUpdate {
	out := make(chan sessionctl.ProviderUpdate, 8)
	go func() {
		defer close(out)
		for {
			select {
			case upd, ok := <-o.updates:
				if !ok {
					return
				}
				select {
				case out <- upd:
				case <-ctx.Done():
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	return out
}

// drainN drains exactly n current-generation catalogCh arrivals (applying
// each exactly as eventLoop's catalogCh case does), for a test asserting
// mid-cycle state where one provider is deliberately left pending --
// unlike drainCatalog, this does not wait for the cycle's own done event.
func (r *Runtime) drainN(t *testing.T, n int) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for i := 0; i < n; i++ {
		select {
		case upd := <-r.catalogCh:
			if upd.gen != r.catalogGen {
				i--
				continue
			}
			r.currentScope = upd.scope
			if upd.ps.Provider != "" {
				r.applyLoadSnapshot(upd.ps.Provider, upd.ps.Sessions, upd.ps.Err, upd.ps.ListOwnsStatus)
			}
			r.recomputeRows()
		case <-deadline:
			t.Fatal("drainN: expected catalogCh arrivals never arrived")
		}
	}
}

// drainObserver applies exactly one r.observerCh update the same way
// eventLoop's observerCh case does.
func (r *Runtime) drainObserver(t *testing.T) {
	t.Helper()
	select {
	case upd, ok := <-r.observerCh:
		if !ok {
			t.Fatal("observerCh closed unexpectedly")
			return
		}
		r.applyObserverUpdate(upd.Provider, upd.Sessions, upd.Err, upd.Warning)
		r.recomputeRows()
	case <-time.After(2 * time.Second):
		t.Fatal("expected observer update never arrived")
	}
}

func rowNames(rows []session.Session) []string {
	names := make([]string, len(rows))
	for i, r := range rows {
		names[i] = r.Name
	}
	return names
}

func contains(values []string, target string) bool {
	for _, v := range values {
		if v == target {
			return true
		}
	}
	return false
}

// TestSlowProviderRetainsOldRowsDuringPartialReload fixes the DesignDoc's
// "slow provider retains old rows" guarantee: while one provider (e.g.
// ChatGPT, still enumerating in the background) hasn't reported in the
// current reload cycle yet, its previously-known rows must stay in
// State.Rows -- never disappear -- alongside whichever other providers
// already reported this cycle.
func TestSlowProviderRetainsOldRowsDuringPartialReload(t *testing.T) {
	claude := &fakeProvider{id: session.ProviderClaude, rows: []session.Session{{Key: session.Key{Provider: session.ProviderClaude, ID: "a"}, Name: "A", CWD: "/work"}}}
	codex := &fakeProvider{id: session.ProviderCodex, rows: []session.Session{{Key: session.Key{Provider: session.ProviderCodex, ID: "b"}, Name: "B", CWD: "/work"}}}
	chatgptFP := &fakeProvider{id: session.ProviderChatGPT, rows: []session.Session{{Key: session.Key{Provider: session.ProviderChatGPT, ID: "c"}, Name: "C", CWD: "/work"}}}
	chatgpt := newGatedProvider(chatgptFP)
	rt := &Runtime{Controller: sessionctl.Controller{Providers: []sessionctl.Source{claude, codex, chatgpt}, Pins: &fakePins{}}, State: NewState(), CWD: "/work"}

	// Seed all three providers' rows via one fully-completed initial cycle.
	rt.requestReload(context.Background())
	initial := awaitCall(t, chatgpt.calls, 2*time.Second)
	close(initial.release)
	rt.drainCatalog(context.Background())
	if len(rt.State.Rows) != 3 {
		t.Fatalf("initial catalog not fully loaded: %+v", rowNames(rt.State.Rows))
	}

	// Second cycle: claude reports a fresh row, chatgpt is left pending
	// (its gated call deliberately never released here).
	claude.rows = []session.Session{{Key: session.Key{Provider: session.ProviderClaude, ID: "a2"}, Name: "A2", CWD: "/work"}}
	rt.requestReload(context.Background())
	pending := awaitCall(t, chatgpt.calls, 2*time.Second)
	rt.drainN(t, 2) // claude + codex, the only two providers that can have arrived

	names := rowNames(rt.State.Rows)
	if !contains(names, "A2") || !contains(names, "B") || !contains(names, "C") {
		t.Fatalf("a still-pending provider's last-known rows must survive a partial reload: rows=%v", names)
	}
	if !rt.State.CatalogLoading {
		t.Fatal("CatalogLoading must still be true while chatgpt is pending")
	}
	close(pending.release)
}

// TestProviderFailureRetainsCachedRowsAndWarning fixes the DesignDoc's
// "provider failure retains cached rows" guarantee: a provider whose List
// starts failing must keep showing its previously-known rows, with only a
// warning added -- never an empty/vanished section.
func TestProviderFailureRetainsCachedRowsAndWarning(t *testing.T) {
	claude := &fakeProvider{id: session.ProviderClaude, rows: []session.Session{{Key: session.Key{Provider: session.ProviderClaude, ID: "a"}, Name: "A", CWD: "/work"}}}
	chatgpt := &failableProvider{fakeProvider: &fakeProvider{id: session.ProviderChatGPT, rows: []session.Session{{Key: session.Key{Provider: session.ProviderChatGPT, ID: "c"}, Name: "C", CWD: "/work"}}}}
	rt := &Runtime{Controller: sessionctl.Controller{Providers: []sessionctl.Source{claude, chatgpt}, Pins: &fakePins{}}, State: NewState(), CWD: "/work"}

	rt.requestReload(context.Background())
	rt.drainCatalog(context.Background())
	if len(rt.State.Rows) != 2 {
		t.Fatalf("initial catalog not loaded: %+v", rowNames(rt.State.Rows))
	}

	chatgpt.err = errors.New("chatgpt bridge broken")
	rt.requestReload(context.Background())
	rt.drainCatalog(context.Background())

	names := rowNames(rt.State.Rows)
	if !contains(names, "A") || !contains(names, "C") {
		t.Fatalf("a failed refresh must not discard previously-known rows: rows=%v", names)
	}
	if rt.State.Warnings[session.ProviderChatGPT] == nil {
		t.Fatal("expected a warning recorded for the failed provider")
	}
}

// TestObserverUpdateReplacesOnlyThatProviderWithoutDuplicates fixes two of
// the DesignDoc's Observer guarantees together: a ChatGPT-only Observer
// publication must never touch Claude/Codex rows, and each publication
// must fully replace -- never append to -- that provider's own rows (no
// duplicates from a stale entry surviving a replacement).
func TestObserverUpdateReplacesOnlyThatProviderWithoutDuplicates(t *testing.T) {
	claude := &fakeProvider{id: session.ProviderClaude, rows: []session.Session{{Key: session.Key{Provider: session.ProviderClaude, ID: "a"}, Name: "A", CWD: "/work"}}}
	codex := &fakeProvider{id: session.ProviderCodex, rows: []session.Session{{Key: session.Key{Provider: session.ProviderCodex, ID: "b"}, Name: "B", CWD: "/work"}}}
	chatgpt := newObserverFakeProvider(&fakeProvider{id: session.ProviderChatGPT})
	rt := &Runtime{Controller: sessionctl.Controller{Providers: []sessionctl.Source{claude, codex, chatgpt}, Pins: &fakePins{}}, State: NewState(), CWD: "/work"}

	ctx := context.Background()
	rt.observerCh = rt.Controller.Observe(ctx)
	rt.requestReload(ctx)
	rt.drainCatalog(ctx)

	cRow := session.Session{Key: session.Key{Provider: session.ProviderChatGPT, ID: "c"}, Name: "C", CWD: "/work", Actions: session.Actions{session.ActionOpen: {Available: true}}}
	chatgpt.updates <- sessionctl.ProviderUpdate{Sessions: []session.Session{cRow}}
	rt.drainObserver(t)

	names := rowNames(rt.State.Rows)
	if !contains(names, "A") || !contains(names, "B") || !contains(names, "C") {
		t.Fatalf("initial observer publication missing: rows=%v", names)
	}

	c2Row := session.Session{Key: session.Key{Provider: session.ProviderChatGPT, ID: "c2"}, Name: "C2", CWD: "/work", Actions: session.Actions{session.ActionOpen: {Available: true}}}
	chatgpt.updates <- sessionctl.ProviderUpdate{Sessions: []session.Session{c2Row}}
	rt.drainObserver(t)

	names = rowNames(rt.State.Rows)
	if !contains(names, "A") || !contains(names, "B") {
		t.Fatalf("an observer replacement for chatgpt must not touch claude/codex: rows=%v", names)
	}
	if contains(names, "C") {
		t.Fatalf("an observer publication must replace, not append to, that provider's rows: rows=%v", names)
	}
	count := 0
	for _, row := range rt.State.Rows {
		if row.Key.ID == "c2" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly one c2 row after replacement, got %d: rows=%v", count, names)
	}
}

// TestObserverSuccessWithWarningReplacesRowsAndSurfacesWarning fixes the
// non-fatal-warning half of applyObserverUpdate: a successful Observer
// publication that also carries a Warning (e.g. ChatGPT's catalog
// refreshed fine but failed to persist locally) must still fully replace
// that provider's rows -- Warning is never a reason to keep old rows or
// treat the update as a failure -- while also surfacing the warning. A
// later clean success (no Warning) must both replace the rows again and
// clear the warning.
func TestObserverSuccessWithWarningReplacesRowsAndSurfacesWarning(t *testing.T) {
	chatgpt := newObserverFakeProvider(&fakeProvider{id: session.ProviderChatGPT})
	rt := &Runtime{Controller: sessionctl.Controller{Providers: []sessionctl.Source{chatgpt}, Pins: &fakePins{}}, State: NewState(), CWD: "/work"}

	ctx := context.Background()
	rt.observerCh = rt.Controller.Observe(ctx)
	rt.requestReload(ctx)
	rt.drainCatalog(ctx)

	bRow := session.Session{Key: session.Key{Provider: session.ProviderChatGPT, ID: "b"}, Name: "B", CWD: "/work", Actions: session.Actions{session.ActionOpen: {Available: true}}}
	persistErr := errors.New("chatgpt: persist catalog: disk full")
	chatgpt.updates <- sessionctl.ProviderUpdate{Sessions: []session.Session{bRow}, Warning: persistErr}
	rt.drainObserver(t)

	names := rowNames(rt.State.Rows)
	if !contains(names, "B") {
		t.Fatalf("a warning-bearing success must still replace rows: rows=%v", names)
	}
	if rt.State.Warnings[session.ProviderChatGPT] == nil {
		t.Fatal("expected the persistence warning to be surfaced")
	}

	cRow := session.Session{Key: session.Key{Provider: session.ProviderChatGPT, ID: "c"}, Name: "C", CWD: "/work", Actions: session.Actions{session.ActionOpen: {Available: true}}}
	chatgpt.updates <- sessionctl.ProviderUpdate{Sessions: []session.Session{cRow}}
	rt.drainObserver(t)

	names = rowNames(rt.State.Rows)
	if !contains(names, "C") || contains(names, "B") {
		t.Fatalf("a later clean success must replace rows again: rows=%v", names)
	}
	if rt.State.Warnings[session.ProviderChatGPT] != nil {
		t.Fatalf("a later clean success must clear the previous warning: %v", rt.State.Warnings[session.ProviderChatGPT])
	}
}

// TestOpenCachedSessionDoesNotWaitForInFlightReload fixes the DesignDoc's
// Phase 7 regression test at the Agent View level: a session already in
// State.Rows from a completed cycle must remain immediately openable while
// a new reload cycle's provider fetch (e.g. ChatGPT's background refresh)
// is still in flight for that very provider.
func TestOpenCachedSessionDoesNotWaitForInFlightReload(t *testing.T) {
	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer inR.Close()
	defer inW.Close()

	chatgptFP := &fakeProvider{id: session.ProviderChatGPT, rows: []session.Session{{Key: session.Key{Provider: session.ProviderChatGPT, ID: "c"}, Name: "C", CWD: "/work"}}}
	chatgpt := newGatedProvider(chatgptFP)
	rt := &Runtime{Controller: sessionctl.Controller{Providers: []sessionctl.Source{chatgpt}, Pins: &fakePins{}}, State: NewState(), Input: inR, Output: &syncBuffer{}, CWD: "/work", terminal: &fakeOverviewLifecycle{}}

	rt.requestReload(context.Background())
	initial := awaitCall(t, chatgpt.calls, 2*time.Second)
	close(initial.release)
	rt.drainCatalog(context.Background())
	if len(rt.State.Rows) != 1 {
		t.Fatalf("initial catalog not loaded: %+v", rowNames(rt.State.Rows))
	}
	row := rt.State.Rows[0]

	rt.requestReload(context.Background())
	inFlight := awaitCall(t, chatgpt.calls, 2*time.Second)
	defer close(inFlight.release)

	done := make(chan error, 1)
	go func() { done <- rt.act(context.Background(), Intent{Kind: IntentOpen, Key: row.Key}) }()
	select {
	case actErr := <-done:
		if actErr != nil {
			t.Fatalf("Open failed: %v", actErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Open blocked behind the in-flight background reload")
	}
	if len(chatgptFP.opened) != 1 || chatgptFP.opened[0] != row.Key {
		t.Fatalf("opened=%v, want exactly [%v]", chatgptFP.opened, row.Key)
	}
}

// This section fixes a real regression: a successful cached List
// (ProviderSnapshot.ListOwnsStatus == false for an Observer-capable
// provider) must never clear a warning only Observer is entitled to
// replace or clear -- see applyLoadSnapshot's doc comment. Without this,
// a routine Ctrl+L against ChatGPT (whose List is now a cheap cached
// read, not a real refresh) would silently erase an unresolved
// persistence or refresh-failure warning the instant the cached read
// itself merely succeeded.

// TestCachedListSuccessPreservesObserverDurabilityWarning is the exact
// regression sequence from the DesignDoc: an Observer-published
// persistence-durability warning must survive an intervening successful
// cached List (Ctrl+L).
func TestCachedListSuccessPreservesObserverDurabilityWarning(t *testing.T) {
	bRow := session.Session{Key: session.Key{Provider: session.ProviderChatGPT, ID: "b"}, Name: "B", CWD: "/work"}
	fp := &fakeProvider{id: session.ProviderChatGPT, rows: []session.Session{bRow}}
	chatgpt := newObserverFakeProvider(fp)
	rt := &Runtime{Controller: sessionctl.Controller{Providers: []sessionctl.Source{chatgpt}, Pins: &fakePins{}}, State: NewState(), CWD: "/work"}

	ctx := context.Background()
	rt.observerCh = rt.Controller.Observe(ctx)
	rt.requestReload(ctx)
	rt.drainCatalog(ctx)

	persistErr := errors.New("chatgpt: persist catalog: disk full")
	chatgpt.updates <- sessionctl.ProviderUpdate{Sessions: []session.Session{bRow}, Warning: persistErr}
	rt.drainObserver(t)
	if rt.State.Warnings[session.ProviderChatGPT] == nil {
		t.Fatal("expected the persistence warning to be recorded")
	}

	// Ctrl+L: an ordinary reload cycle where ChatGPT's cached List simply
	// succeeds with the same rows. This must NOT clear the still-
	// unresolved durability warning.
	rt.requestReload(ctx)
	rt.drainCatalog(ctx)

	if rt.State.Warnings[session.ProviderChatGPT] == nil {
		t.Fatal("a successful cached List must not clear an Observer-owned durability warning")
	}
	if !contains(rowNames(rt.State.Rows), "B") {
		t.Fatalf("rows should still include B: %v", rowNames(rt.State.Rows))
	}
}

// TestCachedListSuccessPreservesObserverRefreshFailureWarning is the
// DesignDoc's "refresh failure regression test": a remote-refresh-failure
// warning from Observer must likewise survive an intervening successful
// cached List, and only a later Observer success may finally clear it.
func TestCachedListSuccessPreservesObserverRefreshFailureWarning(t *testing.T) {
	bRow := session.Session{Key: session.Key{Provider: session.ProviderChatGPT, ID: "b"}, Name: "B", CWD: "/work"}
	fp := &fakeProvider{id: session.ProviderChatGPT, rows: []session.Session{bRow}}
	chatgpt := newObserverFakeProvider(fp)
	rt := &Runtime{Controller: sessionctl.Controller{Providers: []sessionctl.Source{chatgpt}, Pins: &fakePins{}}, State: NewState(), CWD: "/work"}

	ctx := context.Background()
	rt.observerCh = rt.Controller.Observe(ctx)
	rt.requestReload(ctx)
	rt.drainCatalog(ctx)

	chatgpt.updates <- sessionctl.ProviderUpdate{Sessions: []session.Session{bRow}}
	rt.drainObserver(t)
	if rt.State.Warnings[session.ProviderChatGPT] != nil {
		t.Fatalf("unexpected warning after a clean success: %v", rt.State.Warnings[session.ProviderChatGPT])
	}

	chatgpt.updates <- sessionctl.ProviderUpdate{Err: errors.New("timeout")}
	rt.drainObserver(t)
	if rt.State.Warnings[session.ProviderChatGPT] == nil {
		t.Fatal("expected the remote refresh failure to be recorded as a warning")
	}

	// Ctrl+L: cached List still returns B successfully -- must not clear
	// the still-pending refresh-failure warning.
	rt.requestReload(ctx)
	rt.drainCatalog(ctx)
	if rt.State.Warnings[session.ProviderChatGPT] == nil {
		t.Fatal("a successful cached List must not clear an Observer-owned refresh-failure warning")
	}
	if !contains(rowNames(rt.State.Rows), "B") {
		t.Fatalf("rows should still include B: %v", rowNames(rt.State.Rows))
	}

	// Only a later Observer success may finally clear it.
	cRow := session.Session{Key: session.Key{Provider: session.ProviderChatGPT, ID: "c"}, Name: "C", CWD: "/work"}
	chatgpt.updates <- sessionctl.ProviderUpdate{Sessions: []session.Session{cRow}}
	rt.drainObserver(t)
	if rt.State.Warnings[session.ProviderChatGPT] != nil {
		t.Fatalf("expected the later Observer success to clear the warning: %v", rt.State.Warnings[session.ProviderChatGPT])
	}
	names := rowNames(rt.State.Rows)
	if !contains(names, "C") || contains(names, "B") {
		t.Fatalf("rows should now be replaced by C: %v", names)
	}
}

// TestObserverWarningReplacementSurvivesInterveningCachedListSuccess
// covers the DesignDoc's "Observer warning replacement test": Observer
// remains authoritative for status through a full cycle of durability
// warning -> (cached List success, no effect) -> refresh failure -> new
// durability warning, never losing track of "latest provider problem
// wins" to an intervening cached List.
func TestObserverWarningReplacementSurvivesInterveningCachedListSuccess(t *testing.T) {
	bRow := session.Session{Key: session.Key{Provider: session.ProviderChatGPT, ID: "b"}, Name: "B", CWD: "/work"}
	fp := &fakeProvider{id: session.ProviderChatGPT, rows: []session.Session{bRow}}
	chatgpt := newObserverFakeProvider(fp)
	rt := &Runtime{Controller: sessionctl.Controller{Providers: []sessionctl.Source{chatgpt}, Pins: &fakePins{}}, State: NewState(), CWD: "/work"}

	ctx := context.Background()
	rt.observerCh = rt.Controller.Observe(ctx)
	rt.requestReload(ctx)
	rt.drainCatalog(ctx)

	chatgpt.updates <- sessionctl.ProviderUpdate{Sessions: []session.Session{bRow}, Warning: errors.New("disk full")}
	rt.drainObserver(t)

	rt.requestReload(ctx)
	rt.drainCatalog(ctx)
	if rt.State.Warnings[session.ProviderChatGPT] == nil {
		t.Fatal("durability warning must survive the intervening cached List success")
	}

	chatgpt.updates <- sessionctl.ProviderUpdate{Err: errors.New("remote failure")}
	rt.drainObserver(t)
	if !contains(rowNames(rt.State.Rows), "B") {
		t.Fatalf("sessions must be retained on a refresh failure: %v", rowNames(rt.State.Rows))
	}

	cRow := session.Session{Key: session.Key{Provider: session.ProviderChatGPT, ID: "c"}, Name: "C", CWD: "/work"}
	chatgpt.updates <- sessionctl.ProviderUpdate{Sessions: []session.Session{cRow}, Warning: errors.New("disk full again")}
	rt.drainObserver(t)
	names := rowNames(rt.State.Rows)
	if !contains(names, "C") || contains(names, "B") {
		t.Fatalf("rows should now be replaced by C: %v", names)
	}
	if rt.State.Warnings[session.ProviderChatGPT] == nil {
		t.Fatal("expected the new durability warning to be visible")
	}
}

// TestNonObserverProviderListSuccessClearsPreviousListWarning is the
// DesignDoc's "non-Observer recovery regression test": the fix must not
// make List-reported warnings sticky for an ordinary provider (e.g.
// Claude/Codex) that never implements Observer -- a successful List still
// clears a previous List failure exactly as before this change.
func TestNonObserverProviderListSuccessClearsPreviousListWarning(t *testing.T) {
	claude := &failableProvider{
		fakeProvider: &fakeProvider{id: session.ProviderClaude, rows: []session.Session{{Key: session.Key{Provider: session.ProviderClaude, ID: "a"}, Name: "A", CWD: "/work"}}},
		err:          errors.New("temporary error"),
	}
	rt := &Runtime{Controller: sessionctl.Controller{Providers: []sessionctl.Source{claude}, Pins: &fakePins{}}, State: NewState(), CWD: "/work"}

	rt.requestReload(context.Background())
	rt.drainCatalog(context.Background())
	if rt.State.Warnings[session.ProviderClaude] == nil {
		t.Fatal("expected the initial List failure to be recorded as a warning")
	}

	claude.err = nil
	rt.requestReload(context.Background())
	rt.drainCatalog(context.Background())
	if rt.State.Warnings[session.ProviderClaude] != nil {
		t.Fatalf("a successful List for a non-Observer provider must clear its previous warning: %v", rt.State.Warnings[session.ProviderClaude])
	}
	if !contains(rowNames(rt.State.Rows), "A") {
		t.Fatalf("rows=%v", rowNames(rt.State.Rows))
	}
}

// This section fixes a second real regression, independent of the
// warning-preservation one above: once Observer has published a
// successful full catalog for a provider, that provider's rows belong to
// Observer -- not List -- from then on (see providerState.
// observerSnapshotSeen and applyLoadSnapshot's doc comment). Without
// this, a slower LoadStream List arrival from the same (or an earlier)
// reload cycle could be delivered AFTER a faster background refresh has
// already published a newer Observer catalog, silently rolling the UI
// back to a stale snapshot -- catalogGen alone cannot catch this, since
// Observer publications are deliberately independent of reload
// generations (see the DesignDoc's "Observer generations").

// gatedObserverProvider combines gatedProvider's deterministic,
// one-call-at-a-time control over List with a test-driven Observer
// subscription (see newObserverFakeProvider) -- needed to construct the
// exact race this section fixes: a List call held open while an Observer
// publication is applied out from under it.
type gatedObserverProvider struct {
	*gatedProvider
	updates chan sessionctl.ProviderUpdate
}

func newGatedObserverProvider(fp *fakeProvider) *gatedObserverProvider {
	return &gatedObserverProvider{gatedProvider: newGatedProvider(fp), updates: make(chan sessionctl.ProviderUpdate, 8)}
}

func (g *gatedObserverProvider) Observe(ctx context.Context) <-chan sessionctl.ProviderUpdate {
	out := make(chan sessionctl.ProviderUpdate, 8)
	go func() {
		defer close(out)
		for {
			select {
			case upd, ok := <-g.updates:
				if !ok {
					return
				}
				select {
				case out <- upd:
				case <-ctx.Done():
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	return out
}

// TestObserverRowsSurviveDelayedStaleCachedList is the primary
// acceptance test: a LoadStream List call already in flight when Observer
// publishes a newer catalog must never be allowed to roll rows back once
// it finally completes -- exercised across two repeated Ctrl+L cycles to
// prove the lock-in persists, not just survives once.
func TestObserverRowsSurviveDelayedStaleCachedList(t *testing.T) {
	bRow := session.Session{Key: session.Key{Provider: session.ProviderChatGPT, ID: "b"}, Name: "B", CWD: "/work"}
	cRow := session.Session{Key: session.Key{Provider: session.ProviderChatGPT, ID: "c"}, Name: "C", CWD: "/work"}
	fp := &fakeProvider{id: session.ProviderChatGPT, rows: []session.Session{bRow}}
	chatgpt := newGatedObserverProvider(fp)
	rt := &Runtime{Controller: sessionctl.Controller{Providers: []sessionctl.Source{chatgpt}, Pins: &fakePins{}}, State: NewState(), CWD: "/work"}

	ctx := context.Background()
	rt.observerCh = rt.Controller.Observe(ctx)
	// requestReload below initializes providerSnapshots, but this test
	// pushes an Observer update before ever draining a catalogEvent (the
	// only other place currentScope gets set) -- set it explicitly here,
	// exactly as Run() does before its own first requestReload.
	rt.currentScope = session.Scope{CurrentDirectory: "/work"}

	// Cycle 1: a reload's List(B) starts and blocks -- modeling the
	// slower LoadStream arm of a Ctrl+L cycle -- while the faster
	// background refresh (Observer) completes first with a newer catalog.
	rt.requestReload(ctx)
	call := awaitCall(t, chatgpt.calls, 2*time.Second)
	chatgpt.updates <- sessionctl.ProviderUpdate{Sessions: []session.Session{cRow}}
	rt.drainObserver(t)
	if !contains(rowNames(rt.State.Rows), "C") {
		t.Fatalf("expected C after the Observer publication: %v", rowNames(rt.State.Rows))
	}
	close(call.release) // deliver the now-stale List(B)
	rt.drainCatalog(ctx)

	names := rowNames(rt.State.Rows)
	if !contains(names, "C") || contains(names, "B") {
		t.Fatalf("a stale cached List must never roll rows back once Observer owns them: %v", names)
	}

	// Cycle 2: repeat Ctrl+L -- the lock-in must persist, not just survive
	// once. ChatGPT's own List keeps "returning" the stale B.
	rt.requestReload(ctx)
	call2 := awaitCall(t, chatgpt.calls, 2*time.Second)
	close(call2.release)
	rt.drainCatalog(ctx)

	names = rowNames(rt.State.Rows)
	if !contains(names, "C") || contains(names, "B") {
		t.Fatalf("rows must remain C across repeated Ctrl+L cycles: %v", names)
	}
}

// TestListBootstrapsRowsBeforeFirstObserverSuccess fixes the other half
// of the same mechanism: before Observer has ever published successfully,
// List is still allowed -- expected -- to seed rows (the persisted-cache
// restart-bootstrap path), and the first Observer success then takes over
// row ownership from it.
func TestListBootstrapsRowsBeforeFirstObserverSuccess(t *testing.T) {
	bRow := session.Session{Key: session.Key{Provider: session.ProviderChatGPT, ID: "b"}, Name: "B", CWD: "/work"}
	fp := &fakeProvider{id: session.ProviderChatGPT, rows: []session.Session{bRow}}
	chatgpt := newObserverFakeProvider(fp)
	rt := &Runtime{Controller: sessionctl.Controller{Providers: []sessionctl.Source{chatgpt}, Pins: &fakePins{}}, State: NewState(), CWD: "/work"}

	ctx := context.Background()
	rt.observerCh = rt.Controller.Observe(ctx)
	rt.requestReload(ctx)
	rt.drainCatalog(ctx)
	if !contains(rowNames(rt.State.Rows), "B") {
		t.Fatalf("List must bootstrap rows before any Observer success: %v", rowNames(rt.State.Rows))
	}

	cRow := session.Session{Key: session.Key{Provider: session.ProviderChatGPT, ID: "c"}, Name: "C", CWD: "/work"}
	chatgpt.updates <- sessionctl.ProviderUpdate{Sessions: []session.Session{cRow}}
	rt.drainObserver(t)
	names := rowNames(rt.State.Rows)
	if !contains(names, "C") || contains(names, "B") {
		t.Fatalf("the first Observer success must take over row ownership from List: %v", names)
	}
}

// TestObserverErrorDoesNotBlockBootstrapList fixes that an Observer
// *failure* never transfers row authority: a persisted-cache bootstrap
// List result must still be able to seed rows even after an initial
// background refresh has already failed.
func TestObserverErrorDoesNotBlockBootstrapList(t *testing.T) {
	bRow := session.Session{Key: session.Key{Provider: session.ProviderChatGPT, ID: "b"}, Name: "B", CWD: "/work"}
	fp := &fakeProvider{id: session.ProviderChatGPT, rows: []session.Session{bRow}}
	chatgpt := newObserverFakeProvider(fp)
	rt := &Runtime{Controller: sessionctl.Controller{Providers: []sessionctl.Source{chatgpt}, Pins: &fakePins{}}, State: NewState(), CWD: "/work"}

	ctx := context.Background()
	rt.observerCh = rt.Controller.Observe(ctx)
	// This test pushes an Observer update before any requestReload call
	// has ever run (which is what normally lazily initializes
	// providerSnapshots) -- initialize it explicitly, mirroring Run()'s
	// own startup sequencing (Observe subscribes before the first
	// requestReload's background List/Refresh work completes).
	rt.providerSnapshots = map[session.ProviderID]providerState{}
	rt.currentScope = session.Scope{CurrentDirectory: "/work"}

	chatgpt.updates <- sessionctl.ProviderUpdate{Err: errors.New("timeout")}
	rt.drainObserver(t)
	if rt.State.Warnings[session.ProviderChatGPT] == nil {
		t.Fatal("expected the observer error to be recorded")
	}

	rt.requestReload(ctx)
	rt.drainCatalog(ctx)
	names := rowNames(rt.State.Rows)
	if !contains(names, "B") {
		t.Fatalf("a persisted-cache bootstrap List must still seed rows after an initial Observer failure: %v", names)
	}
	if rt.State.Warnings[session.ProviderChatGPT] == nil {
		t.Fatal("the observer error's warning must remain (List does not own status for this provider)")
	}
}

// TestObserverErrorAfterAuthoritativeSnapshotThenRecovery fixes the tail
// end of the state machine: once Observer owns rows, a later Observer
// error must retain those rows and surface as the warning (List still
// cannot roll them back or clear that warning), and a later Observer
// success recovers cleanly.
func TestObserverErrorAfterAuthoritativeSnapshotThenRecovery(t *testing.T) {
	bRow := session.Session{Key: session.Key{Provider: session.ProviderChatGPT, ID: "b"}, Name: "B", CWD: "/work"}
	cRow := session.Session{Key: session.Key{Provider: session.ProviderChatGPT, ID: "c"}, Name: "C", CWD: "/work"}
	dRow := session.Session{Key: session.Key{Provider: session.ProviderChatGPT, ID: "d"}, Name: "D", CWD: "/work"}
	fp := &fakeProvider{id: session.ProviderChatGPT, rows: []session.Session{bRow}}
	chatgpt := newObserverFakeProvider(fp)
	rt := &Runtime{Controller: sessionctl.Controller{Providers: []sessionctl.Source{chatgpt}, Pins: &fakePins{}}, State: NewState(), CWD: "/work"}

	ctx := context.Background()
	rt.observerCh = rt.Controller.Observe(ctx)
	// See TestObserverErrorDoesNotBlockBootstrapList's comment: no
	// requestReload has run yet to lazily initialize these.
	rt.providerSnapshots = map[session.ProviderID]providerState{}
	rt.currentScope = session.Scope{CurrentDirectory: "/work"}

	chatgpt.updates <- sessionctl.ProviderUpdate{Sessions: []session.Session{cRow}}
	rt.drainObserver(t)

	chatgpt.updates <- sessionctl.ProviderUpdate{Err: errors.New("timeout")}
	rt.drainObserver(t)
	if !contains(rowNames(rt.State.Rows), "C") {
		t.Fatalf("rows=%v", rowNames(rt.State.Rows))
	}
	if rt.State.Warnings[session.ProviderChatGPT] == nil {
		t.Fatal("expected the observer error to be recorded")
	}

	// A stale List(B) must neither roll rows back nor clear the warning.
	rt.requestReload(ctx)
	rt.drainCatalog(ctx)
	names := rowNames(rt.State.Rows)
	if !contains(names, "C") || contains(names, "B") {
		t.Fatalf("List must neither rollback rows nor clear the warning: %v", names)
	}
	if rt.State.Warnings[session.ProviderChatGPT] == nil {
		t.Fatal("warning must remain after a stale List")
	}

	chatgpt.updates <- sessionctl.ProviderUpdate{Sessions: []session.Session{dRow}}
	rt.drainObserver(t)
	names = rowNames(rt.State.Rows)
	if !contains(names, "D") || contains(names, "C") {
		t.Fatalf("expected the later Observer success to replace rows: %v", names)
	}
	if rt.State.Warnings[session.ProviderChatGPT] != nil {
		t.Fatalf("expected the later Observer success to clear the warning: %v", rt.State.Warnings[session.ProviderChatGPT])
	}
}

// TestObserverEmptySuccessNotRepopulatedByStaleList fixes the nil/empty
// contract's interaction with row authority: a successful, legitimately
// empty Observer snapshot is still authoritative -- a later stale,
// non-empty List result must not repopulate it.
func TestObserverEmptySuccessNotRepopulatedByStaleList(t *testing.T) {
	bRow := session.Session{Key: session.Key{Provider: session.ProviderChatGPT, ID: "b"}, Name: "B", CWD: "/work"}
	fp := &fakeProvider{id: session.ProviderChatGPT, rows: []session.Session{bRow}}
	chatgpt := newObserverFakeProvider(fp)
	rt := &Runtime{Controller: sessionctl.Controller{Providers: []sessionctl.Source{chatgpt}, Pins: &fakePins{}}, State: NewState(), CWD: "/work"}

	ctx := context.Background()
	rt.observerCh = rt.Controller.Observe(ctx)
	// See TestObserverErrorDoesNotBlockBootstrapList's comment: no
	// requestReload has run yet to lazily initialize these.
	rt.providerSnapshots = map[session.ProviderID]providerState{}
	rt.currentScope = session.Scope{CurrentDirectory: "/work"}

	chatgpt.updates <- sessionctl.ProviderUpdate{Sessions: []session.Session{}}
	rt.drainObserver(t)
	if len(rt.State.Rows) != 0 {
		t.Fatalf("rows=%v", rowNames(rt.State.Rows))
	}

	rt.requestReload(ctx)
	rt.drainCatalog(ctx)
	if len(rt.State.Rows) != 0 {
		t.Fatalf("a stale non-empty List must not repopulate an authoritative empty Observer snapshot: %v", rowNames(rt.State.Rows))
	}
}

// TestScopeChangeReFiltersObserverOwnedRowsWithoutListReplacement fixes
// that scope changes keep working once Observer owns a provider's rows:
// recomputeRows always re-runs MergeSessions/Filter against
// r.currentScope over whatever providerSnapshots currently holds, so a
// scope change re-filters Observer-owned rows correctly without any List
// row replacement being necessary.
func TestScopeChangeReFiltersObserverOwnedRowsWithoutListReplacement(t *testing.T) {
	cRow := session.Session{Key: session.Key{Provider: session.ProviderChatGPT, ID: "c"}, Name: "C", CWD: "/elsewhere"}
	fp := &fakeProvider{id: session.ProviderChatGPT} // List never itself returns C
	chatgpt := newObserverFakeProvider(fp)
	rt := &Runtime{Controller: sessionctl.Controller{Providers: []sessionctl.Source{chatgpt}, Pins: &fakePins{}}, State: NewState(), CWD: "/work"}

	ctx := context.Background()
	rt.observerCh = rt.Controller.Observe(ctx)
	rt.requestReload(ctx) // ScopeSame (default)
	rt.drainCatalog(ctx)

	chatgpt.updates <- sessionctl.ProviderUpdate{Sessions: []session.Session{cRow}}
	rt.drainObserver(t)
	if contains(rowNames(rt.State.Rows), "C") {
		t.Fatalf("C (CWD=/elsewhere) must be filtered out under ScopeSame (/work): %v", rowNames(rt.State.Rows))
	}

	rt.State.Scope = session.ScopeAll
	rt.requestReload(ctx)
	rt.drainCatalog(ctx)
	if !contains(rowNames(rt.State.Rows), "C") {
		t.Fatalf("C must reappear once scope widens to ScopeAll, purely via re-filtering (List returned nothing new): %v", rowNames(rt.State.Rows))
	}
}

// listStatusObserverProvider is a Codex-like Observer provider whose List
// is a native, fresh read: it states ListOwnsStatus through
// sessionctl.ListStatusAuthority, and its List can be made to fail.
type listStatusObserverProvider struct {
	*observerFakeProvider
	err error
}

func (p *listStatusObserverProvider) ListOwnsStatus() bool { return true }

func (p *listStatusObserverProvider) List(ctx context.Context, archived bool) ([]session.Session, error) {
	if p.err != nil {
		return nil, p.err
	}
	return p.observerFakeProvider.List(ctx, archived)
}

// TestListOwningStatusRecoversWarningBeforeObserverAuthority fixes that,
// while the Observer has not published successfully yet, a successful List
// from a provider stating ListOwnsStatus both refreshes the rows and clears
// the warning an earlier List failure left -- the warning must not stick
// until the Observer first connects.
func TestListOwningStatusRecoversWarningBeforeObserverAuthority(t *testing.T) {
	aRow := session.Session{Key: session.Key{Provider: session.ProviderCodex, ID: "a"}, Name: "A", CWD: "/work"}
	fp := &fakeProvider{id: session.ProviderCodex, rows: []session.Session{aRow}}
	codex := &listStatusObserverProvider{observerFakeProvider: newObserverFakeProvider(fp), err: errors.New("temporary error")}
	rt := &Runtime{Controller: sessionctl.Controller{Providers: []sessionctl.Source{codex}, Pins: &fakePins{}}, State: NewState(), CWD: "/work"}
	ctx := context.Background()
	rt.observerCh = rt.Controller.Observe(ctx)

	rt.requestReload(ctx)
	rt.drainCatalog(ctx)
	if rt.State.Warnings[session.ProviderCodex] == nil {
		t.Fatal("expected the List failure to surface as a warning")
	}

	codex.err = nil
	rt.requestReload(ctx)
	rt.drainCatalog(ctx)
	if w := rt.State.Warnings[session.ProviderCodex]; w != nil {
		t.Fatalf("a successful List owning status must clear the warning before Observer authority: %v", w)
	}
	if !contains(rowNames(rt.State.Rows), "A") {
		t.Fatalf("rows=%v", rowNames(rt.State.Rows))
	}
}

// TestCodexInitialObserverFailurePreservesListRowsAndAuthority fixes the
// Codex startup case: daemon Ensure can fail after List has already supplied
// rows, and its error-only Observer update must add a warning without taking
// row authority or replacing those rows.
func TestCodexInitialObserverFailurePreservesListRowsAndAuthority(t *testing.T) {
	aRow := session.Session{Key: session.Key{Provider: session.ProviderCodex, ID: "a"}, Name: "A", CWD: "/work"}
	bRow := session.Session{Key: session.Key{Provider: session.ProviderCodex, ID: "b"}, Name: "B", CWD: "/work"}
	fp := &fakeProvider{id: session.ProviderCodex, rows: []session.Session{aRow}}
	codex := &listStatusObserverProvider{observerFakeProvider: newObserverFakeProvider(fp)}
	rt := &Runtime{Controller: sessionctl.Controller{Providers: []sessionctl.Source{codex}, Pins: &fakePins{}}, State: NewState(), CWD: "/work"}
	ctx := context.Background()
	rt.observerCh = rt.Controller.Observe(ctx)
	rt.requestReload(ctx)
	rt.drainCatalog(ctx)

	codex.updates <- sessionctl.ProviderUpdate{Err: errors.New("ensure codex app-server daemon: unavailable")}
	rt.drainObserver(t)
	if !contains(rowNames(rt.State.Rows), "A") {
		t.Fatalf("List rows must survive the initial Ensure failure: %v", rowNames(rt.State.Rows))
	}
	if rt.State.Warnings[session.ProviderCodex] == nil {
		t.Fatal("Ensure failure must be visible as a warning")
	}
	if rt.providerSnapshots[session.ProviderCodex].observerSnapshotSeen {
		t.Fatal("an error-only update must not transfer row authority")
	}
	if !rt.providerSnapshots[session.ProviderCodex].observerStatusSeen {
		t.Fatal("an error-only update must transfer warning/status authority")
	}

	// A fresh short-lived app-server List still owns and refreshes rows, but
	// cannot prove that the shared daemon recovered after Observer reported it
	// unavailable.
	rt.requestReload(ctx)
	rt.drainCatalog(ctx)
	if !contains(rowNames(rt.State.Rows), "A") {
		t.Fatalf("List rows must remain usable after the Ensure failure: %v", rowNames(rt.State.Rows))
	}
	if rt.State.Warnings[session.ProviderCodex] == nil {
		t.Fatal("successful List must not clear an Observer-owned daemon warning")
	}
	if rt.providerSnapshots[session.ProviderCodex].observerSnapshotSeen {
		t.Fatal("List must not transfer row authority to Observer")
	}
	if !rt.providerSnapshots[session.ProviderCodex].observerStatusSeen {
		t.Fatal("List must not reset Observer warning/status authority")
	}

	codex.updates <- sessionctl.ProviderUpdate{Sessions: []session.Session{bRow}}
	rt.drainObserver(t)
	names := rowNames(rt.State.Rows)
	if !contains(names, "B") || contains(names, "A") {
		t.Fatalf("Observer recovery must replace List rows: %v", names)
	}
	if warning := rt.State.Warnings[session.ProviderCodex]; warning != nil {
		t.Fatalf("Observer recovery must clear the warning: %v", warning)
	}
	st := rt.providerSnapshots[session.ProviderCodex]
	if !st.observerSnapshotSeen || !st.observerStatusSeen {
		t.Fatalf("Observer recovery authorities=%+v", st)
	}
}

// TestListOwningStatusNeverTakesRowsBackFromObserver fixes that
// ListOwnsStatus is only about warnings: after an Observer success, a
// successful List -- even one owning status -- neither rolls rows back
// nor clears the Observer's warning, on repeated reloads.
func TestListOwningStatusNeverTakesRowsBackFromObserver(t *testing.T) {
	bRow := session.Session{Key: session.Key{Provider: session.ProviderCodex, ID: "b"}, Name: "B", CWD: "/work"}
	cRow := session.Session{Key: session.Key{Provider: session.ProviderCodex, ID: "c"}, Name: "C", CWD: "/work"}
	fp := &fakeProvider{id: session.ProviderCodex, rows: []session.Session{bRow}}
	codex := &listStatusObserverProvider{observerFakeProvider: newObserverFakeProvider(fp)}
	rt := &Runtime{Controller: sessionctl.Controller{Providers: []sessionctl.Source{codex}, Pins: &fakePins{}}, State: NewState(), CWD: "/work"}
	ctx := context.Background()
	rt.observerCh = rt.Controller.Observe(ctx)
	rt.requestReload(ctx)
	rt.drainCatalog(ctx)

	warning := errors.New("observer warning")
	codex.updates <- sessionctl.ProviderUpdate{Sessions: []session.Session{cRow}, Warning: warning}
	rt.drainObserver(t)

	for range 2 {
		rt.requestReload(ctx)
		rt.drainCatalog(ctx)
		names := rowNames(rt.State.Rows)
		if !contains(names, "C") || contains(names, "B") {
			t.Fatalf("a List owning status must not roll back Observer-owned rows: %v", names)
		}
		if rt.State.Warnings[session.ProviderCodex] == nil {
			t.Fatal("a List success must not clear the warning once Observer owns the provider")
		}
		if !rt.providerSnapshots[session.ProviderCodex].observerSnapshotSeen {
			t.Fatal("List success must never reset observerSnapshotSeen")
		}
	}

	// A List failure still surfaces, even with Observer authority.
	codex.err = errors.New("list failed")
	rt.requestReload(ctx)
	rt.drainCatalog(ctx)
	if w := rt.State.Warnings[session.ProviderCodex]; w == nil || w.Error() != "list failed" {
		t.Fatalf("warning=%v", w)
	}
}
