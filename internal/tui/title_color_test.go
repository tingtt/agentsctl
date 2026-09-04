package tui

import (
	"strings"
	"testing"

	"github.com/tingtt/agentsctl/internal/session"
)

func TestClaudeProviderUsesFixedTrueColor(t *testing.T) {
	if got := providerColor(session.ProviderClaude); got != providerColorClaudeCodes {
		t.Fatalf("Claude provider color=%q, want the #D97757 truecolor codes", got)
	}
	if wrapped := styleText("claude", providerColor(session.ProviderClaude)); wrapped != "\x1b[38;2;217;119;87mclaude\x1b[0m" {
		t.Fatalf("Claude wrapped label=%q", wrapped)
	}
}

func TestCodexProviderUsesFixedTrueColor(t *testing.T) {
	if got := providerColor(session.ProviderCodex); got != providerColorCodexCodes {
		t.Fatalf("Codex provider color=%q, want the #5368EB truecolor codes", got)
	}
	if wrapped := styleText("codex", providerColor(session.ProviderCodex)); wrapped != "\x1b[38;2;83;104;235mcodex\x1b[0m" {
		t.Fatalf("Codex wrapped label=%q", wrapped)
	}
}

func TestProviderLabelFieldWidthIsSixCells(t *testing.T) {
	if got := lineCells(providerLabel(session.ProviderClaude)); got != 6 {
		t.Fatalf("claude label width=%d, want 6", got)
	}
	if got := lineCells(providerLabel(session.ProviderCodex)); got != 6 {
		t.Fatalf("codex label width=%d, want 6", got)
	}
	if providerLabel(session.ProviderClaude) != "claude" {
		t.Fatalf("claude label=%q, want exactly \"claude\" (already 6 cells)", providerLabel(session.ProviderClaude))
	}
	if providerLabel(session.ProviderCodex) != " codex" {
		t.Fatalf("codex label=%q, want right-aligned \" codex\"", providerLabel(session.ProviderCodex))
	}
}

func TestSessionRowsCarryProviderTrueColorBeforeDirectory(t *testing.T) {
	m := NewModel()
	m.SetRows([]session.Session{
		{Key: session.Key{Provider: session.ProviderClaude, ID: "a"}, Name: "claude-session", Activity: session.ActivityIdle, CWD: "/work"},
		{Key: session.Key{Provider: session.ProviderCodex, ID: "b"}, Name: "codex-session", Activity: session.ActivityIdle, CWD: "/work"},
	})
	view := m.View(80, 12)
	if !strings.Contains(view, providerLabelPrefix(session.ProviderClaude)) {
		t.Fatalf("Claude row missing colored provider label:\n%s", view)
	}
	if !strings.Contains(view, providerLabelPrefix(session.ProviderCodex)) {
		t.Fatalf("Codex row missing colored provider label:\n%s", view)
	}
	// Provider label must sit immediately before the directory block (one
	// literal space between them, per the spec's "claude tingtt/agentsctl/"
	// layout), and titles must no longer carry provider color at all.
	if !strings.Contains(view, providerLabelPrefix(session.ProviderClaude)+" work/") {
		t.Fatalf("Claude provider label is not immediately before the directory:\n%s", view)
	}
	if !strings.Contains(view, providerLabelPrefix(session.ProviderCodex)+" work/") {
		t.Fatalf("Codex provider label is not immediately before the directory:\n%s", view)
	}
	if strings.Contains(view, providerColorClaudeCodes+"claude-session") || strings.Contains(view, providerColorCodexCodes+"codex-session") {
		t.Fatalf("session title still carries provider color:\n%s", view)
	}
}

func TestStatusGlyphSurvivesRunnerColumnRemoval(t *testing.T) {
	m := NewModel()
	m.SetRows([]session.Session{
		{Key: session.Key{Provider: session.ProviderClaude, ID: "a"}, Name: "s", Activity: session.ActivityFailed, CWD: "/work"},
	})
	view := m.View(80, 12)
	if !strings.Contains(view, statusIcon(session.ActivityFailed)) {
		t.Fatalf("status glyph is missing from the row:\n%s", view)
	}
}

func TestJapaneseTitleDoesNotShiftRightBlockColumn(t *testing.T) {
	cwd := "/work/project"
	rows := []session.Session{
		{Key: session.Key{Provider: session.ProviderClaude, ID: "a"}, Name: "short", Activity: session.ActivityIdle, CWD: cwd},
		{Key: session.Key{Provider: session.ProviderClaude, ID: "b"}, Name: "日本語のタイトル", Activity: session.ActivityWorking, CWD: cwd},
		{Key: session.Key{Provider: session.ProviderClaude, ID: "c"}, Name: "mix 混在 title", Activity: session.ActivityFailed, CWD: cwd},
	}
	m := NewModel()
	m.SetRows(rows)
	view := m.View(80, 12)
	var offsets []int
	for i := range rows {
		line := rowLine(t, view, i+3)
		byteIdx := strings.Index(line, "project")
		if byteIdx < 0 {
			t.Fatalf("cwd text not found: %q", line)
		}
		offsets = append(offsets, lineCells(line[:byteIdx]))
	}
	for i := 1; i < len(offsets); i++ {
		if offsets[i] != offsets[0] {
			t.Fatalf("right block column shifted with CJK titles: offsets=%v\n%s", offsets, view)
		}
	}
}

func TestClippingIgnoresANSIBytesInStyledTitle(t *testing.T) {
	m := NewModel()
	m.SetRows([]session.Session{
		{Key: session.Key{Provider: session.ProviderClaude, ID: "a"}, Name: strings.Repeat("n", 60), Activity: session.ActivityIdle, CWD: "/work"},
	})
	// A very narrow viewport forces title clipping; the row must still
	// render as valid, well-formed lines (no ANSI bytes counted as
	// visible width, no line exceeding the terminal width).
	view := m.View(24, 10)
	for _, line := range strings.Split(strings.TrimRight(view, "\n"), "\n") {
		if cells := lineCells(line); cells > 24 {
			t.Fatalf("line exceeds width after clipping styled title: cells=%d %q", cells, line)
		}
	}
}

func TestRenameCursorPreservesTitleStyleBeforeAndAfter(t *testing.T) {
	m := NewModel()
	m.Rows = []session.Session{{Key: session.Key{Provider: session.ProviderClaude, ID: "c"}, Name: "old", Activity: session.ActivityIdle, Capabilities: session.Capabilities{Rename: true}}}
	m.Update("rename")
	m.Update("home")
	m.Update("right") // cursor now sits on the second rune ('l'), with "d" trailing
	view := m.View(80, 12)
	// Rename target is always the selected row, so its base foreground is
	// white (selected=true); not last-attached here, so not bold.
	want := styledCursorSuffix(true, false, "l", "d")
	if !strings.Contains(view, "o"+want) {
		t.Fatalf("rename cursor did not preserve the selected-row title style across the cursor glyph:\n%s\nwant substring: %q", view, "o"+want)
	}
}

func TestEndOfTitleCursorRendersWithSelectedTitleStyle(t *testing.T) {
	m := NewModel()
	m.Rows = []session.Session{{Key: session.Key{Provider: session.ProviderCodex, ID: "c"}, Name: "old", Activity: session.ActivityIdle, Capabilities: session.Capabilities{Rename: true}}}
	m.Update("rename") // cursor starts at the end
	view := m.View(80, 12)
	if !strings.Contains(view, titleStylePrefix(true, false)+"old"+cursorStyle(" ")) {
		t.Fatalf("end-of-title cursor did not render inside the selected-row title style:\n%s", view)
	}
}

// --- Session title context styling (selection / last-attached) ---

func TestTitleStyleCodesMatrix(t *testing.T) {
	cases := []struct {
		selected, lastAttached bool
		want                   []string
	}{
		{false, false, []string{colorGray}},
		{true, false, []string{colorWhite}},
		{false, true, []string{codeBold, colorWhite}},
		{true, true, []string{codeBold, colorWhite}},
	}
	for _, tc := range cases {
		got := titleStyleCodes(tc.selected, tc.lastAttached)
		if strings.Join(got, ";") != strings.Join(tc.want, ";") {
			t.Fatalf("titleStyleCodes(selected=%v, lastAttached=%v)=%v, want %v", tc.selected, tc.lastAttached, got, tc.want)
		}
	}
}

func TestNormalRowTitleIsGrayNotBold(t *testing.T) {
	m := NewModel()
	m.Rows = []session.Session{
		{Key: session.Key{Provider: session.ProviderClaude, ID: "a"}, Name: "A", Activity: session.ActivityIdle},
		{Key: session.Key{Provider: session.ProviderClaude, ID: "b"}, Name: "B", Activity: session.ActivityIdle},
	}
	m.Selected = 0
	view := m.View(80, 12)
	if !strings.Contains(view, rowPrefix(" ", session.ActivityIdle, false, false)+"B") {
		t.Fatalf("unselected row title is not plain gray:\n%s", view)
	}
}

func TestSelectedRowTitleIsWhiteNotBoldByDefault(t *testing.T) {
	m := NewModel()
	m.Rows = []session.Session{{Key: session.Key{Provider: session.ProviderClaude, ID: "a"}, Name: "A", Activity: session.ActivityIdle}}
	m.Selected = 0
	view := m.View(80, 12)
	if !strings.Contains(view, rowPrefix(">", session.ActivityIdle, true, false)+"A") {
		t.Fatalf("selected row title is not plain white:\n%s", view)
	}
}

// TestLastAttachedRowNotSelectedIsWhiteBold pins the core semantics fix:
// last-attached state must never be dropped to gray just because the row
// isn't currently selected. Foreground/weight are asserted against literal
// ANSI bytes ("\x1b[1;37m"), not by round-tripping through
// titleStyleCodes, so a regression in either the matrix or the "37"
// foreground choice is actually caught.
func TestLastAttachedRowNotSelectedIsWhiteBold(t *testing.T) {
	m := NewModel()
	m.Rows = []session.Session{
		{Key: session.Key{Provider: session.ProviderClaude, ID: "a"}, Name: "A", Activity: session.ActivityIdle},
		{Key: session.Key{Provider: session.ProviderClaude, ID: "b"}, Name: "B", Activity: session.ActivityIdle},
	}
	m.Selected = 0
	m.MarkAttached(session.Key{Provider: session.ProviderClaude, ID: "b"})
	view := m.View(80, 12)
	if !strings.Contains(view, "\x1b[1;37mB") {
		t.Fatalf("last-attached, unselected row title is not white+bold (want literal \\x1b[1;37m):\n%s", view)
	}
	if strings.Contains(view, "\x1b[1;90mB") {
		t.Fatalf("last-attached, unselected row title regressed to gray+bold:\n%s", view)
	}
	if !strings.Contains(view, "\x1b[37mA") {
		t.Fatalf("selected-only row lost its plain white style (want literal \\x1b[37m):\n%s", view)
	}
}

func TestSelectedAndLastAttachedRowIsWhiteBold(t *testing.T) {
	m := NewModel()
	m.Rows = []session.Session{{Key: session.Key{Provider: session.ProviderClaude, ID: "a"}, Name: "A", Activity: session.ActivityIdle}}
	m.Selected = 0
	m.MarkAttached(session.Key{Provider: session.ProviderClaude, ID: "a"})
	view := m.View(80, 12)
	if !strings.Contains(view, rowPrefix(">", session.ActivityIdle, true, true)+"A") {
		t.Fatalf("selected+last-attached row title is not white+bold:\n%s", view)
	}
}

func TestWhiteStyleFollowsSelectionMovementBoldStaysOnAttachedKey(t *testing.T) {
	m := NewModel()
	m.Rows = []session.Session{
		{Key: session.Key{Provider: session.ProviderClaude, ID: "a"}, Name: "A", Activity: session.ActivityIdle},
		{Key: session.Key{Provider: session.ProviderClaude, ID: "b"}, Name: "B", Activity: session.ActivityIdle},
	}
	m.Selected = 0
	m.MarkAttached(session.Key{Provider: session.ProviderClaude, ID: "b"})
	m.Update("down") // select "b": now selected AND last-attached
	view := m.View(80, 12)
	if !strings.Contains(view, rowPrefix(">", session.ActivityIdle, true, true)+"B") {
		t.Fatalf("selection did not carry white style to the now-selected row:\n%s", view)
	}
	if !strings.Contains(view, rowPrefix(" ", session.ActivityIdle, false, false)+"A") {
		t.Fatalf("previously-selected row did not revert to plain gray:\n%s", view)
	}
}

// TestLastAttachedStaysBoldWhenSelectionMovesAway is the most direct
// regression test for the reported bug: A is last-attached and initially
// selected; moving selection to B must leave A's white+bold style
// untouched (literal "\x1b[1;37m") and give B plain white (literal
// "\x1b[37m", no bold code).
func TestLastAttachedStaysBoldWhenSelectionMovesAway(t *testing.T) {
	m := NewModel()
	m.Rows = []session.Session{
		{Key: session.Key{Provider: session.ProviderClaude, ID: "a"}, Name: "A", Activity: session.ActivityIdle},
		{Key: session.Key{Provider: session.ProviderClaude, ID: "b"}, Name: "B", Activity: session.ActivityIdle},
	}
	m.Selected = 0
	m.MarkAttached(session.Key{Provider: session.ProviderClaude, ID: "a"})
	view := m.View(80, 12)
	if !strings.Contains(view, "\x1b[1;37mA") {
		t.Fatalf("A is not white+bold before moving selection:\n%s", view)
	}
	m.Update("down") // select "b"; "a" is no longer selected but is still last-attached
	view = m.View(80, 12)
	if !strings.Contains(view, "\x1b[1;37mA") {
		t.Fatalf("A lost white+bold after selection moved away from it:\n%s", view)
	}
	if !strings.Contains(view, "\x1b[37mB") || strings.Contains(view, "\x1b[1;37mB") {
		t.Fatalf("B (selected-only) is not plain white, or wrongly picked up bold:\n%s", view)
	}
}

// TestLastAttachedStyleSurvivesRepeatedSelectionMoves moves the selection
// across every row (A -> B -> C -> B) while A stays last-attached the
// whole time: A's white+bold style must never change, proving bold does
// not "follow" the selection cursor around.
func TestLastAttachedStyleSurvivesRepeatedSelectionMoves(t *testing.T) {
	m := NewModel()
	m.Rows = []session.Session{
		{Key: session.Key{Provider: session.ProviderClaude, ID: "a"}, Name: "A", Activity: session.ActivityIdle},
		{Key: session.Key{Provider: session.ProviderClaude, ID: "b"}, Name: "B", Activity: session.ActivityIdle},
		{Key: session.Key{Provider: session.ProviderClaude, ID: "c"}, Name: "C", Activity: session.ActivityIdle},
	}
	m.Selected = 0
	m.MarkAttached(session.Key{Provider: session.ProviderClaude, ID: "a"})
	for _, step := range []string{"down", "down", "up"} { // select b, then c, then back to b
		m.Update(step)
		view := m.View(80, 12)
		if !strings.Contains(view, "\x1b[1;37mA") {
			t.Fatalf("after %q: A lost white+bold:\n%s", step, view)
		}
		if strings.Contains(view, "\x1b[1;90mA") || strings.Contains(view, "\x1b[90mA") {
			t.Fatalf("after %q: A regressed to gray:\n%s", step, view)
		}
	}
}

// TestNewLastAttachedReplacesOldStyle covers re-attaching to a different
// session: after MarkAttached(B), A (the old last-attached session) must
// drop to gray unless separately selected, and B alone becomes white+bold.
func TestNewLastAttachedReplacesOldStyle(t *testing.T) {
	aKey := session.Key{Provider: session.ProviderClaude, ID: "a"}
	bKey := session.Key{Provider: session.ProviderClaude, ID: "b"}
	m := NewModel()
	m.Rows = []session.Session{
		{Key: aKey, Name: "A", Activity: session.ActivityIdle},
		{Key: bKey, Name: "B", Activity: session.ActivityIdle},
	}
	m.MarkAttached(aKey)
	m.MarkAttached(bKey) // re-attach: B replaces A as last-attached
	m.Selected = 0       // select A, which is last-attached no longer
	view := m.View(80, 12)
	if !strings.Contains(view, "\x1b[1;37mB") {
		t.Fatalf("new last-attached B is not white+bold:\n%s", view)
	}
	if strings.Contains(view, "\x1b[1;37mA") || strings.Contains(view, "\x1b[1;90mA") {
		t.Fatalf("old last-attached A kept bold styling after being replaced:\n%s", view)
	}
	if !strings.Contains(view, "\x1b[37mA") {
		t.Fatalf("A (now selected-only) is not plain white:\n%s", view)
	}
}

// TestSelectedOnlyTitleHasNoBoldCode checks the literal ANSI sequence for
// a selected-but-not-last-attached row: it must be exactly "\x1b[37m"
// (plain white), never containing the bold SGR code "1" anywhere in the
// escape.
func TestSelectedOnlyTitleHasNoBoldCode(t *testing.T) {
	m := NewModel()
	m.Rows = []session.Session{{Key: session.Key{Provider: session.ProviderClaude, ID: "a"}, Name: "A", Activity: session.ActivityIdle}}
	m.Selected = 0
	view := m.View(80, 12)
	if !strings.Contains(view, "\x1b[37mA") {
		t.Fatalf("selected-only row is not plain white (literal \\x1b[37m):\n%s", view)
	}
	if strings.Contains(view, "\x1b[1;37mA") || strings.Contains(view, "\x1b[1mA") {
		t.Fatalf("selected-only row's ANSI sequence carries the bold code \"1\":\n%s", view)
	}
}

// TestLastAttachedTitleContainsBoldAndWhite checks the literal ANSI
// sequence for a last-attached row: it must contain both the bold SGR
// code "1" and the white foreground code "37", composed as "\x1b[1;37m".
func TestLastAttachedTitleContainsBoldAndWhite(t *testing.T) {
	m := NewModel()
	m.Rows = []session.Session{{Key: session.Key{Provider: session.ProviderClaude, ID: "a"}, Name: "A", Activity: session.ActivityIdle}}
	m.Selected = 0
	m.MarkAttached(session.Key{Provider: session.ProviderClaude, ID: "a"})
	view := m.View(80, 12)
	if !strings.Contains(view, "\x1b[1;37mA") {
		t.Fatalf("last-attached row's ANSI sequence does not contain bold+white (\\x1b[1;37m):\n%s", view)
	}
}

func TestLastAttachedKeySurvivesPinReorder(t *testing.T) {
	aKey := session.Key{Provider: session.ProviderClaude, ID: "a"}
	bKey := session.Key{Provider: session.ProviderClaude, ID: "b"}
	m := NewModel()
	m.Rows = []session.Session{
		{Key: aKey, Name: "A", Activity: session.ActivityIdle},
		{Key: bKey, Name: "B", Activity: session.ActivityIdle},
	}
	m.Selected = 0
	m.MarkAttached(bKey)
	// ApplyPin both reorders "b" into Pinned (ahead of "a") and, per the
	// pre-existing pin fix, moves the selection to follow it -- so "b" ends
	// up selected AND last-attached here (white+bold), exercising both
	// "row reordering does not lose LastAttachedKey" and "selected key
	// behavior from previous pin fix remains intact" together.
	m.ApplyPin(bKey, true)
	if m.LastAttachedKey != bKey || m.Rows[m.Selected].Key != bKey {
		t.Fatalf("pin reorder lost the last-attached key or the pin-fix selection: lastAttached=%+v selected=%+v", m.LastAttachedKey, m.Rows[m.Selected].Key)
	}
	view := m.View(80, 12)
	if !strings.Contains(view, rowPrefix(">", session.ActivityIdle, true, true)+"B") {
		t.Fatalf("selected+last-attached styling did not follow \"b\" through the pin reorder:\n%s", view)
	}
}

func TestLastAttachedKeySurvivesSetRowsRefreshWithoutMovingByIndex(t *testing.T) {
	aKey := session.Key{Provider: session.ProviderCodex, ID: "a"}
	bKey := session.Key{Provider: session.ProviderCodex, ID: "b"}
	m := NewModel()
	m.SetRows([]session.Session{
		{Key: aKey, Name: "A", Activity: session.ActivityIdle},
		{Key: bKey, Name: "B", Activity: session.ActivityIdle},
	})
	m.MarkAttached(aKey)
	// A provider refresh rebuilds the row slice (new backing values,
	// reordered by index: "b" is now first) but must track the
	// last-attached row by key, not position -- and selection, tracked the
	// same way, keeps "a" selected across the reorder too.
	m.SetRows([]session.Session{
		{Key: bKey, Name: "B", Activity: session.ActivityWorking},
		{Key: aKey, Name: "A", Activity: session.ActivityWorking},
	})
	if m.LastAttachedKey != aKey || m.Rows[m.Selected].Key != aKey {
		t.Fatalf("refresh lost the last-attached key or selection-by-key: lastAttached=%+v selected=%+v", m.LastAttachedKey, m.Rows[m.Selected].Key)
	}
	view := m.View(80, 12)
	if !strings.Contains(view, rowPrefix(">", session.ActivityWorking, true, true)+"A") {
		t.Fatalf("selected+last-attached styling did not follow \"a\" by key across the refresh:\n%s", view)
	}
	// "b" moved into the last-attached row's old index (0) but must not
	// have inherited its bold styling -- proving bold tracks the key, not
	// the row's position in the (re-sorted) list.
	if !strings.Contains(view, rowPrefix(" ", session.ActivityWorking, false, false)+"B") {
		t.Fatalf("bold styling leaked onto \"b\" by index instead of staying on \"a\" by key:\n%s", view)
	}
}

// --- Row fill / right alignment ---

func TestRowFillsTerminalWidthExactly(t *testing.T) {
	for _, width := range []int{80, 120} {
		m := NewModel()
		m.SetRows([]session.Session{
			{Key: session.Key{Provider: session.ProviderClaude, ID: "a"}, Name: "Selected session title", Activity: session.ActivityIdle, CWD: "/Users/tingtt/ghq/github.com/tingtt/agentsctl"},
		})
		view := m.View(width, 12)
		line := rowLine(t, view, 3)
		if got := lineCells(line); got != width {
			t.Fatalf("width=%d: row visible width=%d, want exactly %d\n%q", width, got, width, line)
		}
	}
}

func TestDirectorySlashIsRightmostVisibleCell(t *testing.T) {
	m := NewModel()
	m.SetRows([]session.Session{
		{Key: session.Key{Provider: session.ProviderClaude, ID: "a"}, Name: "s", Activity: session.ActivityIdle, CWD: "/Users/tingtt/ghq/github.com/tingtt/agentsctl"},
	})
	view := m.View(80, 12)
	line := rowLine(t, view, 3)
	if !strings.HasSuffix(line, "/") {
		t.Fatalf("row does not end with the directory's trailing slash: %q", line)
	}
}

func TestClaudeAndCodexDirectoryRightEdgesMatch(t *testing.T) {
	cwd := "/Users/tingtt/ghq/github.com/tingtt/agentsctl"
	m := NewModel()
	m.SetRows([]session.Session{
		{Key: session.Key{Provider: session.ProviderClaude, ID: "a"}, Name: "Claude session", Activity: session.ActivityIdle, CWD: cwd},
		{Key: session.Key{Provider: session.ProviderCodex, ID: "b"}, Name: "Longer codex session name", Activity: session.ActivityIdle, CWD: cwd},
	})
	view := m.View(80, 12)
	claudeLine := rowLine(t, view, 3)
	codexLine := rowLine(t, view, 4)
	if lineCells(claudeLine) != lineCells(codexLine) {
		t.Fatalf("row widths differ: claude=%d codex=%d", lineCells(claudeLine), lineCells(codexLine))
	}
	if !strings.HasSuffix(claudeLine, "agentsctl/") || !strings.HasSuffix(codexLine, "agentsctl/") {
		t.Fatalf("directory right edge does not line up:\nclaude=%q\ncodex=%q", claudeLine, codexLine)
	}
}

func TestASCIIAndJapaneseDirectoryBothFillAndAlign(t *testing.T) {
	m := NewModel()
	m.SetRows([]session.Session{
		{Key: session.Key{Provider: session.ProviderClaude, ID: "a"}, Name: "ascii cwd", Activity: session.ActivityIdle, CWD: "/work/project"},
		{Key: session.Key{Provider: session.ProviderClaude, ID: "b"}, Name: "japanese cwd", Activity: session.ActivityIdle, CWD: "/Users/tingtt/プロジェクト/エージェント"},
	})
	view := m.View(80, 12)
	for _, idx := range []int{3, 4} {
		line := rowLine(t, view, idx)
		if got := lineCells(line); got != 80 {
			t.Fatalf("row %d width=%d, want 80: %q", idx, got, line)
		}
		if !strings.HasSuffix(line, "/") {
			t.Fatalf("row %d does not end in a directory slash: %q", idx, line)
		}
	}
}

func TestANSIStyleDoesNotAffectRightEdge(t *testing.T) {
	m := NewModel()
	m.Rows = []session.Session{{Key: session.Key{Provider: session.ProviderClaude, ID: "a"}, Name: "s", Activity: session.ActivityWorking, CWD: "/work/project"}}
	m.Selected = 0
	m.MarkAttached(session.Key{Provider: session.ProviderClaude, ID: "a"}) // adds bold on top of white, more embedded ANSI
	view := m.View(80, 12)
	if got := lineCells(rowLine(t, view, 3)); got != 80 {
		t.Fatalf("styled row width=%d, want 80:\n%s", got, view)
	}
}

func TestNarrowTerminalRowNeverOverflowsWidth(t *testing.T) {
	m := NewModel()
	m.SetRows([]session.Session{
		{Key: session.Key{Provider: session.ProviderCodex, ID: "a"}, Name: strings.Repeat("long title ", 10), Activity: session.ActivityIdle, CWD: "/Users/tingtt/ghq/github.com/tingtt/a-fairly-long-repository-name"},
	})
	for _, width := range []int{1, 5, 10, 20, 24, 40} {
		view := m.View(width, 10)
		for _, line := range strings.Split(strings.TrimRight(view, "\n"), "\n") {
			if cells := lineCells(line); cells > width {
				t.Fatalf("width=%d: line exceeds width: cells=%d %q", width, cells, line)
			}
		}
	}
}

// --- Directory slash formatting ---

func TestWithTrailingSlash(t *testing.T) {
	cases := []struct{ in, want string }{
		{"agentsctl", "agentsctl/"},
		{"tingtt/agentsctl", "tingtt/agentsctl/"},
		{"github.com/tingtt/agentsctl", "github.com/tingtt/agentsctl/"},
		{"~/ghq/github.com/tingtt/agentsctl", "~/ghq/github.com/tingtt/agentsctl/"},
		{"~", "~/"},
		{"/", "/"},
		{"already/slashed/", "already/slashed/"},
		{"", "/"},
	}
	for _, tc := range cases {
		if got := withTrailingSlash(tc.in); got != tc.want {
			t.Fatalf("withTrailingSlash(%q)=%q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestDisplayCWDDepthsCarryTrailingSlashThroughRowRendering(t *testing.T) {
	const home = "/Users/tingtt"
	path := home + "/ghq/github.com/tingtt/agentsctl"
	m := NewModel()
	m.SetRows([]session.Session{{Key: session.Key{Provider: session.ProviderClaude, ID: "a"}, Name: "s", Activity: session.ActivityIdle, CWD: path}})

	cases := []struct {
		depth int
		want  string
	}{
		{1, "agentsctl/"},
		{2, "tingtt/agentsctl/"},
		{3, "github.com/tingtt/agentsctl/"},
	}
	for _, tc := range cases {
		m.CWDDepth = tc.depth
		view := m.View(80, 12)
		line := rowLine(t, view, 3)
		if !strings.HasSuffix(line, tc.want) {
			t.Fatalf("depth=%d row=%q, want suffix %q", tc.depth, line, tc.want)
		}
	}

	t.Setenv("HOME", home)
	m.CWDDepth = CWDDepthAll
	view := m.View(80, 12)
	line := rowLine(t, view, 3)
	if !strings.HasSuffix(line, "~/ghq/github.com/tingtt/agentsctl/") {
		t.Fatalf("all-mode row=%q, want the shortHome-abbreviated full path with a trailing slash", line)
	}
}

func TestHomeItselfRendersAsTildeSlash(t *testing.T) {
	const home = "/Users/tingtt"
	m := NewModel()
	m.CWDDepth = 2
	m.SetRows([]session.Session{{Key: session.Key{Provider: session.ProviderClaude, ID: "a"}, Name: "s", Activity: session.ActivityIdle, CWD: home}})
	t.Setenv("HOME", home)
	view := m.View(80, 12)
	line := rowLine(t, view, 3)
	if !strings.HasSuffix(line, "~/") {
		t.Fatalf("HOME row=%q, want suffix \"~/\"", line)
	}
}

func TestTruncatedCWDStillEndsInSlash(t *testing.T) {
	const home = "/Users/tingtt"
	t.Setenv("HOME", home)
	long := home + "/ghq/github.com/tingtt/a-fairly-long-repository-name"
	m := NewModel()
	m.CWDDepth = CWDDepthAll
	m.SetRows([]session.Session{{Key: session.Key{Provider: session.ProviderCodex, ID: "a"}, Name: strings.Repeat("n", 30), Activity: session.ActivityIdle, CWD: long}})
	view := m.View(50, 10)
	line := rowLine(t, view, 3)
	if !strings.HasSuffix(line, "/") {
		t.Fatalf("narrow truncated row does not end in \"/\": %q", line)
	}
	if !strings.Contains(line, "…") {
		t.Fatalf("narrow row did not left-truncate the CWD: %q", line)
	}
}
