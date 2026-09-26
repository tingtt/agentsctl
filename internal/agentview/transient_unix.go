//go:build darwin || linux

package agentview

import (
	"context"
	"time"

	"github.com/tingtt/agentsctl/internal/session"
	"github.com/tingtt/agentsctl/internal/sessionctl"
)

// Transient refresh: a provider can list a session that is still becoming
// something else -- e.g. the Starting row a Claude Dispatch returns, until
// a later List reports the session's native state. Nothing in that provider
// changes the catalog by itself, so a catalog loaded once would show the
// Starting row until the user asked for another reload. While any provider lists an
// ActivityStarting session, Agent View therefore re-Lists just that
// provider on a timer, feeding the result through the same providerSnapshots
// pipeline a reload uses.
//
// This is deliberately not a reload: it neither lists the other providers
// nor requests any Refresher/Observer background refresh (a full reload
// does both), and it stops by itself once no provider lists a transient
// session. What makes the session settle stays inside the provider's List; Agent View only knows that a Starting
// session means "ask again shortly".
const (
	defaultTransientRefreshInterval = time.Second
	// maxTransientRefreshes bounds how many timer rounds follow one reload,
	// so a session that never settles cannot keep re-Listing its provider forever; the next reload starts a
	// new budget.
	maxTransientRefreshes = 180
)

// transientRefresh is Runtime's targeted-refresh state. Only ever touched
// from the eventLoop goroutine (its goroutines just carry results back
// through results).
type transientRefresh struct {
	// interval is the wait before each round; zero means the default.
	interval time.Duration
	// after returns a channel that fires once d has elapsed. Nil means
	// time.After; tests replace it to drive rounds deterministically.
	after func(d time.Duration) <-chan time.Time

	// tick is non-nil exactly while a round is scheduled.
	tick <-chan time.Time
	// results receives each targeted List's outcome; lazily created.
	results chan transientResult
	// inFlight holds the providers with a targeted List still running, each
	// with the reload generation (Runtime.catalogGen) that started it, so a
	// provider is never targeted twice at once and only the result of the
	// List that owns an entry can release it.
	inFlight map[session.ProviderID]int
	// cancel ends the context every targeted List of the current reload
	// generation runs under (a child of the event loop's); nil while none
	// has started. supersede cancels it.
	cancel   context.CancelFunc
	roundCtx context.Context
	// attempts counts the rounds since the last reload (see
	// maxTransientRefreshes).
	attempts int
}

// transientResult is one targeted List's snapshot, stamped with the reload
// generation it was started under.
type transientResult struct {
	gen int
	ps  sessionctl.ProviderSnapshot
}

// supersede ends everything targeted refresh started so far. A reload is
// about to List every provider itself and is authoritative, so a targeted
// List begun earlier is obsolete: it is cancelled, its in-flight entry is
// released (a List that never returns must not stop later rounds from
// being scheduled), any pending round is dropped and the round budget starts
// over. Whatever such a List still returns is discarded by apply, which
// recognizes it by its older generation and touches nothing.
func (t *transientRefresh) supersede() {
	if t.cancel != nil {
		t.cancel()
		t.cancel = nil
	}
	t.inFlight = nil
	t.tick = nil
	t.attempts = 0
}

// providers returns, in configuration order, the providers whose retained
// catalog still lists a Starting session and that have no targeted List
// running.
func (t *transientRefresh) providers(r *Runtime) []session.ProviderID {
	var ids []session.ProviderID
	for _, p := range r.Controller.Providers {
		id := p.ID()
		if _, running := t.inFlight[id]; running {
			continue
		}
		for _, s := range r.providerSnapshots[id].sessions {
			if s.Activity == session.ActivityStarting {
				ids = append(ids, id)
				break
			}
		}
	}
	return ids
}

// schedule arms the next round if one is needed and none is pending. It is
// not armed while a reload cycle is in flight (that cycle's own arrivals
// end with another schedule call) or once the budget is spent, and a
// provider whose targeted List is still running re-arms it on its own
// result.
func (t *transientRefresh) schedule(r *Runtime) {
	if t.tick != nil || r.State.CatalogLoading || t.attempts >= maxTransientRefreshes {
		return
	}
	if len(t.providers(r)) == 0 {
		return
	}
	interval, after := t.interval, t.after
	if interval <= 0 {
		interval = defaultTransientRefreshInterval
	}
	if after == nil {
		after = time.After
	}
	t.tick = after(interval)
}

// refresh runs one round: a targeted List of each provider that still lists
// a Starting session, in the background. The Lists run under a context
// derived from ctx (the event loop's), so they end with the loop and, via
// supersede, with the next reload.
func (t *transientRefresh) refresh(ctx context.Context, r *Runtime) {
	t.tick = nil
	if r.State.CatalogLoading {
		return
	}
	ids := t.providers(r)
	if len(ids) == 0 {
		return
	}
	t.attempts++
	if t.results == nil {
		t.results = make(chan transientResult, len(r.Controller.Providers))
	}
	if t.inFlight == nil {
		t.inFlight = make(map[session.ProviderID]int)
	}
	if t.cancel == nil {
		t.roundCtx, t.cancel = context.WithCancel(ctx)
	}
	roundCtx, gen, results := t.roundCtx, r.catalogGen, t.results
	for _, id := range ids {
		t.inFlight[id] = gen
		go func() {
			res := transientResult{gen: gen, ps: r.Controller.LoadProvider(roundCtx, id)}
			select {
			case results <- res:
			case <-roundCtx.Done():
			}
		}()
	}
}

// apply installs one targeted List through the same pipeline as a reload's
// arrival (applyLoadSnapshot, then recomputeRows), so selection, pins,
// scope, ordering, identity transitions and last-known-good rows on error
// all behave exactly as they do there, then arms the next round if a
// transient session remains. A result from before the latest reload was
// superseded (see supersede): it is dropped without touching anything --
// not the rows, not the in-flight entry a newer List may now own, not the
// timer -- since that reload lists the provider itself and re-arms the
// refresh when it completes. It reports whether State changed.
func (t *transientRefresh) apply(r *Runtime, res transientResult) bool {
	if res.gen != r.catalogGen {
		return false
	}
	if t.inFlight[res.ps.Provider] == res.gen {
		delete(t.inFlight, res.ps.Provider)
	}
	r.applyLoadSnapshot(res.ps.Provider, res.ps.Sessions, res.ps.Err)
	r.recomputeRows()
	t.schedule(r)
	return true
}
