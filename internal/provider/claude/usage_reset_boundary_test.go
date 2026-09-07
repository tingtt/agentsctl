//go:build darwin || linux

package claude

import (
	"testing"
	"time"

	"github.com/tingtt/agentsctl/internal/session"
)

// TestToSessionUsageWindowIsAPureShapeConversion fixes that
// toSessionUsageWindow carries no reset-boundary judgment of its own --
// see toSessionUsage, which applies session.Usage.At (the provider-
// neutral rule tested exhaustively in internal/session) right after. This
// just confirms the field-by-field mapping.
func TestToSessionUsageWindowIsAPureShapeConversion(t *testing.T) {
	reset := time.Date(2026, 9, 7, 15, 0, 0, 0, time.UTC)
	got := toSessionUsageWindow(usageWindowSnapshot{State: session.UsageAvailable, Percent: 92, ResetAt: reset})
	want := session.UsageWindow{State: session.UsageAvailable, Percent: 92, Reset: reset}
	if got != want {
		t.Fatalf("got=%+v, want %+v", got, want)
	}
}

// TestToSessionUsageAppliesResetBoundaryViaSessionUsageAt fixes Issue
// #19's core worked example end to end through toSessionUsage (not just
// session.Usage.At in isolation): a cached 5h reading whose Reset has
// passed renders as Unknown, while weekly's still-valid cached reading in
// the same snapshot is unaffected -- proving toSessionUsage actually
// delegates to session.Usage.At rather than reimplementing (or dropping)
// the rule.
func TestToSessionUsageAppliesResetBoundaryViaSessionUsageAt(t *testing.T) {
	reset := time.Date(2026, 9, 7, 15, 0, 0, 0, time.UTC)
	now := reset.Add(1 * time.Minute)
	snap := usageSnapshot{
		FiveHour: usageWindowSnapshot{State: session.UsageAvailable, Percent: 92, ResetAt: reset},
		Weekly:   usageWindowSnapshot{State: session.UsageAvailable, Percent: 84, ResetAt: time.Date(2026, 9, 13, 5, 0, 0, 0, time.UTC)},
	}
	got := toSessionUsage(snap, now)
	if got.FiveHour.State != session.UsageUnknown {
		t.Fatalf("FiveHour=%+v, want Unknown once its own reset has passed", got.FiveHour)
	}
	if got.Weekly.State != session.UsageAvailable || got.Weekly.Percent != 84 {
		t.Fatalf("Weekly=%+v, want the still-valid 84%% reading unaffected by 5h's own reset crossing", got.Weekly)
	}
}

// TestToSessionUsageExhaustedAfterResetBecomesUnknown fixes that an
// exhausted window carried forward with a known Reset (see
// exhaustedWindowSnapshot) also expires through toSessionUsage once that
// Reset has passed.
func TestToSessionUsageExhaustedAfterResetBecomesUnknown(t *testing.T) {
	reset := time.Date(2026, 9, 7, 15, 0, 0, 0, time.UTC)
	now := reset.Add(1 * time.Minute)
	snap := usageSnapshot{FiveHour: usageWindowSnapshot{State: session.UsageExhausted, Percent: 100, ResetAt: reset}}
	got := toSessionUsage(snap, now)
	if got.FiveHour.State != session.UsageUnknown {
		t.Fatalf("FiveHour=%+v, want Unknown -- exhausted must not survive its own known reset boundary", got.FiveHour)
	}
}
