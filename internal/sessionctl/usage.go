package sessionctl

import (
	"context"
	"sort"
	"sync"

	"github.com/tingtt/agentsctl/internal/session"
)

// UsageSource is an optional provider capability (see the DesignDoc's
// capability-composition principle): a provider that does not implement it
// -- any future provider, e.g. Issue #6's ChatGPT, or a provider for which
// no stable usage interface exists -- simply never contributes a
// session.Usage row. Controller.Usage never requires it, the same way
// Controller.Load's actionsFor never requires Stopper/Renamer/Archiver
// from a Source-only provider.
type UsageSource interface {
	Usage(ctx context.Context) (session.Usage, error)
}

// Usage collects account-level utilization from every configured provider
// that implements UsageSource, concurrently -- mirroring Load's Catalog
// loading section, so one provider's latency is never added to another's.
// A provider that doesn't implement UsageSource, or whose Usage call
// fails, is simply omitted from the result: a partial usage failure must
// never fail the whole call, matching Load's partial-provider-failure
// principle for session catalogs. The result is sorted by ProviderID for a
// deterministic render order (claude, then codex).
func (c Controller) Usage(ctx context.Context) []session.Usage {
	var wg sync.WaitGroup
	var mu sync.Mutex
	var result []session.Usage
	for _, p := range c.Providers {
		src, ok := p.(UsageSource)
		if !ok {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			u, err := src.Usage(ctx)
			if err != nil {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			result = append(result, u)
		}()
	}
	wg.Wait()
	sort.Slice(result, func(i, j int) bool { return result[i].Provider < result[j].Provider })
	return result
}

// UsageUpdate is one provider's incremental usage result from UsageStream.
type UsageUpdate struct {
	Provider session.ProviderID
	Usage    session.Usage
	Err      error
}

// UsageStream is Usage's incremental counterpart: it starts the same
// concurrent per-provider fetch, but returns a channel that delivers each
// provider's UsageUpdate the moment that provider's own call completes,
// rather than collecting every result into one batch behind the slowest
// provider (see Usage's doc comment). This is what lets a caller (Agent
// View's event loop) show a fast provider's usage without waiting on a
// slow or hung one -- latency isolation applied to the caller side, not
// just to the concurrent fetch itself.
//
// The channel is closed once every provider that implements UsageSource
// has sent its result (or immediately, already closed, if none do) -- a
// caller can safely range over it to know when the cycle is complete,
// though Agent View's event loop does not wait for that and instead
// applies updates as they arrive.
func (c Controller) UsageStream(ctx context.Context) <-chan UsageUpdate {
	out := make(chan UsageUpdate)
	var wg sync.WaitGroup
	for _, p := range c.Providers {
		src, ok := p.(UsageSource)
		if !ok {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			u, err := src.Usage(ctx)
			out <- UsageUpdate{Provider: p.ID(), Usage: u, Err: err}
		}()
	}
	go func() {
		wg.Wait()
		close(out)
	}()
	return out
}
