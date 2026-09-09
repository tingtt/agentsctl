package agentview

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tingtt/agentsctl/internal/session"
)

func TestTopRuleExactWidthAndCWDColor(t *testing.T) {
	for _, width := range []int{10, 40, 80, 120} {
		line := topRule("/work/project", width)
		if got := lineCells(line); got != width {
			t.Fatalf("topRule width=%d, got %d cells: %q", width, got, line)
		}
	}
	line := topRule("/work/project", 80)
	if !strings.Contains(line, styleText(" /work/project ", colorGreen)) {
		t.Fatalf("topRule must color the cwd green: %q", line)
	}
}

func TestBottomRuleExactWidth(t *testing.T) {
	for _, width := range []int{1, 10, 80} {
		if got := lineCells(bottomRule(width)); got != width {
			t.Fatalf("bottomRule(%d) = %d cells", width, got)
		}
	}
}

// TestCtrlXHintReflectsSelectedRowActionsNotProvider fixes the DesignDoc's
// "provider-specific condition を Agent View 内で再判定しない" invariant:
// the hint is decided purely from the row's own Actions, for either
// provider, never from row.Key.Provider.
func TestCtrlXHintReflectsSelectedRowActionsNotProvider(t *testing.T) {
	running := session.Session{Key: key("a"), Actions: session.Actions{session.ActionStop: {Available: true}}}
	if label, ok := ctrlXHint(running); !ok || label != "stop" {
		t.Fatalf("running row: label=%q ok=%v, want stop/true", label, ok)
	}
	stopped := session.Session{Key: key("b"), Actions: session.Actions{session.ActionArchive: {Available: true}}}
	if label, ok := ctrlXHint(stopped); !ok || label != "archive" {
		t.Fatalf("stopped row: label=%q ok=%v, want archive/true", label, ok)
	}
	neither := session.Session{Key: key("c"), Actions: session.Actions{session.ActionRename: {Available: true}}}
	if _, ok := ctrlXHint(neither); ok {
		t.Fatal("a row with neither Stop nor Archive available must report ok=false")
	}
}

// TestContextualFooterTextSwapsEmptyVsNonEmptyPrompt fixes #14's ?/Esc
// hint swap: "? to show help"/"esc to exit" only on an empty prompt,
// "esc to clear" (and no "? to show help") once the prompt has text.
func TestContextualFooterTextSwapsEmptyVsNonEmptyPrompt(t *testing.T) {
	s := NewState()
	empty := contextualFooterText(s)
	if !strings.Contains(empty, "ctrl+g to edit in vim") {
		t.Fatalf("empty-prompt footer=%q, want Vim editor hint", empty)
	}
	if !strings.Contains(empty, "? to show help") || !strings.Contains(empty, "esc to exit") {
		t.Fatalf("empty-prompt footer=%q, want show-help and exit hints", empty)
	}
	if strings.Contains(empty, "esc to clear") {
		t.Fatalf("empty-prompt footer=%q must not mention esc to clear", empty)
	}
	s.Composer.Prompt = "hi"
	nonEmpty := contextualFooterText(s)
	if !strings.Contains(nonEmpty, "ctrl+g to edit in vim") {
		t.Fatalf("non-empty-prompt footer=%q, want Vim editor hint", nonEmpty)
	}
	if !strings.Contains(nonEmpty, "esc to clear") {
		t.Fatalf("non-empty-prompt footer=%q, want esc to clear", nonEmpty)
	}
	if strings.Contains(nonEmpty, "? to show help") || strings.Contains(nonEmpty, "esc to exit") {
		t.Fatalf("non-empty-prompt footer=%q must not mention show-help/exit", nonEmpty)
	}
}

// TestUsageColorThresholds fixes the exact boundary values from #14's
// Colors section: white below 70, yellow at 70-89, red at 90-100.
func TestUsageColorThresholds(t *testing.T) {
	cases := []struct {
		pct  int
		want string
	}{
		{0, colorWhite}, {69, colorWhite},
		{70, colorYellow}, {89, colorYellow},
		{90, colorRed}, {100, colorRed},
	}
	for _, c := range cases {
		if got := usageColor(c.pct); got != c.want {
			t.Fatalf("usageColor(%d)=%q, want %q", c.pct, got, c.want)
		}
	}
}

// TestUsageWindowUnavailableIsNotZeroPercent fixes that "not reported" and
// "reported 0%" render distinctly -- a window whose normalized State is
// UsageUnknown must never look like a 0% utilization; it renders the same
// "?%" unknown placeholder as a whole stale/never-fetched provider, not a
// real percentage.
func TestUsageWindowUnavailableIsNotZeroPercent(t *testing.T) {
	now := time.Now()
	unavailable := usageWindowText(session.UsageWindow{State: session.UsageUnknown}, now)
	if !strings.Contains(unavailable, "?%") {
		t.Fatalf("unknown window must render the unknown placeholder: %q", unavailable)
	}
	if strings.Contains(unavailable, "0%") {
		t.Fatalf("unknown window must not render a real percentage: %q", unavailable)
	}
	zero := usageWindowText(session.UsageWindow{State: session.UsageAvailable, Percent: 0, Reset: now.Add(time.Hour)}, now)
	if !strings.Contains(zero, "0%") {
		t.Fatalf("available 0%% window must render 0%%: %q", zero)
	}
}

// TestUsageWindowExhaustedRendersOneHundredPercentRegardlessOfPercent fixes
// Issue #19's rendering contract: a window whose normalized State is
// UsageExhausted always renders 100%, in red (usageColor's top threshold),
// even if Percent carries some other value -- 100% is the display
// convention for exhausted, never something usageWindowText itself derives
// from Percent as a state judgment.
func TestUsageWindowExhaustedRendersOneHundredPercentRegardlessOfPercent(t *testing.T) {
	now := time.Now()
	reset := now.Add(2 * time.Hour)
	got := usageWindowText(session.UsageWindow{State: session.UsageExhausted, Percent: 37, Reset: reset}, now)
	if !strings.Contains(got, "100%") {
		t.Fatalf("exhausted window=%q, want 100%% regardless of Percent", got)
	}
	if strings.Contains(got, "37%") {
		t.Fatalf("exhausted window=%q, must not render the raw Percent value", got)
	}
	if !strings.Contains(got, styleText(fmt.Sprintf("%3d%%", 100), colorRed)) {
		t.Fatalf("exhausted window=%q, want the 100%% rendered in red", got)
	}
	if !strings.Contains(got, formatResetTime(reset, now)) {
		t.Fatalf("exhausted window=%q, want the known reset time rendered", got)
	}
}

// TestUsageWindowExhaustedWithoutKnownResetOmitsResetClause fixes that an
// exhausted window with no known Reset time (a limit detected without a
// carried-forward reset -- see the Claude provider) still renders cleanly,
// without a bogus/zero-value reset clause.
func TestUsageWindowExhaustedWithoutKnownResetOmitsResetClause(t *testing.T) {
	now := time.Now()
	got := usageWindowText(session.UsageWindow{State: session.UsageExhausted}, now)
	if !strings.Contains(got, "100%") {
		t.Fatalf("exhausted window=%q, want 100%%", got)
	}
	if strings.Contains(got, "reset at") {
		t.Fatalf("exhausted window=%q, must not render a reset clause with no known reset time", got)
	}
}

func TestFormatResetTimeSameDayVsOtherDay(t *testing.T) {
	now := time.Date(2026, 9, 7, 10, 0, 0, 0, time.UTC)
	sameDay := time.Date(2026, 9, 7, 19, 10, 0, 0, time.UTC)
	if got := formatResetTime(sameDay, now); got != "07:10 PM" {
		t.Fatalf("same-day reset=%q, want %q", got, "07:10 PM")
	}
	otherDay := time.Date(2026, 9, 13, 5, 0, 0, 0, time.UTC) // a Sunday
	if got := formatResetTime(otherDay, now); got != "Sun 05:00 AM" {
		t.Fatalf("other-day reset=%q, want %q", got, "Sun 05:00 AM")
	}
}

// freshUsageState builds a State with usages applied as fresh (just-now)
// successful updates -- the common setup for usage-line tests that don't
// care about staleness itself.
func freshUsageState(usages ...session.Usage) State {
	s := NewState()
	for _, u := range usages {
		s.ApplyUsageUpdate(u.Provider, u, nil)
	}
	return s
}

// TestUsageLineTextRendersClaudeBeforeCodexInOrder fixes #14's usage
// render order: "claude <5h>/<weekly> · codex <5h>/<weekly>".
func TestUsageLineTextRendersClaudeBeforeCodexInOrder(t *testing.T) {
	s := freshUsageState(
		session.Usage{Provider: session.ProviderClaude, FiveHour: session.UsageWindow{State: session.UsageAvailable, Percent: 70, Reset: time.Now().Add(time.Hour)}, Weekly: session.UsageWindow{State: session.UsageAvailable, Percent: 20, Reset: time.Now().Add(time.Hour)}},
		session.Usage{Provider: session.ProviderCodex, FiveHour: session.UsageWindow{State: session.UsageAvailable, Percent: 0, Reset: time.Now().Add(time.Hour)}, Weekly: session.UsageWindow{State: session.UsageAvailable, Percent: 100, Reset: time.Now().Add(time.Hour)}},
	)
	line := usageLineText(s)
	claudeIdx := strings.Index(line, "claude")
	codexIdx := strings.Index(line, "codex")
	if claudeIdx < 0 || codexIdx < 0 || claudeIdx > codexIdx {
		t.Fatalf("usage line=%q, want claude before codex", line)
	}
}

// TestUsageLineTextShowsUnknownPlaceholderWhenNeverFetched fixes that the
// usage line is never omitted entirely: a fresh State with no usage ever
// applied still renders both known providers, each as the "?%" unknown
// placeholder rather than a real percentage.
func TestUsageLineTextShowsUnknownPlaceholderWhenNeverFetched(t *testing.T) {
	s := NewState()
	line := usageLineText(s)
	if !strings.Contains(line, "claude") || !strings.Contains(line, "codex") {
		t.Fatalf("usage line=%q, want both known providers present even before any fetch", line)
	}
	if strings.Contains(line, "n/a") {
		t.Fatalf("usage line=%q, want the unknown placeholder, not n/a, before any fetch", line)
	}
	if got := strings.Count(line, "?%"); got != 4 {
		t.Fatalf("usage line=%q, want 4 unknown placeholders (2 windows x 2 providers), got %d", line, got)
	}
}

// TestUsageLineTextShowsUnknownPlaceholderWhenStale fixes the freshness
// (not just presence) requirement: a provider whose last successful
// update is older than usageStaleAfter falls back to the unknown
// placeholder even though Usage still holds its last real reading.
func TestUsageLineTextShowsUnknownPlaceholderWhenStale(t *testing.T) {
	s := freshUsageState(session.Usage{Provider: session.ProviderClaude, FiveHour: session.UsageWindow{State: session.UsageAvailable, Percent: 70, Reset: time.Now().Add(time.Hour)}, Weekly: session.UsageWindow{State: session.UsageAvailable, Percent: 20, Reset: time.Now().Add(time.Hour)}})
	s.UsageUpdatedAt[session.ProviderClaude] = time.Now().Add(-(usageStaleAfter + time.Minute))
	line := usageLineText(s)
	if strings.Contains(line, "70%") || strings.Contains(line, "20%") {
		t.Fatalf("usage line=%q, want the stale claude reading replaced by the unknown placeholder", line)
	}
	claudeIdx := strings.Index(line, "claude")
	if claudeIdx < 0 || !strings.Contains(line[claudeIdx:], "?%") {
		t.Fatalf("usage line=%q, want claude's segment to show the unknown placeholder once stale", line)
	}
}

// TestUsageLineTextKeepsFreshProviderWhileOtherIsUnknown fixes that
// staleness/absence is judged per provider, independently: one provider
// showing the unknown placeholder must not affect a different, freshly-
// updated provider's real reading.
func TestUsageLineTextKeepsFreshProviderWhileOtherIsUnknown(t *testing.T) {
	s := freshUsageState(session.Usage{Provider: session.ProviderCodex, FiveHour: session.UsageWindow{State: session.UsageAvailable, Percent: 42, Reset: time.Now().Add(time.Hour)}, Weekly: session.UsageWindow{State: session.UsageAvailable, Percent: 5, Reset: time.Now().Add(time.Hour)}})
	// Claude was never applied at all -- still unknown.
	line := usageLineText(s)
	if !strings.Contains(line, "42%") {
		t.Fatalf("usage line=%q, want codex's fresh 42%% preserved", line)
	}
	claudeIdx := strings.Index(line, "claude")
	codexIdx := strings.Index(line, "codex")
	if claudeIdx < 0 || codexIdx < 0 {
		t.Fatalf("usage line=%q, want both providers present", line)
	}
	if !strings.Contains(line[claudeIdx:codexIdx], "?%") {
		t.Fatalf("usage line=%q, want claude's own segment to show the unknown placeholder", line)
	}
}

// TestUsageLineTextRendersExhaustedWindowEndToEnd fixes Issue #19's
// rendering contract exercised through the full pipeline a real refresh
// uses (ApplyUsageUpdate -> usageLineText -> usageWindowText), not just
// usageWindowText in isolation: a provider whose 5h window came back
// UsageExhausted renders 100% for that window while its still-available
// weekly window renders its own real percentage.
func TestUsageLineTextRendersExhaustedWindowEndToEnd(t *testing.T) {
	s := freshUsageState(session.Usage{
		Provider: session.ProviderClaude,
		FiveHour: session.UsageWindow{State: session.UsageExhausted, Percent: 100, Reset: time.Now().Add(2 * time.Hour)},
		Weekly:   session.UsageWindow{State: session.UsageAvailable, Percent: 84, Reset: time.Now().Add(5 * 24 * time.Hour)},
	})
	line := usageLineText(s)
	if !strings.Contains(line, "100%") {
		t.Fatalf("usage line=%q, want the exhausted 5h window rendered as 100%%", line)
	}
	if !strings.Contains(line, "84%") {
		t.Fatalf("usage line=%q, want the still-available weekly window's real 84%% preserved", line)
	}
}

// TestUsageLineTextExpiresFiveHourAtReadTimeWithoutNewRefresh fixes Issue
// #19's review follow-up: reset-boundary expiry must apply at Agent
// View's own read/render time, not only inside the Claude provider's own
// conversion. Here a usage update arrives (via ApplyUsageUpdate, well
// within usageStaleAfter so the provider-level staleness gate does not
// apply) carrying a 5h window whose own Reset has ALREADY passed by the
// time this renders -- simulating "now crossed Reset with no further
// provider refresh in between" -- while weekly's Reset is still in the
// future. The rendered line must show "?%" for 5h specifically, not the
// stale 92%, while weekly's still-valid 84% renders normally.
func TestUsageLineTextExpiresFiveHourAtReadTimeWithoutNewRefresh(t *testing.T) {
	s := freshUsageState(session.Usage{
		Provider: session.ProviderClaude,
		FiveHour: session.UsageWindow{State: session.UsageAvailable, Percent: 92, Reset: time.Now().Add(-time.Minute)},
		Weekly:   session.UsageWindow{State: session.UsageAvailable, Percent: 84, Reset: time.Now().Add(5 * 24 * time.Hour)},
	})
	line := usageLineText(s)
	if strings.Contains(line, "92%") {
		t.Fatalf("usage line=%q, want the reset-boundary-crossed 5h reading replaced by the unknown placeholder, not the stale 92%%", line)
	}
	if !strings.Contains(line, "84%") {
		t.Fatalf("usage line=%q, want weekly's still-valid 84%% unaffected by 5h's own reset crossing", line)
	}
	claudeIdx := strings.Index(line, "claude")
	if claudeIdx < 0 || !strings.Contains(line[claudeIdx:], "?%") {
		t.Fatalf("usage line=%q, want claude's 5h segment to show the unknown placeholder", line)
	}
}

// TestUsageLineTextExpiresWeeklyAtReadTimeWithoutNewRefresh is the mirror
// of TestUsageLineTextExpiresFiveHourAtReadTimeWithoutNewRefresh: weekly's
// Reset has passed while 5h's has not, and only weekly's reading expires.
func TestUsageLineTextExpiresWeeklyAtReadTimeWithoutNewRefresh(t *testing.T) {
	s := freshUsageState(session.Usage{
		Provider: session.ProviderClaude,
		FiveHour: session.UsageWindow{State: session.UsageAvailable, Percent: 20, Reset: time.Now().Add(time.Hour)},
		Weekly:   session.UsageWindow{State: session.UsageAvailable, Percent: 84, Reset: time.Now().Add(-time.Minute)},
	})
	line := usageLineText(s)
	if strings.Contains(line, "84%") {
		t.Fatalf("usage line=%q, want the reset-boundary-crossed weekly reading replaced by the unknown placeholder, not the stale 84%%", line)
	}
	if !strings.Contains(line, "20%") {
		t.Fatalf("usage line=%q, want 5h's still-valid 20%% unaffected by weekly's own reset crossing", line)
	}
}

// TestUsageLineTextExpiresExhaustedAtReadTimeWithoutNewRefresh fixes that
// an exhausted window, not just an available one, also expires at read
// time once its own known Reset has passed with no new refresh.
func TestUsageLineTextExpiresExhaustedAtReadTimeWithoutNewRefresh(t *testing.T) {
	s := freshUsageState(session.Usage{
		Provider: session.ProviderClaude,
		FiveHour: session.UsageWindow{State: session.UsageExhausted, Percent: 100, Reset: time.Now().Add(-time.Minute)},
	})
	line := usageLineText(s)
	claudeIdx := strings.Index(line, "claude")
	codexIdx := strings.Index(line, "codex")
	if claudeIdx < 0 || codexIdx < 0 {
		t.Fatalf("usage line=%q, want both providers present", line)
	}
	if strings.Contains(line[claudeIdx:codexIdx], "100%") {
		t.Fatalf("usage line=%q, want the reset-boundary-crossed exhausted reading replaced by the unknown placeholder, not a stale 100%%", line)
	}
	if !strings.Contains(line[claudeIdx:codexIdx], "?%") {
		t.Fatalf("usage line=%q, want claude's 5h segment to show the unknown placeholder", line)
	}
}

// TestHelpLinesDeriveKeysFromBindings fixes the single-source-of-truth
// requirement: help text must reference the same Binding.Label values
// State.Handle and the contextual footer key off of, not an independently
// retyped key.
func TestHelpLinesDeriveKeysFromBindings(t *testing.T) {
	lines := strings.Join(helpLines(200), "\n")
	for _, b := range []Binding{bindingPin, bindingStopArchive, bindingScope, bindingStash, bindingPromptEditor, bindingEscape} {
		if !strings.Contains(lines, strings.ToLower(b.Label)) {
			t.Fatalf("help text missing binding %q:\n%s", b.Label, lines)
		}
	}
	if !strings.Contains(strings.ToLower(lines), "ctrl+/") {
		t.Fatalf("help text must mention the directory-scope Ctrl+/ binding:\n%s", lines)
	}
	if !strings.Contains(strings.ToLower(lines), "ctrl+g to edit prompt in vim") {
		t.Fatalf("help text must describe the Ctrl+G Vim binding:\n%s", lines)
	}
}
