package codex

import (
	"context"
	"errors"
	"fmt"
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
		p.obs.rt = newCodexRuntime(p.readySocket)
	}
	return p.obs.rt
}

func (p *Provider) readySocket(ctx context.Context) (string, error) {
	if p.ControlSocket != "" {
		return p.ControlSocket, nil
	}
	if p.Daemon == nil {
		return "", errors.New("codex daemon lifecycle is not configured")
	}
	info, err := p.Daemon.Ensure(ctx)
	if err != nil {
		return "", fmt.Errorf("ensure codex app-server daemon: %w", err)
	}
	return info.SocketPath, nil
}

// withDaemon runs fn on a connection of its own to the shared app-server
// daemon (see readySocket, connectDaemon). Every Codex thread mutation goes
// through it, so the daemon that holds a thread's writer lock is the one
// that changes it.
func (p *Provider) withDaemon(ctx context.Context, fn func(*rpcConn) error) error {
	socket, err := p.readySocket(ctx)
	if err != nil {
		return err
	}
	return connectDaemon(ctx, socket, fn)
}

// Observe implements sessionctl.Observer from the shared Codex app-server
// daemon: every publication is the full Codex catalog, rebuilt from the
// latest catalog and native status the runtime holds, never just the thread
// that changed. Publication is latest-wins; a slow consumer only ever sees
// the newest state.
//
// A ready-endpoint failure before the runtime has connected is published as
// an error-only update, so List keeps owning the rows while Agent View shows
// the failure. Other connection failures publish nothing until the runtime
// has connected once. A first successful publication transfers row authority
// for good. Once connected, losing the
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
		if view.lastReadyErr {
			p.sendObserved(sessionctl.ProviderUpdate{Err: fmt.Errorf("codex app-server unavailable: %w", view.lastErr)})
		}
		return
	}
	update := sessionctl.ProviderUpdate{Sessions: p.observedSessions(view)}
	if !view.live {
		update.Warning = fmt.Errorf("codex app-server unavailable; activity unknown: %w", view.lastErr)
	}
	p.sendObserved(update)
}

func (p *Provider) sendObserved(update sessionctl.ProviderUpdate) {
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
