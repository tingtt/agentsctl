//go:build darwin || linux

package agentview

import (
	"context"
	"time"

	"github.com/tingtt/agentsctl/internal/session"
	"github.com/tingtt/agentsctl/internal/sessionctl"
)

// Transient refresh: a provider can list a session that is still becoming
// something else -- a Codex run listed as Starting until a later List binds
// it to its real thread. Nothing in that provider changes the catalog by
// itself, so a catalog loaded once would show the Starting row until the
// user asked for another reload. While any provider lists an
// ActivityStarting session, Agent View therefore re-Lists just that
// provider on a timer, feeding the result through the same providerSnapshots
// pipeline a reload uses.
//
// This is deliberately not a reload: it neither lists the other providers
// nor requests any Refresher/Observer background refresh (a full reload
// does both), and it stops by itself once no provider lists a transient
// session. What makes the session settle -- reconciliation, a native rename
// -- stays inside the provider's List; Agent View only knows that a Starting
// session means "ask again shortly".
const (
	defaultTransientRefreshInterval = time.Second
	// maxTransientRefreshes bounds how many timer rounds follow one reload,
	// so a session that never settles (e.g. its run can never be bound)
	// cannot keep re-Listing its provider forever; the next reload starts a
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
	// inFlight holds the providers with a targeted List still running, so a
	// provider is never Listed twice at once.
	inFlight map[session.ProviderID]bool
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

// providers returns, in configuration order, the providers whose retained
// catalog still lists a Starting session and that have no targeted List
// running.
func (t *transientRefresh) providers(r *Runtime) []session.ProviderID {
	var ids []session.ProviderID
	for _, p := range r.Controller.Providers {
		id := p.ID()
		if t.inFlight[id] {
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
// a Starting session, in the background under ctx.
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
		t.inFlight = make(map[session.ProviderID]bool)
	}
	gen, results := r.catalogGen, t.results
	for _, id := range ids {
		t.inFlight[id] = true
		go func() {
			res := transientResult{gen: gen, ps: r.Controller.LoadProvider(ctx, id)}
			select {
			case results <- res:
			case <-ctx.Done():
			}
		}()
	}
}

// apply installs one targeted List through the same pipeline as a reload's
// arrival (applyLoadSnapshot, then recomputeRows), so selection, pins,
// scope, ordering, identity transitions and last-known-good rows on error
// all behave exactly as they do there, then arms the next round if a
// transient session remains. A result started before the latest reload is
// dropped: that reload lists the provider itself. It reports whether State
// changed.
func (t *transientRefresh) apply(r *Runtime, res transientResult) bool {
	delete(t.inFlight, res.ps.Provider)
	changed := false
	if res.gen == r.catalogGen {
		r.applyLoadSnapshot(res.ps.Provider, res.ps.Sessions, res.ps.Err, res.ps.ListOwnsStatus)
		r.recomputeRows()
		changed = true
	}
	t.schedule(r)
	return changed
}
