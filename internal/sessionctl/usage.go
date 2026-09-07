package sessionctl

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/tingtt/agentsctl/internal/session"
)

// UsageWindow is one rate-limit window's utilization -- e.g. Claude/Codex's
// 5h or weekly quota. Available distinguishes "reported 0% used" from
// "not reported at all": a provider that cannot report a window must never
// be rendered as if it reported 0%.
type UsageWindow struct {
	Available bool
	Percent   int
	Reset     time.Time
}

// Usage is one provider's account-level utilization, independent of any
// single session -- FiveHour and Weekly mirror Claude/Codex's own rate-
// limit windows (see #14's Composer footer section).
type Usage struct {
	Provider session.ProviderID
	FiveHour UsageWindow
	Weekly   UsageWindow
}

// UsageSource is an optional provider capability (see the DesignDoc's
// capability-composition principle): a provider that does not implement it
// -- any future provider, e.g. Issue #6's ChatGPT -- simply never
// contributes a Usage row. Controller.Usage never requires it, the same
// way Controller.Load's actionsFor never requires Stopper/Renamer/
// Archiver from a Source-only provider.
type UsageSource interface {
	Usage(ctx context.Context) (Usage, error)
}

// Usage collects account-level utilization from every configured provider
// that implements UsageSource, concurrently -- mirroring Load's Catalog
// loading section, so one provider's latency is never added to another's.
// A provider that doesn't implement UsageSource, or whose Usage call
// fails, is simply omitted from the result: a partial usage failure must
// never fail the whole call, matching Load's partial-provider-failure
// principle for session catalogs. The result is sorted by ProviderID for a
// deterministic render order (claude, then codex).
func (c Controller) Usage(ctx context.Context) []Usage {
	var wg sync.WaitGroup
	var mu sync.Mutex
	var result []Usage
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
