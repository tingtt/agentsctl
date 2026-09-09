package session

import "time"

// UsageLimitState is one rate-limit window's provider-independent
// usability, distinct from Percent -- see UsageWindow and Issue #19's
// "Normalized state" section. Every provider (Claude, Codex, and any
// future one) reports one of exactly these three states for each window
// it knows about; a caller like #20's dispatch/queue decision reads only
// this, never a provider-specific error string or a Percent==100
// convention.
type UsageLimitState int

const (
	// UsageUnknown is the zero value: this window's current usability
	// cannot be reported right now -- never fetched, the provider didn't
	// report it at all (e.g. absent from Claude's statusLine payload, or a
	// Codex window with no reported reset time), a refresh failed for a
	// reason other than a detected limit, or a previously-cached reading's
	// own window has since reset with no fresh snapshot yet (see the
	// Claude provider's reset-boundary handling). Percent and Reset carry
	// no meaning when State is UsageUnknown.
	UsageUnknown UsageLimitState = iota
	// UsageAvailable is a trustworthy reading for the window's current
	// period: Percent is the real utilization, Reset (if non-zero) is
	// still in the future.
	UsageAvailable
	// UsageExhausted means the provider itself reported this window's
	// limit as reached -- a valid state transition, not a refresh failure
	// (see the Claude provider's limit detection). Percent is 100 by
	// convention (the display representation of "exhausted"), never
	// derived from a raw percentage reading.
	UsageExhausted
)

// UsageWindow is one rate-limit window's utilization -- e.g. Claude/Codex's
// 5h or weekly quota. State distinguishes "reported 0% used" (Available,
// Percent 0) from "not reported at all" (Unknown) from "provider reported
// the limit reached" (Exhausted): a provider that cannot report a window,
// or whose cached reading no longer applies to the window's current
// period, must never be rendered as if it reported a real percentage (see
// the DesignDoc's Composer footer / usage section and Issue #19).
type UsageWindow struct {
	State   UsageLimitState
	Percent int
	Reset   time.Time
}

// At returns w as it should be treated at wall-clock time now: a reading
// (Available or Exhausted) whose own Reset has already passed (now is not
// before Reset) belonged to a period that has since ended and is reported
// as UsageUnknown (the zero value) instead -- a stale percentage, or a
// stale exhausted state, must never be presented as describing the
// *current* window just because no fresher reading has arrived yet (see
// Issue #19's "cached snapshot の percentage は、その snapshot が属していた usage
// window に対してのみ有効").
//
// This is provider-neutral and deliberately NOT tied to when a provider
// last refreshed: any caller holding a UsageWindow -- a provider
// converting its own cache (see the Claude provider's toSessionUsageWindow,
// which calls this directly), Agent View re-rendering an already-received
// value with no new provider refresh in between, or a future #20 read --
// must apply the exact same rule against its own current `now`, so the
// same UsageWindow value is never valid on one side of that boundary and
// stale-but-still-shown on the other. A window with no Reset at all (zero
// value) has nothing to compare against and is returned unchanged, as is
// one already UsageUnknown.
func (w UsageWindow) At(now time.Time) UsageWindow {
	if w.State == UsageUnknown {
		return w
	}
	if !w.Reset.IsZero() && !now.Before(w.Reset) {
		return UsageWindow{}
	}
	return w
}

// Usage is one provider's account-level utilization, independent of any
// single session -- FiveHour and Weekly mirror Claude/Codex's own rate-
// limit windows. It lives in this provider-neutral package (rather than
// sessionctl, which defines the optional UsageSource capability that
// produces it) so a provider can construct one without importing
// sessionctl, matching every other capability payload
// (Dispatcher/Opener/... all return/accept only session.* types).
type Usage struct {
	Provider ProviderID
	FiveHour UsageWindow
	Weekly   UsageWindow
}

// At returns u with FiveHour and Weekly each independently normalized via
// UsageWindow.At(now) -- one window crossing its own reset boundary never
// affects the other (see Issue #19's "5h と weekly は独立して評価してください").
func (u Usage) At(now time.Time) Usage {
	return Usage{Provider: u.Provider, FiveHour: u.FiveHour.At(now), Weekly: u.Weekly.At(now)}
}
