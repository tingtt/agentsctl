package agentview

import (
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
	if !strings.Contains(empty, "? to show help") || !strings.Contains(empty, "esc to exit") {
		t.Fatalf("empty-prompt footer=%q, want show-help and exit hints", empty)
	}
	if strings.Contains(empty, "esc to clear") {
		t.Fatalf("empty-prompt footer=%q must not mention esc to clear", empty)
	}
	s.Composer.Prompt = "hi"
	nonEmpty := contextualFooterText(s)
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
// "reported 0%" render distinctly -- an unavailable window must never look
// like a 0% utilization.
func TestUsageWindowUnavailableIsNotZeroPercent(t *testing.T) {
	now := time.Now()
	unavailable := usageWindowText(session.UsageWindow{Available: false}, now)
	if strings.Contains(unavailable, "%") {
		t.Fatalf("unavailable window must not render a percentage: %q", unavailable)
	}
	zero := usageWindowText(session.UsageWindow{Available: true, Percent: 0, Reset: now}, now)
	if !strings.Contains(zero, "0%") {
		t.Fatalf("available 0%% window must render 0%%: %q", zero)
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
		session.Usage{Provider: session.ProviderClaude, FiveHour: session.UsageWindow{Available: true, Percent: 70, Reset: time.Now()}, Weekly: session.UsageWindow{Available: true, Percent: 20, Reset: time.Now()}},
		session.Usage{Provider: session.ProviderCodex, FiveHour: session.UsageWindow{Available: true, Percent: 0, Reset: time.Now()}, Weekly: session.UsageWindow{Available: true, Percent: 100, Reset: time.Now()}},
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
	s := freshUsageState(session.Usage{Provider: session.ProviderClaude, FiveHour: session.UsageWindow{Available: true, Percent: 70, Reset: time.Now()}, Weekly: session.UsageWindow{Available: true, Percent: 20, Reset: time.Now()}})
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
	s := freshUsageState(session.Usage{Provider: session.ProviderCodex, FiveHour: session.UsageWindow{Available: true, Percent: 42, Reset: time.Now()}, Weekly: session.UsageWindow{Available: true, Percent: 5, Reset: time.Now()}})
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

// TestHelpLinesDeriveKeysFromBindings fixes the single-source-of-truth
// requirement: help text must reference the same Binding.Label values
// State.Handle and the contextual footer key off of, not an independently
// retyped key.
func TestHelpLinesDeriveKeysFromBindings(t *testing.T) {
	lines := strings.Join(helpLines(200), "\n")
	for _, b := range []Binding{bindingPin, bindingStopArchive, bindingScope, bindingStash, bindingEscape} {
		if !strings.Contains(lines, strings.ToLower(b.Label)) {
			t.Fatalf("help text missing binding %q:\n%s", b.Label, lines)
		}
	}
	if strings.Contains(strings.ToLower(lines), "ctrl+/") {
		t.Fatalf("help text must not mention the removed Ctrl+/ binding:\n%s", lines)
	}
}
