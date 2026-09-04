package tui

import (
	"strings"
	"testing"

	"github.com/tingtt/agentsctl/internal/session"
)

// TestArchiveConfirmationCancelsOnEscAndOnSelectionMove covers both ways
// the archive confirmation (and its row notice) must disappear: Esc, and
// moving the selection to another row.
func TestArchiveConfirmationCancelsOnEscAndOnSelectionMove(t *testing.T) {
	key := session.Key{Provider: session.ProviderClaude, ID: "a"}
	other := session.Key{Provider: session.ProviderClaude, ID: "b"}

	t.Run("esc", func(t *testing.T) {
		m := NewModel()
		m.Rows = []session.Session{{Key: key, Capabilities: session.Capabilities{Archive: true}}}
		m.Update("stop-or-archive")
		if _, _, ok := m.rowNotice(key); !ok {
			t.Fatal("archive confirmation was not set")
		}
		m.Update("quit") // Esc
		if _, _, ok := m.rowNotice(key); ok {
			t.Fatal("archive confirmation survived Esc")
		}
		if m.Notice != "Archive cancelled" {
			t.Fatalf("notice=%q, want an explicit cancellation notice", m.Notice)
		}
	})

	t.Run("selection move", func(t *testing.T) {
		m := NewModel()
		m.Rows = []session.Session{
			{Key: key, Capabilities: session.Capabilities{Archive: true}},
			{Key: other, Capabilities: session.Capabilities{Archive: true}},
		}
		m.Update("stop-or-archive")
		if _, _, ok := m.rowNotice(key); !ok {
			t.Fatal("archive confirmation was not set")
		}
		m.Update("down")
		if _, _, ok := m.rowNotice(key); ok {
			t.Fatal("archive confirmation survived a selection move")
		}
	})
}

func TestNoticeColorMapsInfoCyanAndAlertRed(t *testing.T) {
	if got := noticeColor(noticeInfo); got != colorCyan {
		t.Fatalf("info color=%q, want cyan (%q)", got, colorCyan)
	}
	if got := noticeColor(noticeAlert); got != colorRed {
		t.Fatalf("alert color=%q, want red (%q)", got, colorRed)
	}
}

// TestRowNoticeFollowsSessionKeyAcrossPinReorder covers "pin/reorder 後も
// notice target が session.Key に追従する": the archive confirmation is
// keyed by session.Key, so it must still resolve to the same session after
// ApplyPin reorders the row slice.
func TestRowNoticeFollowsSessionKeyAcrossPinReorder(t *testing.T) {
	target := session.Key{Provider: session.ProviderCodex, ID: "target"}
	other := session.Key{Provider: session.ProviderCodex, ID: "other"}
	m := NewModel()
	m.Rows = []session.Session{{Key: other}, {Key: target, Capabilities: session.Capabilities{Archive: true}}}
	m.Selected = 1
	if a := m.Update("stop-or-archive"); a.Kind != ActionNone {
		t.Fatalf("action=%+v", a)
	}
	if _, _, ok := m.rowNotice(target); !ok {
		t.Fatal("archive confirmation not set on target before reorder")
	}
	// Pinning "other" reorders the row slice; the confirmation must still
	// resolve to "target" by key, not by the index it used to occupy.
	m.ApplyPin(other, true)
	if text, severity, ok := m.rowNotice(target); !ok || severity != noticeAlert || !strings.Contains(text, "again") {
		t.Fatalf("notice did not follow target across reorder: text=%q severity=%v ok=%v", text, severity, ok)
	}
	if _, _, ok := m.rowNotice(other); ok {
		t.Fatal("notice leaked onto a different session after reorder")
	}
}

// TestSplitRowWidthPrioritizesProviderCWDOverNoticeOverTitle covers the
// narrow-terminal width-allocation priority order required by the spec:
// provider/cwd first, then notice, then title.
func TestSplitRowWidthPrioritizesProviderCWDOverNoticeOverTitle(t *testing.T) {
	const cwdCells = 10
	const noticeCells = 30 // longer than "Press Ctrl+X again to archive" needs to be to force clipping.

	// Plenty of width: everyone gets what they asked for.
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

	// Width just barely wide enough for the fixed prefix/provider/cwd and
	// nothing else: notice and title must both be squeezed to 0, and cwd
	// must still be reported as fully wanted (cwd only ever shrinks when
	// width can't even fit rowLeftFixed+rowRightFixed+cwdCells).
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

	// A little slack: enough for a short clipped notice but not the whole
	// thing, and still zero title.
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

	// Even narrower than cwd can fit: cwd itself gives way (existing
	// behavior, left-truncated by the caller), notice and title get none.
	title, notice, cwd = splitRowWidth(rowLeftFixed+rowRightFixed+3, cwdCells, noticeCells)
	if cwd != 3 {
		t.Fatalf("ultra-narrow: cwd=%d, want 3", cwd)
	}
	if notice != 0 || title != 0 {
		t.Fatalf("ultra-narrow: notice=%d title=%d, want 0/0", notice, title)
	}
}

// TestArchiveConfirmationRendersRedOnTargetRowNotFooter is the row-level
// counterpart of the removed footer-status assertion: the confirmation
// must render in red on the target session's own row, immediately before
// the provider/cwd block, and nowhere in the footer.
func TestArchiveConfirmationRendersRedOnTargetRowNotFooter(t *testing.T) {
	key := session.Key{Provider: session.ProviderCodex, ID: "c"}
	m := NewModel()
	m.Rows = []session.Session{{Key: key, Name: "old", CWD: "/work", Activity: session.ActivityCompleted, Capabilities: session.Capabilities{Archive: true}}}
	m.Update("stop-or-archive")
	view := m.View(80, 12)
	lines := strings.Split(strings.TrimRight(view, "\n"), "\n")
	confirmStyled := styleText("Press Ctrl+X again to archive", colorRed)
	found := false
	for _, line := range lines[:len(lines)-3] { // exclude the 3 footer lines
		if strings.Contains(line, "old") && strings.Contains(line, confirmStyled) {
			found = true
		}
	}
	if !found {
		t.Fatalf("archive confirmation not found styled red on the target row:\n%s", view)
	}
	for _, line := range lines[len(lines)-3:] {
		if strings.Contains(line, "Press Ctrl+X again to archive") {
			t.Fatalf("archive confirmation leaked into the footer: %q", line)
		}
	}
}

// TestFullwidthTitleWithNoticeKeepsCWDAlignment extends the existing
// full-width-title alignment guarantee to a row carrying a notice: the
// provider/cwd block's start column must stay identical whether or not the
// row has a notice, and regardless of full-width glyphs in the title.
func TestFullwidthTitleWithNoticeKeepsCWDAlignment(t *testing.T) {
	cwd := "/work/project"
	target := session.Key{Provider: session.ProviderCodex, ID: "b"}
	rows := []session.Session{
		{Key: session.Key{Provider: session.ProviderCodex, ID: "a"}, Name: "short", Activity: session.ActivityIdle, CWD: cwd, Capabilities: session.Capabilities{Archive: true}},
		{Key: target, Name: "日本語のタイトル", Activity: session.ActivityWorking, CWD: cwd, Capabilities: session.Capabilities{Archive: true}},
	}
	m := NewModel()
	m.SetRows(rows)
	m.Selected = 1
	m.Update("stop-or-archive") // sets the archive confirmation on row "b"
	view := m.View(80, 12)
	lines := strings.Split(strings.TrimRight(view, "\n"), "\n")
	var offsets []int
	for _, line := range lines {
		// cellOffset (used elsewhere in this package for symmetric rows)
		// does not skip ANSI escapes, so it only cancels out when every
		// compared row carries the exact same escape sequences — not true
		// here, since only one row has a notice. lineCells is ANSI-aware
		// and measures true rendered width, so slice-and-measure with it
		// instead.
		if idx := strings.Index(line, "project"); idx >= 0 {
			offsets = append(offsets, lineCells(line[:idx]))
		}
	}
	if len(offsets) < 2 {
		t.Fatalf("expected both rows' cwd in view:\n%s", view)
	}
	for i := 1; i < len(offsets); i++ {
		if offsets[i] != offsets[0] {
			t.Fatalf("cwd column shifted by the notice: offsets=%v\n%s", offsets, view)
		}
	}
	if !strings.Contains(view, "Press Ctrl+X again to archive") {
		t.Fatalf("notice missing from a wide-enough view:\n%s", view)
	}
}
