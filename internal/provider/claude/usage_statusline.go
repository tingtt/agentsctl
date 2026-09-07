package claude

import (
	"encoding/json"
	"math"
	"time"

	"github.com/tingtt/agentsctl/internal/session"
)

// statusLinePayload is the slice of Claude Code's statusLine stdin JSON
// this package actually consumes -- confirmed against the installed CLI's
// own documentation (https://code.claude.com/docs/en/statusline, "Rate
// limit usage" / "Available data"): `rate_limits.five_hour` and
// `rate_limits.seven_day`, each with `used_percentage` (0-100, a float)
// and `resets_at` (Unix epoch seconds). The full payload carries many more
// fields (model, workspace, cost, context_window, ...); everything else is
// ignored by relying on json.Unmarshal's default "unknown fields are
// dropped" behavior.
//
// Per that same documentation, `rate_limits` -- and each window within it
// -- is legitimately absent: only present for Pro/Max subscribers (or
// behind a spend-limit gateway) and only after the session's first API
// response, and a window disappears again once its own resets_at time
// passes. Absence must never be read as a 0% reading (see
// statusLineWindow.toUsageWindowSnapshot).
type statusLinePayload struct {
	RateLimits *statusLineRateLimits `json:"rate_limits"`
}

type statusLineRateLimits struct {
	FiveHour *statusLineWindow `json:"five_hour"`
	SevenDay *statusLineWindow `json:"seven_day"`
}

type statusLineWindow struct {
	UsedPercentage float64 `json:"used_percentage"`
	ResetsAt       int64   `json:"resets_at"`
}

// parseStatusLinePayload decodes one statusLine stdin payload into this
// package's own provider-neutral-ish snapshot shape (see usage_snapshot.go
// -- still Claude-specific, but shaped for local storage rather than the
// raw wire JSON). A payload with no `rate_limits` object at all (a Free
// plan account, or before the session's first API response) yields a
// snapshot with both windows unavailable, not an error -- that is a valid,
// expected shape, not a parse failure.
func parseStatusLinePayload(raw []byte) (usageSnapshot, error) {
	var payload statusLinePayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return usageSnapshot{}, err
	}
	snap := usageSnapshot{ObservedAt: time.Now()}
	if payload.RateLimits == nil {
		return snap, nil
	}
	snap.FiveHour = toUsageWindowSnapshot(payload.RateLimits.FiveHour)
	snap.Weekly = toUsageWindowSnapshot(payload.RateLimits.SevenDay)
	return snap, nil
}

// toUsageWindowSnapshot converts one statusLine rate-limit window into the
// local snapshot shape. A nil window (the field was absent from the
// payload) is session.UsageUnknown (the zero value), never a guessed 0% --
// matching Codex's own rateLimitWindow contract (see
// provider/codex.rateLimitWindow) so both providers draw the same
// distinction between "reported 0%" and "not reported".
func toUsageWindowSnapshot(w *statusLineWindow) usageWindowSnapshot {
	if w == nil {
		return usageWindowSnapshot{}
	}
	return usageWindowSnapshot{
		State:   session.UsageAvailable,
		Percent: int(math.Round(w.UsedPercentage)),
		ResetAt: time.Unix(w.ResetsAt, 0),
	}
}
