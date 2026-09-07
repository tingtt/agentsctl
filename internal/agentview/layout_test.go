package agentview

import (
	"strings"
	"testing"
)

// TestSplitRowWidthPrioritizesProviderCWDOverNoticeOverTitle fixes the
// narrow-terminal width-allocation priority order required by the
// DesignDoc's Width priority section: provider/cwd first, then notice,
// then title.
func TestSplitRowWidthPrioritizesProviderCWDOverNoticeOverTitle(t *testing.T) {
	const cwdCells = 10
	const noticeCells = 30 // longer than "Press Ctrl+X again to archive" needs to be to force clipping.

	title, notice, cwd := splitRowWidth(120, cwdCells, noticeCells)
	if cwd != cwdCells {
		t.Fatalf("wide: cwd=%d, want %d", cwd, cwdCells)
	}
	if notice != noticeCells {
		t.Fatalf("wide: notice=%d, want %d", notice, noticeCells)
	}
	if title <= 0 {
		t.Fatalf("wide: title=%d, want positive", title)
	}

	tight := rowLeftFixed + rowRightFixed + cwdCells
	title, notice, cwd = splitRowWidth(tight, cwdCells, noticeCells)
	if cwd != cwdCells {
		t.Fatalf("tight: cwd=%d, want %d (must never shrink for a notice)", cwd, cwdCells)
	}
	if notice != 0 {
		t.Fatalf("tight: notice=%d, want 0 (dropped before touching cwd)", notice)
	}
	if title != 0 {
		t.Fatalf("tight: title=%d, want 0", title)
	}

	title, notice, cwd = splitRowWidth(tight+5, cwdCells, noticeCells)
	if cwd != cwdCells {
		t.Fatalf("slack: cwd=%d, want %d", cwd, cwdCells)
	}
	if notice <= 0 || notice >= noticeCells {
		t.Fatalf("slack: notice=%d, want a clipped value in (0, %d)", notice, noticeCells)
	}
	if title != 0 {
		t.Fatalf("slack: title=%d, want 0", title)
	}

	title, notice, cwd = splitRowWidth(rowLeftFixed+rowRightFixed+3, cwdCells, noticeCells)
	if cwd != 3 {
		t.Fatalf("ultra-narrow: cwd=%d, want 3", cwd)
	}
	if notice != 0 || title != 0 {
		t.Fatalf("ultra-narrow: notice=%d title=%d, want 0/0", notice, title)
	}
}

// TestClipLineTruncationClosesDanglingStyle fixes a narrow-terminal
// corruption risk: clipping a styled line off before its own style closes
// (e.g. #14's topRule, whose colored cwd can be longer than the terminal
// is wide) must append a reset, or the color would bleed into whatever
// this line's caller writes next.
func TestClipLineTruncationClosesDanglingStyle(t *testing.T) {
	styled := styleText("a very long colored value that will not fit", colorGreen)
	got := clipLine(styled, 5)
	if !strings.HasSuffix(got, "\x1b[0m") {
		t.Fatalf("clipLine truncated a styled value without closing it: %q", got)
	}
}

// TestClipLineUnclippedStyleIsUnchanged fixes that clipLine doesn't add a
// redundant reset when the whole value already fit (styleText already
// closes its own style).
func TestClipLineUnclippedStyleIsUnchanged(t *testing.T) {
	styled := styleText("short", colorGreen)
	if got := clipLine(styled, 80); got != styled {
		t.Fatalf("clipLine(%q, 80) = %q, want unchanged", styled, got)
	}
}

func TestTruncateLeftCellsKeepsTail(t *testing.T) {
	got := truncateLeftCells("/a/very/long/directory/path", 10)
	if len(got) == 0 || got[len(got)-1] != 'h' {
		t.Fatalf("truncateLeftCells=%q, want the tail preserved", got)
	}
}
