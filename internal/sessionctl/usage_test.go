package sessionctl

import (
	"context"
	"testing"
	"time"

	"github.com/tingtt/agentsctl/internal/session"
)

// fakeUsageSource is a Source that also implements UsageSource -- the
// Claude/Codex shape. Embedding plain fakeSource (not fakeFullProvider)
// keeps this independent of the other session-lifecycle capabilities, so
// these tests can't accidentally depend on them.
type fakeUsageSource struct {
	fakeSource
	usage    session.Usage
	usageErr error
}

func (f fakeUsageSource) Usage(context.Context) (session.Usage, error) { return f.usage, f.usageErr }

// TestControllerUsageSkipsProvidersWithoutTheCapability fixes the
// optional-capability contract (see the DesignDoc's capability-composition
// principle): a Source-only provider -- the architecture-acceptance shape
// for a future partial provider such as Issue #6's ChatGPT -- must not be
// required to implement UsageSource, and simply contributes no Usage row.
func TestControllerUsageSkipsProvidersWithoutTheCapability(t *testing.T) {
	c := Controller{Providers: []Source{fakeSource{id: session.ProviderClaude}}}
	got := c.Usage(context.Background())
	if len(got) != 0 {
		t.Fatalf("Usage()=%+v, want none from a Source-only provider", got)
	}
}

// TestControllerUsageOrdersClaudeBeforeCodex fixes #14's deterministic
// render order regardless of Controller.Providers registration order or
// goroutine completion order.
func TestControllerUsageOrdersClaudeBeforeCodex(t *testing.T) {
	c := Controller{Providers: []Source{
		fakeUsageSource{fakeSource: fakeSource{id: session.ProviderCodex}, usage: session.Usage{Provider: session.ProviderCodex}},
		fakeUsageSource{fakeSource: fakeSource{id: session.ProviderClaude}, usage: session.Usage{Provider: session.ProviderClaude}},
	}}
	got := c.Usage(context.Background())
	if len(got) != 2 || got[0].Provider != session.ProviderClaude || got[1].Provider != session.ProviderCodex {
		t.Fatalf("Usage()=%+v, want [claude codex] regardless of registration order", got)
	}
}

// blockingUsageSource is a Source+UsageSource whose Usage call signals
// entered the instant it's called, then blocks until the test closes
// release (or ctx is cancelled) -- for deterministic (no time.Sleep)
// latency-isolation tests: the test controls exactly when a "slow"
// provider's call is allowed to return, rather than guessing a duration
// that's probably longer than the fast path.
type blockingUsageSource struct {
	fakeSource
	entered chan struct{}
	release <-chan struct{}
	usage   session.Usage
}

func (f blockingUsageSource) Usage(ctx context.Context) (session.Usage, error) {
	close(f.entered)
	select {
	case <-f.release:
	case <-ctx.Done():
		return session.Usage{}, ctx.Err()
	}
	return f.usage, nil
}

// TestUsageStreamDeliversFastProviderWithoutWaitingForSlowOne fixes the
// core latency-isolation guarantee UsageStream exists for: a fast
// provider's UsageUpdate must arrive on the channel while a slower
// provider's own Usage call is still blocked, never gated behind it.
func TestUsageStreamDeliversFastProviderWithoutWaitingForSlowOne(t *testing.T) {
	slowEntered := make(chan struct{})
	slowRelease := make(chan struct{})
	c := Controller{Providers: []Source{
		blockingUsageSource{fakeSource: fakeSource{id: session.ProviderClaude}, entered: slowEntered, release: slowRelease, usage: session.Usage{Provider: session.ProviderClaude}},
		fakeUsageSource{fakeSource: fakeSource{id: session.ProviderCodex}, usage: session.Usage{Provider: session.ProviderCodex, FiveHour: session.UsageWindow{Available: true, Percent: 5}}},
	}}
	stream := c.UsageStream(context.Background())

	<-slowEntered // the slow provider's call has started and is now blocked

	select {
	case upd := <-stream:
		if upd.Provider != session.ProviderCodex || upd.Err != nil {
			t.Fatalf("first update=%+v, want codex's fast result", upd)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("fast provider's update never arrived while the slow one was still blocked")
	}

	// The stream must not be closed yet -- the slow provider hasn't
	// reported (or been released) yet.
	select {
	case upd, ok := <-stream:
		t.Fatalf("stream produced %+v (ok=%v) before the slow provider was released", upd, ok)
	default:
	}

	close(slowRelease)
	select {
	case upd := <-stream:
		if upd.Provider != session.ProviderClaude {
			t.Fatalf("second update=%+v, want claude's now-unblocked result", upd)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("slow provider's update never arrived after release")
	}
	select {
	case _, ok := <-stream:
		if ok {
			t.Fatal("stream must close once every provider has reported")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stream never closed after both providers reported")
	}
}

// TestUsageStreamSkipsProvidersWithoutTheCapability mirrors Usage's own
// optional-capability contract for the incremental stream: a Source-only
// provider contributes no update, and the stream closes immediately.
func TestUsageStreamSkipsProvidersWithoutTheCapability(t *testing.T) {
	c := Controller{Providers: []Source{fakeSource{id: session.ProviderClaude}}}
	stream := c.UsageStream(context.Background())
	select {
	case upd, ok := <-stream:
		if ok {
			t.Fatalf("got update=%+v from a Source-only provider", upd)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stream never closed for a provider with no UsageSource at all")
	}
}

// TestUsageStreamPartialFailureReportsErrForThatProviderOnly fixes that a
// failing provider's UsageUpdate carries its own error without blocking or
// invalidating a different provider's successful update.
func TestUsageStreamPartialFailureReportsErrForThatProviderOnly(t *testing.T) {
	c := Controller{Providers: []Source{
		fakeUsageSource{fakeSource: fakeSource{id: session.ProviderClaude}, usageErr: errBoom},
		fakeUsageSource{fakeSource: fakeSource{id: session.ProviderCodex}, usage: session.Usage{Provider: session.ProviderCodex}},
	}}
	stream := c.UsageStream(context.Background())
	seen := map[session.ProviderID]error{}
	for upd := range stream {
		seen[upd.Provider] = upd.Err
	}
	if len(seen) != 2 {
		t.Fatalf("seen=%+v, want an update from both providers", seen)
	}
	if seen[session.ProviderClaude] == nil {
		t.Fatal("claude's update must carry its error")
	}
	if seen[session.ProviderCodex] != nil {
		t.Fatalf("codex's update must not carry an error: %v", seen[session.ProviderCodex])
	}
}

// TestControllerUsagePartialFailureKeepsOtherProvider fixes the same
// partial-provider-failure principle Load applies to session catalogs: one
// provider's Usage error must not drop another provider's successful
// result.
func TestControllerUsagePartialFailureKeepsOtherProvider(t *testing.T) {
	c := Controller{Providers: []Source{
		fakeUsageSource{fakeSource: fakeSource{id: session.ProviderClaude}, usageErr: errBoom},
		fakeUsageSource{fakeSource: fakeSource{id: session.ProviderCodex}, usage: session.Usage{Provider: session.ProviderCodex, FiveHour: session.UsageWindow{Available: true, Percent: 42, Reset: time.Now()}}},
	}}
	got := c.Usage(context.Background())
	if len(got) != 1 || got[0].Provider != session.ProviderCodex {
		t.Fatalf("Usage()=%+v, want only codex's successful result", got)
	}
}
