package agentview

import (
	"fmt"
	"strings"
	"time"

	"github.com/tingtt/agentsctl/internal/session"
)

// topRule renders the composer's top border: a gray horizontal rule with
// the composer's directory context -- green, per #14's Colors section --
// right-aligned near the end, exactly width cells wide.
func topRule(cwd string, width int) string {
	label := " " + cwd + " "
	tail := styleText(label, colorGreen) + styleText("─", colorGray)
	tailCells := lineCells(label) + 1
	dashes := max(0, width-tailCells)
	return clipLine(styleText(strings.Repeat("─", dashes), colorGray)+tail, width)
}

// bottomRule renders the composer's plain rule between the prompt and the
// footer/help area: a full-width gray horizontal rule with no cwd.
func bottomRule(width int) string {
	return clipLine(styleText(strings.Repeat("─", width), colorGray), width)
}

// ctrlXHint reports the footer's ctrl+x shortcut label ("stop" or
// "archive") from the selected row's own action availability -- never
// re-derived from provider ID (see the DesignDoc's "provider-specific
// condition を Agent View 内で再判定しない") -- and whether one applies at
// all: a selected row where neither Stop nor Archive is available (e.g.
// only Rename), or no selection, reports ok=false.
func ctrlXHint(row session.Session) (label string, ok bool) {
	if row.Actions.Available(session.ActionStop) {
		return "stop", true
	}
	if row.Actions.Available(session.ActionArchive) {
		return "archive", true
	}
	return "", false
}

// contextualFooterText builds #14's single-line contextual shortcut
// summary under the composer: the ctrl+x hint reflects the selected
// session's own state, and the "?"/Esc hints swap based on whether the
// composer prompt is empty (see State.Handle's "?" and Esc handling,
// which this text must stay consistent with).
func contextualFooterText(s State) string {
	provider := styleText(providerName(s.Provider), providerColor(s.Provider))
	if err := s.Warnings[s.Provider]; err != nil {
		provider += styleText(" (unavailable: "+err.Error()+")", colorGray)
	}
	segments := []string{provider + styleText(" (shift+tab to cycle)", colorGray)}
	segments = append(segments, styleText(strings.ToLower(bindingPromptEditor.Label)+" to edit in vim", colorGray))
	if row, ok := s.SelectedRow(); ok {
		if label, has := ctrlXHint(row); has {
			segments = append(segments, styleText("ctrl+x to "+label, colorGray))
		}
	}
	if s.Composer.Prompt == "" {
		segments = append(segments, styleText("? to show help", colorGray), styleText("esc to exit", colorGray))
	} else {
		segments = append(segments, styleText("esc to clear", colorGray))
	}
	return strings.Join(segments, styleText(" · ", colorGray))
}

// usageColor maps a utilization percentage to its #14 threshold color:
// white below 70%, yellow at 70-89%, red at 90% and above.
func usageColor(percent int) string {
	switch {
	case percent >= 90:
		return colorRed
	case percent >= 70:
		return colorYellow
	default:
		return colorWhite
	}
}

// usageWindowText renders one rate-limit window from its normalized
// session.UsageLimitState (see Issue #19's "Normalized state" and
// UsageWindow): UsageExhausted always renders 100% regardless of any
// Percent the provider may have carried forward (100% is the *display*
// convention for exhausted, never something read back out of Percent as a
// state judgment -- that direction only ever goes provider -> Percent, see
// the Claude provider's exhausted-snapshot construction), UsageAvailable
// renders the real percentage, and UsageUnknown (never fetched, not
// reported by the provider, or a cached reading whose own window has since
// reset) renders the same "?%" unknown placeholder usageProviderText
// already falls back to for a whole stale/never-updated provider
// (usageUnknownPercentText) -- one consistent "no trustworthy reading"
// representation regardless of which of those reasons produced it, never
// a percentage that could look like a real 0%.
//
// w.At(now) is applied here, at read/render time, before branching on
// State -- not just once when the provider first produced w. State.Usage
// can sit unchanged across many renders with no new provider refresh in
// between (see State's own doc comment: "直前の値を... 表示し続ける"), so a
// window's own Reset time can pass between when it arrived and when this
// runs again; w.At re-checks that boundary every time, independently of
// whether the Claude provider that produced w already applied the same
// rule once on its own side (see session.UsageWindow.At's doc comment --
// this is the same provider-neutral rule, applied again here so Agent
// View can never show a value the provider boundary would already
// consider expired).
func usageWindowText(w session.UsageWindow, now time.Time) string {
	w = w.At(now)
	switch w.State {
	case session.UsageExhausted:
		pct := styleText(fmt.Sprintf("%3d%%", 100), colorRed)
		if w.Reset.IsZero() {
			return pct
		}
		reset := styleText("(reset at "+formatResetTime(w.Reset, now)+")", colorGray)
		return pct + " " + reset
	case session.UsageAvailable:
		pct := styleText(fmt.Sprintf("%3d%%", w.Percent), usageColor(w.Percent))
		reset := styleText("(reset at "+formatResetTime(w.Reset, now)+")", colorGray)
		return pct + " " + reset
	default:
		return usageUnknownPercentText()
	}
}

// usageStaleAfter is how long a provider's last successful usage fetch
// stays trusted before the composer usage line stops showing it and
// falls back to unknown ("?%") instead: a rate-limit reading from well
// over usageStaleAfter ago is no longer "current usage" and would be
// actively misleading left on screen indefinitely (e.g. across a long
// idle stretch where reload never happens to run again).
const usageStaleAfter = 5 * time.Minute

// usageKnownProviders is the fixed pair of providers the composer usage
// line always reserves a segment for -- the same two providers already
// hardcoded elsewhere in this package (NewState's default composer
// target, Shift+Tab's toggle target), not a general provider-capability
// decision: a provider that has never reported anything yet still gets
// its own row (see usageProviderText), rather than the whole line being
// omitted until every provider has spoken at least once.
var usageKnownProviders = [...]session.ProviderID{session.ProviderClaude, session.ProviderCodex}

// usageUnknownPercentText is the placeholder shown in place of a
// provider's utilization when it has either never been fetched yet or
// gone stale (see usageStaleAfter) -- and, via usageWindowText's
// UsageUnknown case, in place of a single window's own reading whenever
// its normalized session.UsageLimitState is UsageUnknown (never reported,
// or a reset-boundary-crossed cached reading). Deliberately shaped like
// usageWindowText's own "%3d%%" so the column doesn't shift width when a
// provider or window flips between known and unknown.
func usageUnknownPercentText() string {
	return styleText(fmt.Sprintf("%3s%%", "?"), colorGray)
}

// usageProviderText renders one provider's "<provider> <5h> / <weekly>"
// segment (see #14's Composer footer render order). If provider has never
// reported a successful usage update, or its last one is older than
// usageStaleAfter, both windows render as unknown ("?%") instead of a
// stale or absent reading -- freshness, not just Available/Percent,
// decides what's shown.
func usageProviderText(provider session.ProviderID, s State, now time.Time) string {
	name := styleText(providerName(provider), providerColor(provider))
	updatedAt, everUpdated := s.UsageUpdatedAt[provider]
	if !everUpdated || now.Sub(updatedAt) > usageStaleAfter {
		unknown := usageUnknownPercentText()
		return name + " " + unknown + styleText(" / ", colorGray) + unknown
	}
	var u session.Usage
	for _, candidate := range s.Usage {
		if candidate.Provider == provider {
			u = candidate
			break
		}
	}
	return name + " " + usageWindowText(u.FiveHour, now) + styleText(" / ", colorGray) + usageWindowText(u.Weekly, now)
}

// usageLineText builds #14's single usage line ("claude <5h>/<weekly> ·
// codex <5h>/<weekly>"), one segment per usageKnownProviders entry --
// always both, never omitted: a provider that hasn't reported yet (or has
// gone stale) renders its own unknown placeholder instead of the whole
// line disappearing.
func usageLineText(s State) string {
	now := time.Now()
	parts := make([]string, len(usageKnownProviders))
	for i, p := range usageKnownProviders {
		parts[i] = usageProviderText(p, s, now)
	}
	return strings.Join(parts, styleText(" · ", colorGray))
}

// formatResetTime renders a reset time as a bare "03:04 PM" when it falls
// on the same calendar day as now, or "Mon 03:04 PM" otherwise -- matching
// #14's examples ("07:10 AM" for a same-day 5h reset, "Sun 05:00 AM" for a
// weekly reset days out).
func formatResetTime(t, now time.Time) string {
	ty, tm, td := t.Date()
	ny, nm, nd := now.Date()
	if ty == ny && tm == nm && td == nd {
		return t.Format("03:04 PM")
	}
	return t.Format("Mon 03:04 PM")
}

// helpLines renders #14's help view: the shortcut categories and
// descriptions shown in place of the contextual footer/usage lines while
// State.HelpVisible is set. Physical keys are read from keymap.go's named
// Bindings (bindingPin, bindingStopArchive, bindingScope, bindingStash,
// bindingPromptEditor, bindingEscape) rather than re-typed here, so the
// footer and help view can never drift on which physical key a shortcut
// is bound to.
func helpLines(width int) []string {
	line := func(text string) string { return clipLine(styleText(text, colorGray), width) }
	return []string{
		line("  manage sessions"),
		line("    " + strings.ToLower(bindingPin.Label) + " to pin/unpin session"),
		line("    " + strings.ToLower(bindingStopArchive.Label) + " to stop session"),
		line("    " + strings.ToLower(bindingScope.Label) + " to cycle session listing target directory scope"),
		line("  prompt"),
		line("    " + strings.ToLower(bindingStash.Label) + " to stash/pop"),
		line("    " + strings.ToLower(bindingPromptEditor.Label) + " to edit prompt in vim"),
		line("  help"),
		line("    " + strings.ToLower(bindingEscape.Label) + " to hide this help"),
	}
}
