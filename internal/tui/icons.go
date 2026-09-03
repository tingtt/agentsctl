package tui

import "github.com/tingtt/agentsctl/internal/session"

// ANSI SGR color codes used by the status icon mapping below.
const (
	colorYellow = "33"
	colorGray   = "90"
	colorRed    = "31"
	colorBlue   = "34"
	colorGreen  = "32"
)

func ansiColor(glyph, code string) string {
	return "\x1b[" + code + "m" + glyph + "\x1b[0m"
}

// providerColorClaude and providerColorCodex are the fixed 24-bit ANSI
// truecolor foreground escapes used to color session titles, so a
// provider is distinguishable without a Nerd Font runner glyph.
const (
	providerColorClaude = "\x1b[38;2;217;119;87m" // #D97757
	providerColorCodex  = "\x1b[38;2;83;104;235m"  // #5368EB
)

// providerTitleColor is the single centralized mapping from provider to
// the ANSI truecolor escape used for its session titles. Row rendering
// must never spell out "claude" or "codex" as text or a glyph to convey
// provider identity in the overview list; this is the only place that
// maps provider to color.
func providerTitleColor(provider session.ProviderID) string {
	switch provider {
	case session.ProviderClaude:
		return providerColorClaude
	case session.ProviderCodex:
		return providerColorCodex
	default:
		return ""
	}
}

// statusIcon is the single centralized mapping from session activity to
// its one-cell colored glyph, per the fixed glyph/color table.
func statusIcon(activity session.Activity) string {
	switch activity {
	case session.ActivityIdle:
		return ansiColor("･", colorYellow)
	case session.ActivityCompleted:
		return ansiColor("･", colorGray)
	case session.ActivityFailed:
		return ansiColor("x", colorRed)
	case session.ActivityStarting:
		return ansiColor("･", colorBlue)
	case session.ActivityWorking:
		return ansiColor("*", colorGreen)
	case session.ActivityNeedsInput:
		return ansiColor("*", colorYellow)
	case session.ActivityWaitingQuota:
		return ansiColor("･", colorBlue)
	default:
		return ansiColor("?", colorGray)
	}
}
