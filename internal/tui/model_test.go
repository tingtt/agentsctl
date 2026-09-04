package tui

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/tingtt/agentsctl/internal/session"
	"golang.org/x/term"
)

// rowPrefix builds the expected "> [status] <title-style>" prefix for a
// row so tests don't hardcode raw ANSI bytes. There is no runner-glyph
// column and titles no longer carry provider color; the style escape
// (see titleStyleCodes) leads directly into the title text, so
// rowPrefix(...)+"text" reproduces exactly the bytes that precede a row's
// title in the rendered view. selected/lastAttached mirror the row's
// state the same way titleStyleCodes expects.
func rowPrefix(cursor string, activity session.Activity, selected, lastAttached bool) string {
	return cursor + " " + statusIcon(activity) + " " + titleStylePrefix(selected, lastAttached)
}

// titleStylePrefix reproduces the single ANSI SGR escape styleText opens
// with for the given selection/last-attached state, without its closing
// reset (the title text and, in the rename editor, a mid-title cursor
// segment, follow it).
func titleStylePrefix(selected, lastAttached bool) string {
	return "\x1b[" + strings.Join(titleStyleCodes(selected, lastAttached), ";") + "m"
}

// styledCursorSuffix reproduces the exact bytes styleText produces for a
// mid-title cursor followed by trailing suffix text: the cursor glyph
// (reverse video, closing with its own ANSI reset), then the title style
// re-opened — so the reset doesn't erase it for the suffix — immediately
// before that suffix.
func styledCursorSuffix(selected, lastAttached bool, glyph, suffix string) string {
	return cursorStyle(glyph) + titleStylePrefix(selected, lastAttached) + suffix
}

// providerLabelPrefix reproduces the exact bytes a session row's right
// block starts with: the provider label, colored and padded to
// providerFieldWidth cells.
func providerLabelPrefix(provider session.ProviderID) string {
	return styleText(providerLabel(provider), providerColor(provider))
}

// cellOffset returns the terminal-cell offset of substr's first byte
// occurrence in line. Multi-byte glyphs (icons, Japanese titles) mean a
// byte offset (strings.Index) is not the same as a cell offset, so
// alignment assertions must go through this.
func cellOffset(t *testing.T, line, substr string) int {
	t.Helper()
	byteIdx := strings.Index(line, substr)
	if byteIdx < 0 {
		t.Fatalf("substring %q not found in %q", substr, line)
	}
	cells := 0
	for _, r := range line[:byteIdx] {
		cells += runeCells(r)
	}
	return cells
}

func rowLine(t *testing.T, view string, rowIndex int) string {
	t.Helper()
	lines := strings.Split(strings.TrimRight(view, "\n"), "\n")
	// Line 0 is the header; row rendering starts at line 1 for a single
	// ungrouped/"Other" section (no rows are pinned in these fixtures, so
	// there's no "Pinned"/"Other" heading before the first row here).
	if rowIndex >= len(lines) {
		t.Fatalf("view has only %d lines, want row %d\n%s", len(lines), rowIndex, view)
	}
	return lines[rowIndex]
}

func TestFullwidthTitleDoesNotShiftStatusOrCWDColumns(t *testing.T) {
	cwd := "/work/project"
	rows := []session.Session{
		{Key: session.Key{Provider: session.ProviderCodex, ID: "a"}, Name: "short", Activity: session.ActivityIdle, CWD: cwd},
		{Key: session.Key{Provider: session.ProviderCodex, ID: "b"}, Name: "日本語のタイトル", Activity: session.ActivityWorking, CWD: cwd},
		{Key: session.Key{Provider: session.ProviderCodex, ID: "c"}, Name: "mix 混在 title", Activity: session.ActivityFailed, CWD: cwd},
	}
	m := NewModel()
	m.SetRows(rows)
	view := m.View(80, 12)
	// Lines: 0="agentsctl · ...", 1="" (header blank), 2="Other" heading,
	// 3.. = rows (none of these rows are pinned).
	var offsets []int
	for i := range rows {
		line := rowLine(t, view, i+3)
		offsets = append(offsets, cellOffset(t, line, "project"))
	}
	for i := 1; i < len(offsets); i++ {
		if offsets[i] != offsets[0] {
			t.Fatalf("cwd column shifted: offsets=%v\n%s", offsets, view)
		}
	}
}

func TestShortHomeExpandsHomeDirectory(t *testing.T) {
	home := "/Users/foo"
	cases := []struct {
		path string
		want string
	}{
		{path: "/Users/foo", want: "~"},
		{path: "/Users/foo/", want: "~"},
		{path: "/Users/foo/ghq/github.com/tingtt/agentsctl", want: "~/ghq/github.com/tingtt/agentsctl"},
		{path: "/Users/foobar", want: "/Users/foobar"},        // sibling dir, not HOME
		{path: "/Users/foobar/ghq", want: "/Users/foobar/ghq"}, // sibling dir subpath
		{path: "/var/tmp", want: "/var/tmp"},
	}
	for _, tc := range cases {
		if got := shortenHome(tc.path, home); got != tc.want {
			t.Fatalf("shortenHome(%q, %q)=%q, want %q", tc.path, home, got, tc.want)
		}
	}
}

func TestTruncateLeftCellsKeepsTailVisible(t *testing.T) {
	long := "~/ghq/github.com/tingtt/agentsctl"
	got := truncateLeftCells(long, 19)
	if !strings.HasSuffix(got, "tingtt/agentsctl") {
		t.Fatalf("truncated=%q lost the tail directory", got)
	}
	if !strings.HasPrefix(got, "…") {
		t.Fatalf("truncated=%q missing left ellipsis", got)
	}
	if lineCells(got) > 19 {
		t.Fatalf("truncated=%q exceeds width", got)
	}
	if lineCells(truncateLeftCells("short", 19)) != len("short") {
		t.Fatalf("short value should not be altered")
	}
}

func TestStatusIconsAreSingleTerminalCell(t *testing.T) {
	activities := []session.Activity{
		session.ActivityIdle, session.ActivityCompleted, session.ActivityFailed, session.ActivityStarting,
		session.ActivityWorking, session.ActivityNeedsInput, session.ActivityWaitingQuota, session.ActivityUnknown,
	}
	for _, activity := range activities {
		if cells := lineCells(statusIcon(activity)); cells != 1 {
			t.Fatalf("status icon for %s is %d cells, want 1", activity, cells)
		}
	}
}

// nerdFontRunnerGlyphs are the two Font Awesome codepoints the runner
// column used to render (nf-fa-magic for Claude, nf-fa-terminal for
// Codex), kept here only so a regression that reintroduces them can be
// detected without depending on a Nerd Font being installed to view them.
const nerdFontRunnerGlyphs = "\uf0d0\uf120"

func TestRunnerColumnAndGlyphAreAbsent(t *testing.T) {
	rows := []session.Session{
		{Key: session.Key{Provider: session.ProviderClaude, ID: "a"}, Name: "claude-row", Activity: session.ActivityIdle, CWD: "/work"},
		{Key: session.Key{Provider: session.ProviderCodex, ID: "b"}, Name: "codex-row", Activity: session.ActivityWorking, CWD: "/work"},
	}
	m := NewModel()
	m.SetRows(rows)
	view := m.View(80, 12)
	for _, r := range nerdFontRunnerGlyphs {
		if strings.ContainsRune(view, r) {
			t.Fatalf("rendered view still contains a Nerd Font runner glyph %q:\n%s", r, view)
		}
	}
	// With no runner column, the title (cursor+status+2 separators = 4
	// cells) immediately follows the fixed prefix. Offset is measured with
	// the ANSI-aware lineCells, not naive rune counting, since the title
	// is now selection/last-attached-style-wrapped (see titleStyleCodes).
	line := rowLine(t, view, 3) // header(0), blank(1), "Other"(2), row(3): no pinned rows here.
	byteIdx := strings.Index(line, "claude-row")
	if byteIdx < 0 {
		t.Fatalf("title text not found in row:\n%s", line)
	}
	if got := lineCells(line[:byteIdx]); got != 4 {
		t.Fatalf("title does not start at the runner-less column offset: got=%d line=%q", got, line)
	}
}

func TestANSIStyleIsNotCountedAsVisibleWidth(t *testing.T) {
	styled := ansiColor("A", colorRed)
	if cells := lineCells(styled); cells != 1 {
		t.Fatalf("styled glyph reported as %d cells, want 1: %q", cells, styled)
	}
	// Clipping to width 1 must keep the whole styled glyph (escape bytes
	// are zero-width), not truncate mid-escape-sequence.
	clipped := clipLine(styled, 1)
	if clipped != styled {
		t.Fatalf("clipLine mangled styled glyph: got %q, want %q", clipped, styled)
	}
	// Clipping to width 0 must drop the glyph but is not required to keep
	// the escape bytes.
	if cells := lineCells(clipLine(styled, 0)); cells != 0 {
		t.Fatalf("clipLine(width=0) left visible width: %q", clipLine(styled, 0))
	}
	padded := fitCells(styled, 5)
	if lineCells(padded) != 5 {
		t.Fatalf("fitCells did not pad styled content to width: %q (%d cells)", padded, lineCells(padded))
	}
}

func TestCursorWindowStylesJapaneseGlyphAtCursor(t *testing.T) {
	got := cursorWindow("あiう", 1, 20) // cursor sits on the ASCII "i"
	want := "あ" + cursorStyle("i") + "う"
	if !strings.HasPrefix(got, want) {
		t.Fatalf("got=%q, want prefix %q", got, want)
	}
}

func TestPromptStashSwapsAndPreservesOverviewState(t *testing.T) {
	selected := session.Key{Provider: session.ProviderCodex, ID: "selected"}
	cases := []struct {
		name, prompt, stash, wantPrompt, wantStash string
	}{
		{name: "both empty"},
		{name: "store prompt", prompt: "A", wantStash: "A"},
		{name: "restore stash", stash: "A", wantPrompt: "A"},
		{name: "swap", prompt: "B", stash: "A", wantPrompt: "A", wantStash: "B"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := NewModel()
			m.Provider = session.ProviderCodex
			m.Prompt = tc.prompt
			m.Stash = tc.stash
			m.Scope = session.ScopeAll
			m.Rows = []session.Session{{Key: selected}}
			m.Selected = 0
			if action := m.Update("stash"); action.Kind != ActionNone {
				t.Fatalf("action=%+v", action)
			}
			if m.Prompt != tc.wantPrompt || m.Stash != tc.wantStash {
				t.Fatalf("prompt=%q stash=%q", m.Prompt, m.Stash)
			}
			if m.Provider != session.ProviderCodex || m.Scope != session.ScopeAll || m.Rows[m.Selected].Key != selected {
				t.Fatalf("overview state changed: %+v", m)
			}
		})
	}

	m := NewModel()
	m.Prompt = "claude value"
	m.Update("stash")
	m.Update("shift+tab")
	m.Prompt = "codex value"
	m.Update("stash")
	if m.Provider != session.ProviderCodex || m.Prompt != "claude value" || m.Stash != "codex value" {
		t.Fatalf("stash was provider-scoped: %+v", m)
	}
}

func TestPromptStashIsInMemoryOnly(t *testing.T) {
	m := NewModel()
	m.Prompt = "ephemeral"
	m.Update("stash")
	if m.Stash != "ephemeral" {
		t.Fatalf("stash=%q", m.Stash)
	}
	if next := NewModel(); next.Prompt != "" || next.Stash != "" {
		t.Fatalf("new process model retained prompt state: %+v", next)
	}
}

func TestShiftTabPreservesPrompt(t *testing.T) {
	m := NewModel()
	m.Prompt = "keep me"
	m.Update("shift+tab")
	if m.Provider != session.ProviderCodex || m.Prompt != "keep me" {
		t.Fatalf("model=%+v", m)
	}
}

func TestComposerCursorEditingUsesRunes(t *testing.T) {
	m := NewModel()
	for _, key := range []string{"A", "界", "B", "left", "left", "X", "right", "delete", "home", "delete", "end", "backspace"} {
		m.Update(key)
	}
	if m.Prompt != "X" || m.PromptCursor != 1 {
		t.Fatalf("prompt=%q cursor=%d", m.Prompt, m.PromptCursor)
	}
	if view := m.View(40, 8); !strings.Contains(view, composerPrefix(session.ProviderClaude, "")+"X"+cursorStyle(" ")) {
		t.Fatalf("composer cursor is not visible:\n%s", view)
	}
}

func TestStashRestorePlacesComposerCursorAtEnd(t *testing.T) {
	m := NewModel()
	m.Stash = "界x"
	m.Update("stash")
	if m.Prompt != "界x" || m.PromptCursor != 2 {
		t.Fatalf("model=%+v", m)
	}
}

func TestSelectionFollowsSessionIdentityAcrossRefresh(t *testing.T) {
	m := NewModel()
	m.SetRows([]session.Session{
		{Key: session.Key{Provider: session.ProviderCodex, ID: "first"}},
		{Key: session.Key{Provider: session.ProviderCodex, ID: "selected"}},
	})
	m.Selected = 1
	m.SetRows([]session.Session{
		{Key: session.Key{Provider: session.ProviderCodex, ID: "selected"}},
		{Key: session.Key{Provider: session.ProviderCodex, ID: "first"}},
	})
	if m.Selected != 0 || m.Rows[m.Selected].Key.ID != "selected" {
		t.Fatalf("selection=%d row=%+v", m.Selected, m.Rows[m.Selected])
	}
}

func TestPinnedAndOtherRenderInNavigationOrder(t *testing.T) {
	m := NewModel()
	m.SetRows([]session.Session{
		{Key: session.Key{Provider: session.ProviderClaude, ID: "A"}, Name: "A", Pinned: true},
		{Key: session.Key{Provider: session.ProviderCodex, ID: "B"}, Name: "B", Pinned: true},
		{Key: session.Key{Provider: session.ProviderClaude, ID: "C"}, Name: "C"},
		{Key: session.Key{Provider: session.ProviderCodex, ID: "D"}, Name: "D"},
	})
	view := m.View(80, 16)
	positions := []int{
		strings.Index(view, "Pinned"),
		strings.Index(view, rowPrefix(">", "", true, false)+"A"),
		strings.Index(view, rowPrefix(" ", "", false, false)+"B"),
		strings.Index(view, "Other"),
		strings.Index(view, rowPrefix(" ", "", false, false)+"C"),
		strings.Index(view, rowPrefix(" ", "", false, false)+"D"),
	}
	for i, position := range positions {
		if position < 0 || i > 0 && position <= positions[i-1] {
			t.Fatalf("render order=%v\n%s", positions, view)
		}
	}
	for _, want := range []string{"B", "C", "D"} {
		m.Update("down")
		if got := m.Rows[m.Selected].Key.ID; got != want {
			t.Fatalf("down selected %s, want %s", got, want)
		}
	}
	for _, want := range []string{"C", "B", "A"} {
		m.Update("up")
		if got := m.Rows[m.Selected].Key.ID; got != want {
			t.Fatalf("up selected %s, want %s", got, want)
		}
	}
}

func TestCtrlTTogglesSelectedPin(t *testing.T) {
	m := NewModel()
	m.Rows = []session.Session{{Key: session.Key{Provider: session.ProviderClaude, ID: "selected"}}}
	action := m.Update("pin")
	if action.Kind != ActionPin || action.Session == nil || action.Session.Key.ID != "selected" {
		t.Fatalf("action=%+v", action)
	}
}

func TestViewIsBoundedAndKeepsSelectionVisible(t *testing.T) {
	m := NewModel()
	for i := 0; i < 100; i++ {
		m.Rows = append(m.Rows, session.Session{Key: session.Key{Provider: session.ProviderCodex, ID: fmt.Sprint(i)}, Name: fmt.Sprintf("session-%03d", i), Activity: session.ActivityIdle, UpdatedAt: time.Now()})
	}
	m.Selected = 80
	view := m.View(80, 16)
	if lines := strings.Count(view, "\n"); lines != 16 {
		t.Fatalf("lines=%d", lines)
	}
	if !strings.Contains(view, rowPrefix(">", session.ActivityIdle, true, false)+"session-080") {
		t.Fatalf("selected row is outside viewport:\n%s", view)
	}
	if strings.Contains(view, "session-000") {
		t.Fatal("viewport rendered off-screen rows")
	}
}

func TestTerminalFrameUsesCarriageReturnAndOneScreenClear(t *testing.T) {
	frame := terminalFrame("one\ntwo\r\nthree\n")
	if !strings.HasPrefix(frame, "\x1b[2J\x1b[H") {
		t.Fatalf("prefix=%q", frame)
	}
	if strings.Contains(strings.ReplaceAll(frame, "\r\n", ""), "\n") {
		t.Fatalf("bare LF in raw-terminal frame: %q", frame)
	}
	if strings.Contains(frame, "\r\r\n") {
		t.Fatalf("existing CRLF was converted twice: %q", frame)
	}
}

func TestReadKeyLeavesBatchedKeyAfterShiftTab(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("\x1b[Z\r"))
	first, err := readKey(r)
	if err != nil || first != "shift+tab" {
		t.Fatalf("first=%q err=%v", first, err)
	}
	second, err := readKey(r)
	if err != nil || second != "enter" {
		t.Fatalf("second=%q err=%v", second, err)
	}
}
func TestEnterDispatchesComposerOrAttachesSelection(t *testing.T) {
	m := NewModel()
	m.Prompt = "fix tests"
	a := m.Update("enter")
	if a.Kind != ActionDispatch || a.Prompt != "fix tests" {
		t.Fatalf("action=%+v", a)
	}
	m.Prompt = ""
	m.Rows = []session.Session{{Key: session.Key{Provider: session.ProviderCodex, ID: "c"}, Capabilities: session.Capabilities{Attach: true}}}
	a = m.Update("enter")
	if a.Kind != ActionAttach || a.Session.Key.ID != "c" {
		t.Fatalf("action=%+v", a)
	}
}

func TestEmptyEnterWithoutSelectionIsSafe(t *testing.T) {
	m := NewModel()
	a := m.Update("enter")
	if a.Kind != ActionNone || m.Error != "No session selected" {
		t.Fatalf("action=%+v error=%q", a, m.Error)
	}
}

func TestModeInputPriority(t *testing.T) {
	key := session.Key{Provider: session.ProviderCodex, ID: "stable"}
	row := session.Session{Key: key, Name: "old", Capabilities: session.Capabilities{Attach: true, Rename: true}}
	for _, tc := range []struct {
		name  string
		setup func(*Model)
		key   string
	}{
		{name: "rename stash", setup: func(m *Model) { m.Update("rename") }, key: "stash"},
		{name: "rename selection", setup: func(m *Model) { m.Update("rename") }, key: "down"},
		{name: "archive stash", setup: func(m *Model) { target := key; m.archiveConfirm = &target }, key: "stash"},
		{name: "archive enter", setup: func(m *Model) { target := key; m.archiveConfirm = &target }, key: "enter"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := NewModel()
			m.Rows = []session.Session{row, {Key: session.Key{Provider: session.ProviderCodex, ID: "other"}}}
			m.Prompt, m.Stash = "prompt", "stash"
			tc.setup(&m)
			a := m.Update(tc.key)
			if a.Kind != ActionNone || m.Prompt != "prompt" || m.Stash != "stash" || m.Selected != 0 {
				t.Fatalf("action=%+v model=%+v", a, m)
			}
		})
	}
}

// TestCtrlAIsUnboundAndDoesNotChangeScope covers the acceptance criterion
// "Ctrl+A では scope が変わらない": Ctrl+A no longer maps to any key action
// (readKey returns "" for its raw byte, see TestReadKeyConsumesLegacy...),
// and even if that empty string somehow reached Update, it must not be
// interpreted as any scope-changing key.
func TestCtrlAIsUnboundAndDoesNotChangeScope(t *testing.T) {
	m := NewModel()
	before := m.Scope
	key, err := readKey(bufio.NewReader(strings.NewReader("\x01")))
	if err != nil {
		t.Fatal(err)
	}
	if key != "" {
		t.Fatalf("Ctrl+A (0x01) decoded to key=%q, want unbound", key)
	}
	if action := m.Update(key); action.Kind != ActionNone || m.Scope != before {
		t.Fatalf("action=%+v scope=%v, want unchanged", action, m.Scope)
	}
}

// TestCtrlGCyclesScopeThreeWaysThenWraps covers the acceptance criterion
// "Ctrl+G で cwd -> cwd/** -> all -> cwd と cycle する".
func TestCtrlGCyclesScopeThreeWaysThenWraps(t *testing.T) {
	m := NewModel()
	if m.Scope != session.ScopeCWD {
		t.Fatalf("initial scope=%v, want ScopeCWD", m.Scope)
	}
	want := []session.DirectoryScope{session.ScopeSubtree, session.ScopeAll, session.ScopeCWD, session.ScopeSubtree}
	for i, w := range want {
		action := m.Update("scope-cycle")
		if action.Kind != ActionRefresh {
			t.Fatalf("step %d: action=%+v, want ActionRefresh", i, action)
		}
		if m.Scope != w {
			t.Fatalf("step %d: scope=%v, want %v", i, m.Scope, w)
		}
	}
}

func TestInlineRenameEditingTargetsStableSession(t *testing.T) {
	target := session.Key{Provider: session.ProviderCodex, ID: "target"}
	m := NewModel()
	m.Prompt, m.Stash = "composer", "stashed"
	m.Rows = []session.Session{
		{Key: target, Name: "alpha", Capabilities: session.Capabilities{Rename: true}},
		{Key: session.Key{Provider: session.ProviderCodex, ID: "other"}, Name: "other", Capabilities: session.Capabilities{Rename: true}},
	}
	if action := m.Update("rename"); action.Kind != ActionNone || !m.Renaming || m.RenameTarget != target || m.RenameOriginal != "alpha" || m.RenameDraft != "alpha" || m.RenameCursor != 5 {
		t.Fatalf("rename start action=%+v model=%+v", action, m)
	}
	m.Update("home")
	m.Update("right")
	m.Update("delete")
	m.Update("X")
	m.Update("end")
	m.Update("backspace")
	if m.RenameDraft != "aXph" {
		t.Fatalf("draft=%q cursor=%d", m.RenameDraft, m.RenameCursor)
	}
	m.Selected = 1 // A refresh/reorder must not retarget the pending rename.
	a := m.Update("enter")
	if a.Kind != ActionRename || a.SessionKey == nil || *a.SessionKey != target || a.Name != "aXph" {
		t.Fatalf("rename action=%+v", a)
	}
	if m.Prompt != "composer" || m.Stash != "stashed" {
		t.Fatalf("composer state changed: %+v", m)
	}
}

func TestInlineRenameCancelAndCapabilityFailurePreserveComposer(t *testing.T) {
	m := NewModel()
	m.Prompt, m.Stash = "composer", "stashed"
	m.Rows = []session.Session{{Key: session.Key{Provider: session.ProviderClaude, ID: "c"}, Name: "old", Capabilities: session.Capabilities{Reason: "session is archived"}}}
	if action := m.Update("rename"); action.Kind != ActionNone || m.Renaming || !strings.Contains(m.Error, "session is archived") {
		t.Fatalf("unsupported rename action=%+v model=%+v", action, m)
	}
	m.Rows[0].Capabilities.Rename = true
	m.Update("rename")
	m.Update("Z")
	m.Update("quit")
	if m.Renaming || m.Prompt != "composer" || m.Stash != "stashed" || m.Rows[0].Name != "old" {
		t.Fatalf("cancel changed state: %+v", m)
	}
}

func TestInlineRenameRendersEditorInsideSelectedNameCell(t *testing.T) {
	m := NewModel()
	m.Rows = []session.Session{{Key: session.Key{Provider: session.ProviderCodex, ID: "c"}, Name: "old", Activity: session.ActivityIdle, Capabilities: session.Capabilities{Rename: true}}}
	m.Update("rename")
	view := m.View(80, 12)
	wantEditor := rowPrefix(">", session.ActivityIdle, true, false) + "old" + cursorStyle(" ")
	if !strings.Contains(view, wantEditor) || strings.Contains(view, "Rename:") {
		t.Fatalf("rename editor is not in row name cell:\n%s", view)
	}
}

func TestNarrowInlineRenameKeepsRowAndCursorVisible(t *testing.T) {
	m := NewModel()
	m.Rows = []session.Session{{Key: session.Key{Provider: session.ProviderCodex, ID: "c"}, Name: strings.Repeat("n", 40), Activity: session.ActivityIdle, Capabilities: session.Capabilities{Rename: true}}}
	m.Update("rename")
	view := m.View(24, 10)
	if !strings.Contains(view, rowPrefix(">", session.ActivityIdle, true, false)) || !strings.Contains(view, "\x1b[30;47m") {
		t.Fatalf("narrow rename lost selected row or cursor:\n%s", view)
	}
}

func TestCodexAgentsKeyTransitions(t *testing.T) {
	m := NewModel()
	m.Rows = []session.Session{{Key: session.Key{Provider: session.ProviderCodex, ID: "c"}, Name: "old", Capabilities: session.Capabilities{Attach: true, Stop: true, Rename: true}}}
	if a := m.Update("open"); a.Kind != ActionAttach {
		t.Fatalf("open action=%+v", a)
	}
	if a := m.Update("stop-or-archive"); a.Kind != ActionStop {
		t.Fatalf("stop action=%+v", a)
	}
	if a := m.Update("scope-cycle"); a.Kind != ActionRefresh || m.Scope != session.ScopeSubtree {
		t.Fatalf("scope-cycle action=%+v model=%+v", a, m)
	}
	if a := m.Update("rename"); a.Kind != ActionNone || !m.Renaming || m.RenameDraft != "old" {
		t.Fatalf("rename start action=%+v model=%+v", a, m)
	}
	m.Update("backspace")
	if a := m.Update("enter"); a.Kind != ActionRename || a.Name != "ol" || a.SessionKey == nil || a.SessionKey.ID != "c" {
		t.Fatalf("rename commit action=%+v", a)
	}
}

func TestCtrlXRequiresConfirmationBeforeArchive(t *testing.T) {
	key := session.Key{Provider: session.ProviderClaude, ID: "done"}
	m := NewModel()
	m.Rows = []session.Session{{Key: key, Capabilities: session.Capabilities{Archive: true}}}
	if a := m.Update("stop-or-archive"); a.Kind != ActionNone {
		t.Fatalf("first action=%+v", a)
	}
	// The confirmation is not a Status/Error string — it is derived from
	// session.Key state (see rowNotice) so it always tracks the right row.
	text, severity, ok := m.rowNotice(key)
	if !ok || !strings.Contains(text, "again") || severity != noticeAlert {
		t.Fatalf("row notice=%q severity=%v ok=%v, want an alert-severity confirmation", text, severity, ok)
	}
	if m.Error != "" {
		t.Fatalf("archive confirmation must not use the error notification: %q", m.Error)
	}
	if a := m.Update("stop-or-archive"); a.Kind != ActionArchive {
		t.Fatalf("action=%+v", a)
	}
	if _, _, ok := m.rowNotice(key); ok {
		t.Fatal("row notice remained after the second Ctrl+X committed the archive")
	}
}

func TestUnsupportedActionExplainsWhy(t *testing.T) {
	m := NewModel()
	m.Rows = []session.Session{{Key: session.Key{Provider: session.ProviderCodex, ID: "external"}, Capabilities: session.Capabilities{Reason: "external writer"}}}
	if a := m.Update("open"); a.Kind != ActionNone || !strings.Contains(m.Error, "external writer") {
		t.Fatalf("action=%+v error=%q", a, m.Error)
	}
}

func TestReadKeyConsumesLegacyAndUnknownSequencesAtomically(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"\x1b[Z", "shift+tab"},
		{"\x0f", "open"},
		{"\x01", ""}, // Ctrl+A is no longer bound to anything.
		{"\x07", "scope-cycle"},
		{"\x18", "stop-or-archive"},
		{"\x13", "stash"},
		{"\x14", "pin"},
		{"\x1b[H", "home"},
		{"\x1b[F", "end"},
		{"\x1b[1~", "home"},
		{"\x1b[4~", "end"},
		{"\x1b[3~", "delete"},
		{"\x1b[55;5uX", ""},
		{"\x1b[999~X", ""},
	}
	for _, tc := range cases {
		r := bufio.NewReader(strings.NewReader(tc.input))
		got, err := readKey(r)
		if err != nil || got != tc.want {
			t.Fatalf("input=%q got=%q err=%v", tc.input, got, err)
		}
		if strings.HasSuffix(tc.input, "X") {
			next, err := readKey(r)
			if err != nil || next != "X" {
				t.Fatalf("input=%q trailing key=%q err=%v", tc.input, next, err)
			}
		}
	}
}

// TestReadKeySupportsSS3ArrowAndHomeEndSequences covers the ESC O <letter>
// (SS3) key encoding, not just the ESC [ <letter> (CSI) form covered by
// TestReadKeyConsumesLegacyAndUnknownSequencesAtomically. This form is not
// hypothetical: `infocmp` for TERM=xterm-256color (a common default,
// including on macOS terminals) declares kcub1/kcuf1/kcuu1/kcud1
// (Left/Right/Up/Down) as \EOD/\EOC/\EOA/\EOB.
func TestReadKeySupportsSS3ArrowAndHomeEndSequences(t *testing.T) {
	cases := []struct{ input, want string }{
		{"\x1bOD", "left"},
		{"\x1bOC", "right"},
		{"\x1bOA", "up"},
		{"\x1bOB", "down"},
		{"\x1bOH", "home"},
		{"\x1bOF", "end"},
	}
	for _, tc := range cases {
		r := bufio.NewReader(strings.NewReader(tc.input))
		got, err := readKey(r)
		if err != nil || got != tc.want {
			t.Fatalf("input=%q got=%q want=%q err=%v", tc.input, got, tc.want, err)
		}
	}
}

func TestReadKeyReturnsCompleteUTF8Rune(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("界x"))
	first, err := readKey(r)
	if err != nil || first != "界" {
		t.Fatalf("first=%q err=%v", first, err)
	}
	second, err := readKey(r)
	if err != nil || second != "x" {
		t.Fatalf("second=%q err=%v", second, err)
	}
}

func TestInputBytesDriveActionsWithoutLeakingProtocolText(t *testing.T) {
	m := NewModel()
	m.Rows = []session.Session{{
		Key:          session.Key{Provider: session.ProviderCodex, ID: "fixture"},
		Capabilities: session.Capabilities{Attach: true, Stop: true},
	}}
	for _, tc := range []struct {
		input string
		want  ActionKind
	}{
		{"\x0f", ActionAttach},
		{"\x18", ActionStop},
		{"\x0c", ActionRefresh},
	} {
		key, err := readKey(bufio.NewReader(strings.NewReader(tc.input)))
		if err != nil {
			t.Fatal(err)
		}
		if action := m.Update(key); action.Kind != tc.want {
			t.Fatalf("input=%q action=%+v", tc.input, action)
		}
	}

	for _, input := range []string{"\x16", "\x1b[55;5u", "\x1b[999~"} {
		key, err := readKey(bufio.NewReader(strings.NewReader(input)))
		if err != nil {
			t.Fatal(err)
		}
		before := m.Prompt
		if action := m.Update(key); action.Kind != ActionNone || m.Prompt != before || m.Scope != session.ScopeCWD {
			t.Fatalf("input=%q key=%q action=%+v model=%+v", input, key, action, m)
		}
	}
}

func TestTerminalLifecycleUsesNoEnhancedKeyboardProtocol(t *testing.T) {
	var output bytes.Buffer
	beginTerminal(&output)
	endTerminal(&output)
	got := output.String()
	if strings.Contains(got, "[>1u") || strings.Contains(got, "[<u") {
		t.Fatalf("enhanced keyboard protocol present: %q", got)
	}
	for _, sequence := range []string{"\x1b[?1049h", "\x1b[?25l", "\x1b[?25h", "\x1b[?1049l"} {
		if strings.Count(got, sequence) != 1 {
			t.Fatalf("terminal sequence %q count=%d", sequence, strings.Count(got, sequence))
		}
	}
}

func TestRunRestoresTerminalOnErrorAndPanic(t *testing.T) {
	for _, tc := range []struct {
		name  string
		read  func(*bufio.Reader) (string, error)
		panic bool
	}{
		{name: "error", read: func(*bufio.Reader) (string, error) { return "", errors.New("fixture read failure") }},
		{name: "panic", panic: true, read: func(*bufio.Reader) (string, error) { panic("fixture panic") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			master, slave, err := pty.Open()
			if err != nil {
				t.Fatal(err)
			}
			defer master.Close()
			defer slave.Close()
			before, err := term.GetState(int(slave.Fd()))
			if err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			app := App{Input: slave, Output: &output, CWD: "/work", Model: NewModel(), ReadInput: tc.read}
			func() {
				if tc.panic {
					defer func() {
						if recover() == nil {
							t.Fatal("expected panic")
						}
					}()
				}
				_ = app.Run(context.Background())
			}()
			after, err := term.GetState(int(slave.Fd()))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("terminal state changed: before=%#v after=%#v", before, after)
			}
			if !strings.Contains(output.String(), "\x1b[?25h\x1b[?1049l") {
				t.Fatalf("terminal UI was not restored: %q", output.String())
			}
		})
	}
}

func TestViewKeepsFooterComposerSelectionAndErrorWithinNarrowViewport(t *testing.T) {
	for _, count := range []int{0, 1, 100} {
		m := NewModel()
		for i := 0; i < count; i++ {
			m.Rows = append(m.Rows, session.Session{Key: session.Key{Provider: session.ProviderCodex, ID: fmt.Sprint(i)}, Name: fmt.Sprintf("session-%03d", i), Activity: session.ActivityIdle})
		}
		if count > 0 {
			m.Selected = count - 1
		}
		m.Error = "fixture error"
		view := m.View(48, 10)
		if lines := strings.Count(view, "\n"); lines != 10 {
			t.Fatalf("count=%d lines=%d\n%s", count, lines, view)
		}
		for _, text := range []string{composerPrefix(session.ProviderClaude, ""), "Ctrl+X", "fixture error"} {
			if !strings.Contains(view, text) {
				t.Fatalf("count=%d missing %q\n%s", count, text, view)
			}
		}
		if count > 0 && !strings.Contains(view, rowPrefix(">", session.ActivityIdle, true, false)) {
			t.Fatalf("count=%d selected row not visible\n%s", count, view)
		}
	}
}
