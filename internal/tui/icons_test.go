package tui

import (
	"testing"

	"github.com/tingtt/agentsctl/internal/session"
)

// TestStatusGlyphsMatchSpecTable asserts each activity's exact glyph and
// color, per the fixed status table: idle/completed/starting/
// waitingForQuota use "∙"; failed/working/needsInput use "✻"; unknown
// uses "?". Colors are unchanged from before this glyph swap.
func TestStatusGlyphsMatchSpecTable(t *testing.T) {
	cases := []struct {
		activity session.Activity
		glyph    string
		color    string
	}{
		{session.ActivityIdle, "∙", colorYellow},
		{session.ActivityCompleted, "∙", colorGray},
		{session.ActivityFailed, "✻", colorRed},
		{session.ActivityStarting, "∙", colorBlue},
		{session.ActivityWorking, "✻", colorGreen},
		{session.ActivityNeedsInput, "✻", colorYellow},
		{session.ActivityWaitingQuota, "∙", colorBlue},
		{session.ActivityUnknown, "?", colorGray},
	}
	for _, tc := range cases {
		want := ansiColor(tc.glyph, tc.color)
		if got := statusIcon(tc.activity); got != want {
			t.Fatalf("statusIcon(%s)=%q, want %q (glyph %q)", tc.activity, got, want, tc.glyph)
		}
	}
}

// TestStatusGlyphsAreSingleCellInTerminalWidth pins runeCells' output for
// the two new glyphs specifically (as opposed to
// TestStatusIconsAreSingleTerminalCell, which exercises the same
// assertion indirectly through every activity's full ansiColor(...)
// escape): both "∙" (U+2219 BULLET OPERATOR) and "✻" (U+273B
// TEARDROP-SPOKED ASTERISK) are East-Asian-Width "Narrow"/"Neutral"
// codepoints outside every wide range runeCells recognizes, so they
// render as a single terminal cell on the terminals this project targets
// (macOS Terminal.app, iTerm2) -- the same width the glyphs they replaced
// ("･", "*", "x") had, so row alignment carries over unchanged.
func TestStatusGlyphsAreSingleCellInTerminalWidth(t *testing.T) {
	for _, r := range []rune{'∙', '✻'} {
		if got := runeCells(r); got != 1 {
			t.Fatalf("runeCells(%q)=%d, want 1", r, got)
		}
	}
}
