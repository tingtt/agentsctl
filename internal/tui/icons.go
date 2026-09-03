package tui

import "github.com/tingtt/agentsctl/internal/session"

// Nerd Font glyphs used for the runner column. Both are classic Font
// Awesome codepoints (present in essentially every Nerd Font variant,
// including the minimal/mono builds) so they render even without a
// "complete" Nerd Font install. Neither codepoint falls inside any of the
// wide ranges runeCells recognizes, so each occupies exactly one terminal
// cell, matching the Nerd Font convention of patching icons onto
// otherwise-unassigned narrow codepoints.
const (
	glyphClaude = "" // nf-fa-magic
	glyphCodex  = "" // nf-fa-terminal
)

// ANSI SGR color codes shared by the runner and status icon mappings below.
const (
	colorYellow  = "33"
	colorGray    = "90"
	colorRed     = "31"
	colorBlue    = "34"
	colorGreen   = "32"
	colorMagenta = "35"
	colorCyan    = "36"
)

func ansiColor(glyph, code string) string {
	return "\x1b[" + code + "m" + glyph + "\x1b[0m"
}

// runnerIcon is the single centralized mapping from provider to its
// one-cell colored glyph. Row rendering must never spell out "claude" or
// "codex" as text; this is the only place that maps provider to glyph.
func runnerIcon(provider session.ProviderID) string {
	switch provider {
	case session.ProviderClaude:
		return ansiColor(glyphClaude, colorMagenta)
	case session.ProviderCodex:
		return ansiColor(glyphCodex, colorCyan)
	default:
		return "?"
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
