package agentview

import (
	"strings"
	"testing"

	"github.com/tingtt/agentsctl/internal/session"
)

// TestArchiveConfirmationRendersRedOnTargetRowNotFooter fixes the row-
// notice placement contract: a pending confirmation must render in red on
// the target session's own row, immediately before the provider/cwd
// block, and nowhere in the footer.
func TestArchiveConfirmationRendersRedOnTargetRowNotFooter(t *testing.T) {
	s := NewState()
	s.SetRows([]session.Session{{Key: key("c"), Name: "old", CWD: "/work", Activity: session.ActivityCompleted, Actions: session.Actions{session.ActionArchive: {Available: true}}}})
	s.Handle(KeyEvent{Key: KeyCtrlX})
	view := s.View(80, 12)
	lines := strings.Split(strings.TrimRight(view, "\n"), "\n")
	confirmStyled := styleText("Press Ctrl+X again to archive", colorRed)
	found, inComposer := false, false
	for _, line := range lines {
		// The composer block starts at the top rule (identifiable by its
		// "─" fill, unlike any session-list line); everything at or after
		// it is the footer, which must never carry the row notice.
		if strings.Contains(line, "─") {
			inComposer = true
		}
		if inComposer {
			if strings.Contains(line, "Press Ctrl+X again to archive") {
				t.Fatalf("archive confirmation leaked into the composer/footer: %q", line)
			}
			continue
		}
		if strings.Contains(line, "old") && strings.Contains(line, confirmStyled) {
			found = true
		}
	}
	if !found {
		t.Fatalf("archive confirmation not found styled red on the target row:\n%s", view)
	}
}

// TestFullwidthTitleWithNoticeKeepsCWDAlignment fixes that the provider/
// cwd block's start column stays identical whether or not a row carries a
// notice, and regardless of full-width glyphs in the title. Only Pinned
// rows across more than one directory carry an inline CWD column (see
// groupRows), so both rows here are pinned and given distinct directories
// under a shared "/work/project..." prefix.
func TestFullwidthTitleWithNoticeKeepsCWDAlignment(t *testing.T) {
	s := NewState()
	s.SetRows([]session.Session{
		{Key: key("a"), Name: "short", Pinned: true, Activity: session.ActivityIdle, CWD: "/work/project-a", Actions: session.Actions{session.ActionArchive: {Available: true}}},
		{Key: key("b"), Name: "日本語のタイトル", Pinned: true, Activity: session.ActivityWorking, CWD: "/work/project-b", Actions: session.Actions{session.ActionArchive: {Available: true}}},
	})
	s.selectIndex(1)
	s.Handle(KeyEvent{Key: KeyCtrlX}) // arms the confirmation on row "b"
	view := s.View(80, 12)
	lines := strings.Split(strings.TrimRight(view, "\n"), "\n")
	var offsets []int
	for _, line := range lines {
		// Restrict to session rows: the composer's own top rule also
		// renders "/work/project-b" (the selected row's ComposerCWD), which
		// would otherwise be miscounted as a third row here.
		if !strings.Contains(line, "short") && !strings.Contains(line, "日本語のタイトル") {
			continue
		}
		if idx := strings.Index(line, "/work/project"); idx >= 0 {
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
}

// TestSelectedRowRendersCursorMarker is a representative rendering test:
// the selected row must carry the ">" cursor and no other row does.
func TestSelectedRowRendersCursorMarker(t *testing.T) {
	s := NewState()
	s.SetRows([]session.Session{{Key: key("a"), Name: "first"}, {Key: key("b"), Name: "second"}})
	s.selectIndex(1)
	view := s.View(80, 12)
	lines := strings.Split(view, "\n")
	cursorLines := 0
	for _, line := range lines {
		if strings.HasPrefix(line, "> ") {
			cursorLines++
			if !strings.Contains(line, "second") {
				t.Fatalf("cursor marker on the wrong row: %q", line)
			}
		}
	}
	if cursorLines != 1 {
		t.Fatalf("expected exactly one cursor-marked row, got %d", cursorLines)
	}
}

// TestNarrowTerminalNeverPanics is a representative narrow-terminal
// rendering guarantee: View must degrade gracefully (never panic, never
// produce negative-width slicing) at pathologically small dimensions.
func TestNarrowTerminalNeverPanics(t *testing.T) {
	s := NewState()
	s.SetRows([]session.Session{{Key: key("a"), Name: "a very long session title indeed", CWD: "/some/long/path/here", Actions: session.Actions{session.ActionArchive: {Available: true}}}})
	s.Handle(KeyEvent{Key: KeyCtrlX})
	for _, dims := range [][2]int{{0, 0}, {1, 1}, {5, 3}, {80, 0}} {
		_ = s.View(dims[0], dims[1])
	}
}

// TestNarrowTerminalHandlesMultiDirectoryPinnedAndFullwidth is a
// representative narrow-terminal guarantee for #14's new list rendering:
// a Pinned row's directory column, a directory group heading, the
// provider field, the title, a row notice, and full-width Japanese glyphs
// must all degrade gracefully together (never panic) at a narrow width.
func TestNarrowTerminalHandlesMultiDirectoryPinnedAndFullwidth(t *testing.T) {
	s := NewState()
	s.SetRows([]session.Session{
		{Key: key("a"), Name: "日本語のセッションタイトルとても長い", Pinned: true, CWD: "/workspace/github.com/tingtt/agentsctl", Actions: session.Actions{session.ActionArchive: {Available: true}}},
		{Key: key("b"), Name: "second", CWD: "/workspace/github.com/tingtt-dojo/third-score", Actions: session.Actions{session.ActionArchive: {Available: true}}},
	})
	s.selectIndex(0)
	s.Handle(KeyEvent{Key: KeyCtrlX}) // arms a row notice on row "a"
	for width := 1; width <= 40; width++ {
		_ = s.View(width, 12)
	}
}
