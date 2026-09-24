package codex

import (
	"context"
	"fmt"
	"os"
	"sync"

	"github.com/tingtt/agentsctl/internal/localstate"
	"github.com/tingtt/agentsctl/internal/session"
	"github.com/tingtt/agentsctl/internal/sessionctl"
)

// observerHub is the Provider's Observer state: the runtime it observes
// through and the live subscriptions. The runtime connection runs while at
// least one subscription does.
type observerHub struct {
	mu   sync.Mutex
	rt   *codexRuntime
	subs map[chan sessionctl.ProviderUpdate]struct{}
	// stop ends the running lifecycle; nil while none runs. stopped closes
	// once that lifecycle's goroutines have exited.
	stop    context.CancelFunc
	stopped chan struct{}
}

// runtime returns the Provider's codexRuntime, creating it on first use.
// It exists independently of any subscription so List can keep its
// catalog current (see syncObservedCatalog).
func (p *Provider) runtime() *codexRuntime {
	p.obs.mu.Lock()
	defer p.obs.mu.Unlock()
	if p.obs.rt == nil {
		p.obs.rt = newCodexRuntime(p.controlSocket)
	}
	return p.obs.rt
}

func (p *Provider) controlSocket() (string, error) {
	if p.ControlSocket != "" {
		return p.ControlSocket, nil
	}
	home, err := resolveCodexHome(os.Getenv, os.UserHomeDir)
	if err != nil {
		return "", err
	}
	return controlSocketPath(home), nil
}

// Observe implements sessionctl.Observer from the shared Codex app-server
// daemon: every publication is the full Codex catalog, rebuilt from the
// latest catalog and native status the runtime holds, never just the thread
// that changed. Publication is latest-wins; a slow consumer only ever sees
// the newest state.
//
// Nothing is published until the runtime has connected once. Until then
// List keeps owning the rows in Agent View (a first successful publication
// transfers that authority for good), so a missing daemon leaves the
// existing List-based catalog untouched. Once connected, losing the
// connection publishes the full catalog with every thread Unknown plus a
// Warning, instead of leaving the last observed Activity standing;
// reconnecting publishes the rebuilt state and clears the Warning.
func (p *Provider) Observe(ctx context.Context) <-chan sessionctl.ProviderUpdate {
	rt := p.runtime()
	ch := make(chan sessionctl.ProviderUpdate, 1)
	h := &p.obs
	h.mu.Lock()
	if h.subs == nil {
		h.subs = map[chan sessionctl.ProviderUpdate]struct{}{}
	}
	h.subs[ch] = struct{}{}
	if h.stop == nil {
		lifeCtx, cancel := context.WithCancel(context.Background())
		previous, stopped := h.stopped, make(chan struct{})
		h.stop, h.stopped = cancel, stopped
		go p.observeLoop(lifeCtx, rt, previous, stopped)
	}
	h.mu.Unlock()

	go func() {
		<-ctx.Done()
		h.mu.Lock()
		defer h.mu.Unlock()
		delete(h.subs, ch)
		close(ch)
		if len(h.subs) == 0 && h.stop != nil {
			h.stop()
			h.stop = nil
		}
	}()
	return ch
}

// observeLoop runs the runtime connection and publishes on every change
// until lifeCtx ends. It first waits for the previous lifecycle (if any) to
// have stopped, so two connections never feed one runtime.
func (p *Provider) observeLoop(lifeCtx context.Context, rt *codexRuntime, previous <-chan struct{}, stopped chan struct{}) {
	defer close(stopped)
	if previous != nil {
		<-previous
	}
	rt.startLifecycle()
	done := make(chan struct{})
	go func() {
		defer close(done)
		rt.run(lifeCtx)
	}()
	for {
		select {
		case <-lifeCtx.Done():
			<-done
			return
		case <-rt.changed:
			p.publishObserved(rt)
		}
	}
}

// publishObserved builds and publishes the full snapshot, once the runtime
// has earned Observer authority (see Observe).
func (p *Provider) publishObserved(rt *codexRuntime) {
	view := rt.view()
	if !view.everLive {
		return
	}
	update := sessionctl.ProviderUpdate{Sessions: p.observedSessions(view)}
	if !view.live {
		update.Warning = fmt.Errorf("codex app-server unavailable; activity unknown: %w", view.lastErr)
	}
	p.obs.mu.Lock()
	defer p.obs.mu.Unlock()
	for ch := range p.obs.subs {
		sendLatest(ch, update)
	}
}

// observedSessions builds the full Codex catalog from view, merged with the
// existing managed-run state exactly as List merges it (see sessionRows),
// without List's reconciliation side effects.
func (p *Provider) observedSessions(view runtimeView) []session.Session {
	var runs map[string]localstate.Run
	if p.Store != nil {
		runs, _ = p.Store.Runs()
	}
	return p.sessionRows(view.catalog, runs, false, view.observe)
}

// syncObservedCatalog installs a catalog List fetched (fetch numbered by
// beginCatalogFetch) and republishes if it changed, so rows only the
// existing execution path produces reach the Observer's snapshots.
func (p *Provider) syncObservedCatalog(fetch uint64, threads []Thread) {
	rt := p.runtime()
	rt.installCatalog(fetch, threads)
	// Managed-run state may have changed even when the catalog did not.
	rt.kick()
}

// sendLatest delivers update on a one-slot channel, replacing an unread
// older update rather than blocking.
func sendLatest(ch chan sessionctl.ProviderUpdate, update sessionctl.ProviderUpdate) {
	for {
		select {
		case ch <- update:
			return
		default:
		}
		select {
		case <-ch:
		default:
		}
	}
}
