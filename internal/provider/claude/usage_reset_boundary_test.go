//go:build darwin || linux

package claude

import (
	"testing"
	"time"

	"github.com/tingtt/agentsctl/internal/session"
)

// TestToSessionUsageWindowAvailableBeforeResetStaysAvailable fixes the
// baseline: a cached Available reading whose Reset time is still in the
// future is used as-is.
func TestToSessionUsageWindowAvailableBeforeResetStaysAvailable(t *testing.T) {
	now := time.Date(2026, 9, 7, 15, 0, 0, 0, time.UTC)
	reset := now.Add(1 * time.Hour)
	w := usageWindowSnapshot{State: session.UsageAvailable, Percent: 92, ResetAt: reset}
	got := toSessionUsageWindow(w, now)
	if got.State != session.UsageAvailable || got.Percent != 92 {
		t.Fatalf("got=%+v, want Available/92%% while now is before Reset", got)
	}
}

// TestToSessionUsageWindowAvailableAfterResetBecomesUnknown fixes Issue
// #19's core reset-boundary scenario:
//
//	5h: 92%, reset = T
//	now > T, new snapshot unavailable
//
// The cached 92% must not be shown as the current 5h usage once now has
// passed T -- it becomes UsageUnknown ("?%"), never a stale percentage
// masquerading as current.
func TestToSessionUsageWindowAvailableAfterResetBecomesUnknown(t *testing.T) {
	reset := time.Date(2026, 9, 7, 15, 0, 0, 0, time.UTC)
	now := reset.Add(1 * time.Minute)
	w := usageWindowSnapshot{State: session.UsageAvailable, Percent: 92, ResetAt: reset}
	got := toSessionUsageWindow(w, now)
	if got.State != session.UsageUnknown {
		t.Fatalf("got=%+v, want UsageUnknown once now has passed the cached Reset time", got)
	}
	if got.Percent == 92 {
		t.Fatalf("got=%+v, must not carry the stale 92%% forward as if it were current", got)
	}
}

// TestToSessionUsageWindowAtExactResetInstantBecomesUnknown fixes the
// boundary itself (now == Reset, not just now > Reset): the cached
// reading belongs to the period that just ended, not the new one that
// starts at that same instant.
func TestToSessionUsageWindowAtExactResetInstantBecomesUnknown(t *testing.T) {
	reset := time.Date(2026, 9, 7, 15, 0, 0, 0, time.UTC)
	w := usageWindowSnapshot{State: session.UsageAvailable, Percent: 50, ResetAt: reset}
	got := toSessionUsageWindow(w, reset)
	if got.State != session.UsageUnknown {
		t.Fatalf("got=%+v, want UsageUnknown at the exact reset instant", got)
	}
}

// TestToSessionUsageWindowExhaustedAfterResetBecomesUnknown fixes Issue
// #19's exhausted-reset-crossing scenario:
//
//	before: exhausted / 100%, reset = T
//	after:  now > T, refresh unavailable
//
// A window exhausted before its reset must not still read as exhausted
// once the reset boundary it was carrying a known Reset time for has
// passed.
func TestToSessionUsageWindowExhaustedAfterResetBecomesUnknown(t *testing.T) {
	reset := time.Date(2026, 9, 7, 15, 0, 0, 0, time.UTC)
	now := reset.Add(1 * time.Minute)
	w := usageWindowSnapshot{State: session.UsageExhausted, Percent: 100, ResetAt: reset}
	got := toSessionUsageWindow(w, now)
	if got.State != session.UsageUnknown {
		t.Fatalf("got=%+v, want UsageUnknown -- exhausted must not survive its own known reset boundary", got)
	}
}

// TestToSessionUsageWindowExhaustedWithUnknownResetNeverAutoClears fixes
// that an exhausted window detected with no prior reset time ever cached
// (see exhaustedWindowSnapshot) has nothing to compare "now" against, so
// it is never boundary-invalidated on that basis alone -- it stays
// exhausted until an explicit new refresh outcome overrides it (TTL-
// driven, not reset-boundary-driven).
func TestToSessionUsageWindowExhaustedWithUnknownResetNeverAutoClears(t *testing.T) {
	w := usageWindowSnapshot{State: session.UsageExhausted, Percent: 100}
	got := toSessionUsageWindow(w, time.Now().Add(365*24*time.Hour))
	if got.State != session.UsageExhausted {
		t.Fatalf("got=%+v, want Exhausted to persist with no reset time to compare against", got)
	}
}

// TestToSessionUsageWindowsAreIndependentAcrossReset fixes Issue #19's "5h
// と weekly は独立して評価してください": in one conversion, a 5h window whose
// reset has passed becomes unknown while weekly's still-valid window is
// completely unaffected.
func TestToSessionUsageWindowsAreIndependentAcrossReset(t *testing.T) {
	now := time.Date(2026, 9, 7, 15, 1, 0, 0, time.UTC)
	snap := usageSnapshot{
		FiveHour: usageWindowSnapshot{State: session.UsageAvailable, Percent: 92, ResetAt: time.Date(2026, 9, 7, 15, 0, 0, 0, time.UTC)},
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

// TestToSessionUsageWindowsAreIndependentAcrossResetWeeklySide is the
// mirror of TestToSessionUsageWindowsAreIndependentAcrossReset: weekly's
// reset has passed while 5h's has not, and only weekly becomes unknown.
func TestToSessionUsageWindowsAreIndependentAcrossResetWeeklySide(t *testing.T) {
	now := time.Date(2026, 9, 13, 5, 1, 0, 0, time.UTC)
	snap := usageSnapshot{
		FiveHour: usageWindowSnapshot{State: session.UsageAvailable, Percent: 20, ResetAt: time.Date(2026, 9, 13, 10, 0, 0, 0, time.UTC)},
		Weekly:   usageWindowSnapshot{State: session.UsageAvailable, Percent: 84, ResetAt: time.Date(2026, 9, 13, 5, 0, 0, 0, time.UTC)},
	}
	got := toSessionUsage(snap, now)
	if got.Weekly.State != session.UsageUnknown {
		t.Fatalf("Weekly=%+v, want Unknown once its own reset has passed", got.Weekly)
	}
	if got.FiveHour.State != session.UsageAvailable || got.FiveHour.Percent != 20 {
		t.Fatalf("FiveHour=%+v, want the still-valid 20%% reading unaffected by weekly's own reset crossing", got.FiveHour)
	}
}

// TestToSessionUsageWindowNeverReportedStaysUnknown fixes that a window
// never reported at all (the zero value) stays UsageUnknown regardless of
// now, matching the pre-#19 "not reported" contract.
func TestToSessionUsageWindowNeverReportedStaysUnknown(t *testing.T) {
	got := toSessionUsageWindow(usageWindowSnapshot{}, time.Now())
	if got.State != session.UsageUnknown {
		t.Fatalf("got=%+v, want UsageUnknown for a never-reported window", got)
	}
}
