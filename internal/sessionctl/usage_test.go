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
	usage    Usage
	usageErr error
}

func (f fakeUsageSource) Usage(context.Context) (Usage, error) { return f.usage, f.usageErr }

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
		fakeUsageSource{fakeSource: fakeSource{id: session.ProviderCodex}, usage: Usage{Provider: session.ProviderCodex}},
		fakeUsageSource{fakeSource: fakeSource{id: session.ProviderClaude}, usage: Usage{Provider: session.ProviderClaude}},
	}}
	got := c.Usage(context.Background())
	if len(got) != 2 || got[0].Provider != session.ProviderClaude || got[1].Provider != session.ProviderCodex {
		t.Fatalf("Usage()=%+v, want [claude codex] regardless of registration order", got)
	}
}

// TestControllerUsagePartialFailureKeepsOtherProvider fixes the same
// partial-provider-failure principle Load applies to session catalogs: one
// provider's Usage error must not drop another provider's successful
// result.
func TestControllerUsagePartialFailureKeepsOtherProvider(t *testing.T) {
	c := Controller{Providers: []Source{
		fakeUsageSource{fakeSource: fakeSource{id: session.ProviderClaude}, usageErr: errBoom},
		fakeUsageSource{fakeSource: fakeSource{id: session.ProviderCodex}, usage: Usage{Provider: session.ProviderCodex, FiveHour: UsageWindow{Available: true, Percent: 42, Reset: time.Now()}}},
	}}
	got := c.Usage(context.Background())
	if len(got) != 1 || got[0].Provider != session.ProviderCodex {
		t.Fatalf("Usage()=%+v, want only codex's successful result", got)
	}
}
