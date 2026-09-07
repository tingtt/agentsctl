package agentview

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
	colorWhite  = "37"
	colorCyan   = "36"
	codeBold    = "1"
)

func ansiColor(glyph, code string) string {
	return "\x1b[" + code + "m" + glyph + "\x1b[0m"
}

// providerColorClaudeCodes and providerColorCodexCodes are the fixed
// 24-bit ANSI truecolor SGR codes (without the leading "\x1b[" / trailing
// "m") used to color provider identity -- the session list's provider
// label and the prompt composer's runner label, which share this single
// mapping so the two stay in sync. Session titles do not carry provider
// color; see titleStyleCodes for what a title's style conveys instead
// (selection / last-attached state).
const (
	providerColorClaudeCodes = "38;2;217;119;87" // #D97757
	providerColorCodexCodes  = "38;2;83;104;235" // #5368EB
)

// providerColor is the single centralized mapping from provider to the
// ANSI truecolor SGR codes used for provider identity.
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
// is padded to -- long enough for "claude" (6 cells) -- so a field built
// from it never shifts whatever follows it when the provider changes.
const providerFieldWidth = 6

// providerName is the plain-text, unpadded provider identity -- e.g. for
// the composer footer/usage lines, which (unlike a session row's provider
// field) don't need column alignment across providers.
func providerName(provider session.ProviderID) string {
	switch provider {
	case session.ProviderClaude:
		return "claude"
	case session.ProviderCodex:
		return "codex"
	default:
		return string(provider)
	}
}

// providerLabel is the plain-text (uncolored) provider identity,
// right-aligned/padded to exactly providerFieldWidth visible cells.
func providerLabel(provider session.ProviderID) string {
	name := providerName(provider)
	if pad := providerFieldWidth - lineCells(name); pad > 0 {
		return strings.Repeat(" ", pad) + name
	}
	return name
}

// titleStyleCodes is the single centralized mapping from a session row's
// selection/last-attached state to its title's SGR codes. Last-attached
// state takes priority over selection: the session most recently opened
// from the overview is always white + bold, whether or not it is
// currently selected. A selected-but-not-last-attached row is white +
// normal. Every other row is gray + normal.
func titleStyleCodes(selected, lastAttached bool) []string {
	if lastAttached {
		return []string{codeBold, colorWhite}
	}
	if selected {
		return []string{colorWhite}
	}
	return []string{colorGray}
}

// statusIcon is the single centralized mapping from session activity to
// its one-cell colored glyph.
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

// noticeColor maps a Severity to its ANSI SGR color code.
func noticeColor(severity Severity) string {
	switch severity {
	case SeverityAlert:
		return colorRed
	case SeverityWarning:
		return colorYellow
	default:
		return colorCyan
	}
}

// styleText wraps text in a single ANSI SGR escape built from codes (e.g.
// styleText(s, "1", "97") for bold+white), the shared style-composition
// primitive for every foreground/weight span in a row. text may already
// contain an embedded cursorStyle segment; that segment closes with its
// own reset, which would otherwise wipe codes' style for anything after
// it, so the style is re-opened immediately after every embedded reset
// before the whole thing is closed with one final reset. Empty codes are
// dropped, so a "no style" caller (e.g. an unknown provider) gets text
// back unchanged.
func styleText(text string, codes ...string) string {
	var active []string
	for _, c := range codes {
		if c != "" {
			active = append(active, c)
		}
	}
	if len(active) == 0 {
		return text
	}
	prefix := "\x1b[" + strings.Join(active, ";") + "m"
	reopened := strings.ReplaceAll(text, "\x1b[0m", "\x1b[0m"+prefix)
	return prefix + reopened + "\x1b[0m"
}
