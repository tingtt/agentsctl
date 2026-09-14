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
// providerSnapshots/applyProviderUpdate/recomputeRows): retaining a
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
				r.applyProviderUpdate(upd.ps.Provider, upd.ps.Sessions, upd.ps.Err, nil)
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
		r.applyProviderUpdate(upd.Provider, upd.Sessions, upd.Err, upd.Warning)
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
// non-fatal-warning half of applyProviderUpdate: a successful Observer
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
	rt := &Runtime{Controller: sessionctl.Controller{Providers: []sessionctl.Source{chatgpt}, Pins: &fakePins{}}, State: NewState(), Input: inR, Output: &syncBuffer{}, CWD: "/work"}

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
