package session

import (
	"testing"
	"time"
)

// TestUsageWindowAtBeforeResetStaysAvailable fixes the baseline: a reading
// whose Reset time is still in the future is used as-is.
func TestUsageWindowAtBeforeResetStaysAvailable(t *testing.T) {
	now := time.Date(2026, 9, 7, 15, 0, 0, 0, time.UTC)
	reset := now.Add(1 * time.Hour)
	w := UsageWindow{State: UsageAvailable, Percent: 92, Reset: reset}
	got := w.At(now)
	if got.State != UsageAvailable || got.Percent != 92 {
		t.Fatalf("got=%+v, want Available/92%% while now is before Reset", got)
	}
}

// TestUsageWindowAtAfterResetBecomesUnknown fixes Issue #19's core
// worked example:
//
//	5h: 92%, reset = T
//	now > T, new snapshot unavailable
//
// The cached 92% must not be shown as the current 5h usage once now has
// passed T -- it becomes UsageUnknown ("?%"), never a stale percentage
// masquerading as current. This is the provider-neutral rule any caller
// (a provider's own conversion, Agent View re-rendering an already-
// received value, or a future #20 read) applies via the exact same At
// call -- see UsageWindow.At's doc comment.
func TestUsageWindowAtAfterResetBecomesUnknown(t *testing.T) {
	reset := time.Date(2026, 9, 7, 15, 0, 0, 0, time.UTC)
	now := reset.Add(1 * time.Minute)
	w := UsageWindow{State: UsageAvailable, Percent: 92, Reset: reset}
	got := w.At(now)
	if got.State != UsageUnknown {
		t.Fatalf("got=%+v, want UsageUnknown once now has passed the cached Reset time", got)
	}
	if got.Percent == 92 {
		t.Fatalf("got=%+v, must not carry the stale 92%% forward as if it were current", got)
	}
}

// TestUsageWindowAtExactResetInstantBecomesUnknown fixes the boundary
// itself (now == Reset, not just now > Reset): the cached reading belongs
// to the period that just ended, not the new one starting at that same
// instant.
func TestUsageWindowAtExactResetInstantBecomesUnknown(t *testing.T) {
	reset := time.Date(2026, 9, 7, 15, 0, 0, 0, time.UTC)
	w := UsageWindow{State: UsageAvailable, Percent: 50, Reset: reset}
	got := w.At(reset)
	if got.State != UsageUnknown {
		t.Fatalf("got=%+v, want UsageUnknown at the exact reset instant", got)
	}
}

// TestUsageWindowAtExhaustedAfterResetBecomesUnknown fixes Issue #19's
// exhausted-reset-crossing scenario: a window exhausted before its reset
// must not still read as exhausted once the reset boundary it carried a
// known Reset time for has passed.
func TestUsageWindowAtExhaustedAfterResetBecomesUnknown(t *testing.T) {
	reset := time.Date(2026, 9, 7, 15, 0, 0, 0, time.UTC)
	now := reset.Add(1 * time.Minute)
	w := UsageWindow{State: UsageExhausted, Percent: 100, Reset: reset}
	got := w.At(now)
	if got.State != UsageUnknown {
		t.Fatalf("got=%+v, want UsageUnknown -- exhausted must not survive its own known reset boundary", got)
	}
}

// TestUsageWindowAtExhaustedWithNoResetNeverExpires fixes that an
// exhausted window with no known Reset time has nothing to compare "now"
// against, so it is never boundary-invalidated on that basis alone -- it
// stays exhausted until an explicit new reading overrides it.
func TestUsageWindowAtExhaustedWithNoResetNeverExpires(t *testing.T) {
	w := UsageWindow{State: UsageExhausted, Percent: 100}
	got := w.At(time.Now().Add(365 * 24 * time.Hour))
	if got.State != UsageExhausted {
		t.Fatalf("got=%+v, want Exhausted to persist with no reset time to compare against", got)
	}
}

// TestUsageWindowAtNeverReportedStaysUnknown fixes that a window never
// reported at all (the zero value) stays UsageUnknown regardless of now.
func TestUsageWindowAtNeverReportedStaysUnknown(t *testing.T) {
	got := UsageWindow{}.At(time.Now())
	if got.State != UsageUnknown {
		t.Fatalf("got=%+v, want UsageUnknown for a never-reported window", got)
	}
}

// TestUsageAtWindowsAreIndependentFiveHourSide fixes Issue #19's "5h と
// weekly は独立して評価してください": in one Usage.At call, a 5h window whose
// reset has passed becomes unknown while weekly's still-valid window is
// completely unaffected.
func TestUsageAtWindowsAreIndependentFiveHourSide(t *testing.T) {
	now := time.Date(2026, 9, 7, 15, 1, 0, 0, time.UTC)
	u := Usage{
		Provider: ProviderClaude,
		FiveHour: UsageWindow{State: UsageAvailable, Percent: 92, Reset: time.Date(2026, 9, 7, 15, 0, 0, 0, time.UTC)},
		Weekly:   UsageWindow{State: UsageAvailable, Percent: 84, Reset: time.Date(2026, 9, 13, 5, 0, 0, 0, time.UTC)},
	}
	got := u.At(now)
	if got.FiveHour.State != UsageUnknown {
		t.Fatalf("FiveHour=%+v, want Unknown once its own reset has passed", got.FiveHour)
	}
	if got.Weekly.State != UsageAvailable || got.Weekly.Percent != 84 {
		t.Fatalf("Weekly=%+v, want the still-valid 84%% reading unaffected by 5h's own reset crossing", got.Weekly)
	}
}

// TestUsageAtWindowsAreIndependentWeeklySide is the mirror: weekly's reset
// has passed while 5h's has not, and only weekly becomes unknown.
func TestUsageAtWindowsAreIndependentWeeklySide(t *testing.T) {
	now := time.Date(2026, 9, 13, 5, 1, 0, 0, time.UTC)
	u := Usage{
		Provider: ProviderClaude,
		FiveHour: UsageWindow{State: UsageAvailable, Percent: 20, Reset: time.Date(2026, 9, 13, 10, 0, 0, 0, time.UTC)},
		Weekly:   UsageWindow{State: UsageAvailable, Percent: 84, Reset: time.Date(2026, 9, 13, 5, 0, 0, 0, time.UTC)},
	}
	got := u.At(now)
	if got.Weekly.State != UsageUnknown {
		t.Fatalf("Weekly=%+v, want Unknown once its own reset has passed", got.Weekly)
	}
	if got.FiveHour.State != UsageAvailable || got.FiveHour.Percent != 20 {
		t.Fatalf("FiveHour=%+v, want the still-valid 20%% reading unaffected by weekly's own reset crossing", got.FiveHour)
	}
}
