package tui

import (
	"strings"

	"github.com/tingtt/agentsctl/internal/session"
)

// ANSI SGR color codes used by the status icon mapping below.
const (
	colorYellow = "33"
	colorGray   = "90"
	colorRed    = "31"
	colorBlue   = "34"
	colorGreen  = "32"
	colorWhite  = "97"
	codeBold    = "1"
)

func ansiColor(glyph, code string) string {
	return "\x1b[" + code + "m" + glyph + "\x1b[0m"
}

// providerColorClaudeCodes and providerColorCodexCodes are the fixed
// 24-bit ANSI truecolor SGR codes (without the leading "\x1b[" / trailing
// "m") used to color provider identity — the session list's provider
// label and the prompt composer's runner label, which share this single
// mapping so the two stay in sync. Session titles no longer carry
// provider color; see titleStyleCodes for what a title's style conveys
// instead (selection / last-detached state).
const (
	providerColorClaudeCodes = "38;2;217;119;87" // #D97757
	providerColorCodexCodes  = "38;2;83;104;235" // #5368EB
)

// providerColor is the single centralized mapping from provider to the
// ANSI truecolor SGR codes used for provider identity, shared by the
// session list's provider label and the prompt composer's runner label.
func providerColor(provider session.ProviderID) string {
	switch provider {
	case session.ProviderClaude:
		return providerColorClaudeCodes
	case session.ProviderCodex:
		return providerColorCodexCodes
	default:
		return ""
	}
}

// providerFieldWidth is the fixed visible-cell width every provider label
// is padded to — long enough for "claude" (6 cells), the longer of the
// two provider names — so a field built from it never shifts whatever
// follows it (the directory in a session row, the prompt body in the
// composer) when the provider changes.
const providerFieldWidth = 6

// providerLabel is the plain-text (uncolored) provider identity, right-
// aligned/padded to exactly providerFieldWidth visible cells. Used both
// immediately before a session row's directory and as the prompt
// composer's runner field.
func providerLabel(provider session.ProviderID) string {
	var name string
	switch provider {
	case session.ProviderClaude:
		name = "claude"
	case session.ProviderCodex:
		name = "codex"
	default:
		name = string(provider)
	}
	if pad := providerFieldWidth - lineCells(name); pad > 0 {
		return strings.Repeat(" ", pad) + name
	}
	return name
}

// titleStyleCodes is the single centralized mapping from a session row's
// selection/last-detach state to its title's SGR codes: selected rows are
// white, others gray; a session that was last explicitly detached from
// (via agentsctl's own Ctrl+], never a natural attached-process exit) is
// additionally bold. Bold composes with — never replaces — the foreground
// color, so all four combinations (selected x last-detached) are
// distinguishable.
func titleStyleCodes(selected, lastDetached bool) []string {
	color := colorGray
	if selected {
		color = colorWhite
	}
	if lastDetached {
		return []string{codeBold, color}
	}
	return []string{color}
}

// statusIcon is the single centralized mapping from session activity to
// its one-cell colored glyph, per the fixed glyph/color table.
func statusIcon(activity session.Activity) string {
	switch activity {
	case session.ActivityIdle:
		return ansiColor("∙", colorYellow)
	case session.ActivityCompleted:
		return ansiColor("∙", colorGray)
	case session.ActivityFailed:
		return ansiColor("✻", colorRed)
	case session.ActivityStarting:
		return ansiColor("∙", colorBlue)
	case session.ActivityWorking:
		return ansiColor("✻", colorGreen)
	case session.ActivityNeedsInput:
		return ansiColor("✻", colorYellow)
	case session.ActivityWaitingQuota:
		return ansiColor("∙", colorBlue)
	default:
		return ansiColor("?", colorGray)
	}
}
