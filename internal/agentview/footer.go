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

// usageWindowText renders one rate-limit window ("<pct>% (reset at
// <time>)"), or a gray "n/a" when the provider didn't report it --
// Available must never be conflated with a reported 0% (see UsageWindow).
func usageWindowText(w session.UsageWindow, now time.Time) string {
	if !w.Available {
		return styleText("n/a", colorGray)
	}
	pct := styleText(fmt.Sprintf("%3d%%", w.Percent), usageColor(w.Percent))
	reset := styleText("(reset at "+formatResetTime(w.Reset, now)+")", colorGray)
	return pct + " " + reset
}

// usageProviderText renders one provider's "<provider> <5h> / <weekly>"
// segment (see #14's Composer footer render order).
func usageProviderText(u session.Usage, now time.Time) string {
	name := styleText(providerName(u.Provider), providerColor(u.Provider))
	return name + " " + usageWindowText(u.FiveHour, now) + styleText(" / ", colorGray) + usageWindowText(u.Weekly, now)
}

// usageLineText joins every provider's usage segment into #14's single
// usage line ("claude <5h>/<weekly> · codex <5h>/<weekly>"), or reports
// ok=false when there is nothing to show at all (no provider implements
// session.UsageSource, or none reported usage this reload) -- the line
// is omitted entirely rather than rendered empty.
func usageLineText(usages []session.Usage) (string, bool) {
	if len(usages) == 0 {
		return "", false
	}
	now := time.Now()
	parts := make([]string, len(usages))
	for i, u := range usages {
		parts[i] = usageProviderText(u, now)
	}
	return strings.Join(parts, styleText(" · ", colorGray)), true
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
// bindingEscape) rather than re-typed here, so the footer and help view
// can never drift on which physical key a shortcut is bound to.
func helpLines(width int) []string {
	line := func(text string) string { return clipLine(styleText(text, colorGray), width) }
	return []string{
		line("  manage sessions"),
		line("    " + strings.ToLower(bindingPin.Label) + " to pin/unpin session"),
		line("    " + strings.ToLower(bindingStopArchive.Label) + " to stop session"),
		line("    " + strings.ToLower(bindingScope.Label) + " to cycle session listing target directory scope"),
		line("  prompt"),
		line("    " + strings.ToLower(bindingStash.Label) + " to stash/pop"),
		line("  help"),
		line("    " + strings.ToLower(bindingEscape.Label) + " to hide this help"),
	}
}
