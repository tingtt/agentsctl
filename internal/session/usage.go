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
