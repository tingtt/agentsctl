package session

import "time"

// UsageWindow is one rate-limit window's utilization -- e.g. Claude/Codex's
// 5h or weekly quota. Available distinguishes "reported 0% used" from
// "not reported at all": a provider that cannot report a window must never
// be rendered as if it reported 0% (see the DesignDoc's Composer footer /
// usage section).
type UsageWindow struct {
	Available bool
	Percent   int
	Reset     time.Time
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
