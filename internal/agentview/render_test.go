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

// TestMultiDirectoryPinnedRowOrdersCWDBeforeProvider fixes #14's Desired UI
// for a multi-directory Pinned row: the row's right-hand block renders
// `cwd -> provider`, not the reviewed-away `provider -> cwd` order.
func TestMultiDirectoryPinnedRowOrdersCWDBeforeProvider(t *testing.T) {
	s := NewState()
	s.SetRows([]session.Session{
		{Key: key("a"), Name: "first", Pinned: true, CWD: "/work/repo-a"},
		{Key: key("b"), Name: "second", Pinned: true, CWD: "/work/repo-b"},
	})
	view := s.View(120, 12)
	lines := strings.Split(view, "\n")
	var row string
	for _, line := range lines {
		if strings.Contains(line, "first") {
			row = line
			break
		}
	}
	if row == "" {
		t.Fatalf("row for \"first\" not found:\n%s", view)
	}
	cwdIdx := strings.Index(row, displayCWD("/work/repo-a"))
	providerIdx := strings.Index(row, "claude")
	if cwdIdx < 0 || providerIdx < 0 {
		t.Fatalf("row missing cwd or provider: %q", row)
	}
	if cwdIdx >= providerIdx {
		t.Fatalf("row=%q, want cwd (idx %d) before provider (idx %d)", row, cwdIdx, providerIdx)
	}
}

// TestSameDirectoryRowRendersNoCWD fixes that a same-directory scope's rows
// never render an inline CWD, at the full View level (not just groupRows'
// showCWD flag).
func TestSameDirectoryRowRendersNoCWD(t *testing.T) {
	s := NewState()
	s.SetRows([]session.Session{
		{Key: key("a"), Name: "first", CWD: "/work/only-directory"},
		{Key: key("b"), Name: "second", CWD: "/work/only-directory"},
	})
	view := s.View(120, 12)
	for _, line := range strings.Split(view, "\n") {
		if (strings.Contains(line, "first") || strings.Contains(line, "second")) && strings.Contains(line, displayCWD("/work/only-directory")) {
			t.Fatalf("same-directory row must not render its cwd inline: %q", line)
		}
	}
}

// TestUnpinnedDirectoryGroupRowDoesNotRepeatCWD fixes that an unpinned
// row's own line never repeats the directory already stated by its group
// heading, at the full View level.
func TestUnpinnedDirectoryGroupRowDoesNotRepeatCWD(t *testing.T) {
	s := NewState()
	s.SetRows([]session.Session{
		{Key: key("a"), Name: "first", CWD: "/work/repo-a"},
		{Key: key("b"), Name: "second", CWD: "/work/repo-b"},
	})
	view := s.View(120, 12)
	for _, line := range strings.Split(view, "\n") {
		if strings.Contains(line, "first") && strings.Contains(line, displayCWD("/work/repo-a")) {
			t.Fatalf("unpinned directory-group row must not repeat its heading's cwd: %q", line)
		}
	}
}

// TestNarrowMultiDirectoryPinnedRowNeverLeavesDanglingANSI is the
// integration-level counterpart to TestClipLineTruncationClosesDanglingStyle:
// rendering a real multi-directory Pinned row (title -> cwd -> provider,
// each individually styled) across a sweep of narrow widths must never
// leave a line with an unclosed SGR sequence that would bleed color into
// whatever renders next.
func TestNarrowMultiDirectoryPinnedRowNeverLeavesDanglingANSI(t *testing.T) {
	s := NewState()
	s.SetRows([]session.Session{
		{Key: key("a"), Name: "日本語のセッションタイトルとても長い", Pinned: true, CWD: "/workspace/github.com/tingtt/agentsctl", Actions: session.Actions{session.ActionArchive: {Available: true}}},
		{Key: key("b"), Name: "second", Pinned: true, CWD: "/workspace/github.com/tingtt-dojo/third-score"},
	})
	s.selectIndex(0)
	s.Handle(KeyEvent{Key: KeyCtrlX}) // arms a row notice on row "a"
	for width := 1; width <= 60; width++ {
		view := s.View(width, 12)
		for _, line := range strings.Split(view, "\n") {
			if open := ansiOpenAtLineEnd(line); open {
				t.Fatalf("width=%d: line leaves an unclosed ANSI style: %q", width, line)
			}
		}
	}
}

// ansiOpenAtLineEnd reports whether line ends with an SGR style still
// active -- i.e. its last SGR escape sequence isn't a reset ("\x1b[0m").
func ansiOpenAtLineEnd(line string) bool {
	open := false
	for i := 0; i < len(line); {
		if line[i] == 0x1b {
			j := skipANSI(line, i)
			open = line[i:j] != "\x1b[0m"
			i = j
			continue
		}
		i++
	}
	return open
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
