package agentview

import (
	"strings"
	"testing"

	"github.com/tingtt/agentsctl/internal/session"
)

func rowWith(k session.Key, actions session.Actions) session.Session {
	return session.Session{Key: k, Actions: actions}
}

func TestNavigationMovesSelectionByKeyNotIndex(t *testing.T) {
	s := NewState()
	s.SetRows([]session.Session{{Key: key("a")}, {Key: key("b")}, {Key: key("c")}})
	s.selectIndex(0)
	if intent := s.Handle(KeyEvent{Key: KeyDown}); intent.Kind != IntentNone {
		t.Fatalf("plain navigation must not produce an Intent: %+v", intent)
	}
	if s.SelectedIndex() != 1 {
		t.Fatalf("index=%d, want 1", s.SelectedIndex())
	}
	s.Handle(KeyEvent{Key: KeyUp})
	if s.SelectedIndex() != 0 {
		t.Fatalf("index=%d, want 0", s.SelectedIndex())
	}
	// Up at the top and Down at the bottom must not wrap or go out of range.
	s.Handle(KeyEvent{Key: KeyUp})
	if s.SelectedIndex() != 0 {
		t.Fatal("Up at the first row must not move selection")
	}
	s.selectIndex(2)
	s.Handle(KeyEvent{Key: KeyDown})
	if s.SelectedIndex() != 2 {
		t.Fatal("Down at the last row must not move selection")
	}
}

// interleavedDirectoryRows fixes the raw creation-time order that exposes
// the grouped-navigation bug: two directories' sessions interleaved in
// Rows (A1, B1, A2, B2), which groupRows regroups for display into
// contiguous per-directory runs (A1, A2, B1, B2). Raw-index navigation
// from A1 lands on B1 (Rows' next raw entry); visual-order navigation
// must land on A2 (the row immediately below A1 on screen).
func interleavedDirectoryRows() []session.Session {
	return []session.Session{
		rowAt(key("A1"), "/work/repo-a", false),
		rowAt(key("B1"), "/work/repo-b", false),
		rowAt(key("A2"), "/work/repo-a", false),
		rowAt(key("B2"), "/work/repo-b", false),
	}
}

// selectedName returns the DisplayName of the current selection, or "" if
// none, for compact assertions against the visual order fixtures above
// (whose sessions carry no Name/Summary, so DisplayName falls back to
// their Key.ID -- "A1", "B1", etc).
func selectedName(t *testing.T, s *State) string {
	t.Helper()
	row, ok := s.SelectedRow()
	if !ok {
		return ""
	}
	return row.DisplayName()
}

// TestGroupedNavigationFollowsVisualOrderDown is the primary regression for
// the directory-grouping bug: Down must walk the on-screen (grouped) order
// -- A1, A2, B1, B2 -- not Rows' raw interleaved order, which would skip
// A2 and jump straight from A1 to B1. Also covers the "last row" boundary
// (see the DesignDoc's selection guarantees / #14): a further Down at the
// last visual row must not move selection.
func TestGroupedNavigationFollowsVisualOrderDown(t *testing.T) {
	s := NewState()
	s.SetRows(interleavedDirectoryRows())
	s.selectIndex(0) // A1
	for _, want := range []string{"A2", "B1", "B2"} {
		s.Handle(KeyEvent{Key: KeyDown})
		if got := selectedName(t, &s); got != want {
			t.Fatalf("selected=%q, want %q", got, want)
		}
	}
	s.Handle(KeyEvent{Key: KeyDown})
	if got := selectedName(t, &s); got != "B2" {
		t.Fatalf("Down at the last visual row must not move selection: got %q", got)
	}
}

// TestGroupedNavigationFollowsVisualOrderUp is Down's mirror: Up from B2
// must walk B1, A2, A1 -- crossing back over the directory-group boundary
// -- and a further Up at the first visual row must not move selection.
func TestGroupedNavigationFollowsVisualOrderUp(t *testing.T) {
	s := NewState()
	s.SetRows(interleavedDirectoryRows())
	s.selectIndex(3) // B2
	for _, want := range []string{"B1", "A2", "A1"} {
		s.Handle(KeyEvent{Key: KeyUp})
		if got := selectedName(t, &s); got != want {
			t.Fatalf("selected=%q, want %q", got, want)
		}
	}
	s.Handle(KeyEvent{Key: KeyUp})
	if got := selectedName(t, &s); got != "A1" {
		t.Fatalf("Up at the first visual row must not move selection: got %q", got)
	}
}

// TestGroupedNavigationPinnedToDirectoryGroup fixes that Pinned sessions
// (a single group spanning every directory) and the per-directory groups
// beneath it form one continuous visual order: Down off the last Pinned
// row must land on the first row of the first directory group, and Up
// must return.
func TestGroupedNavigationPinnedToDirectoryGroup(t *testing.T) {
	s := NewState()
	s.SetRows([]session.Session{
		rowAt(key("P1"), "/work/repo-a", true),
		rowAt(key("P2"), "/work/repo-b", true),
		rowAt(key("A1"), "/work/repo-a", false),
		rowAt(key("B1"), "/work/repo-b", false),
	})
	s.selectIndex(1) // P2
	s.Handle(KeyEvent{Key: KeyDown})
	if got := selectedName(t, &s); got != "A1" {
		t.Fatalf("selected=%q, want A1 (visual next after Pinned)", got)
	}
	s.Handle(KeyEvent{Key: KeyUp})
	if got := selectedName(t, &s); got != "P2" {
		t.Fatalf("selected=%q, want P2 (visual previous, back into Pinned)", got)
	}
}

// TestConfirmationCancelsOnGroupedSelectionMove is
// TestConfirmationCancelsOnSelectionMove's multi-directory counterpart:
// moving off a row with an armed confirmation must cancel it and land on
// the visual-next row (A2), not Rows' raw-next row (B1) -- the same bug
// TestGroupedNavigationFollowsVisualOrderDown fixes, exercised through the
// confirmation path.
func TestConfirmationCancelsOnGroupedSelectionMove(t *testing.T) {
	s := NewState()
	s.SetRows([]session.Session{
		{Key: key("A1"), CWD: "/work/repo-a", Actions: session.Actions{session.ActionArchive: {Available: true}}},
		{Key: key("B1"), CWD: "/work/repo-b"},
		{Key: key("A2"), CWD: "/work/repo-a"},
	})
	s.selectIndex(0) // A1
	s.Handle(KeyEvent{Key: KeyCtrlX})
	s.Handle(KeyEvent{Key: KeyDown})
	if _, ok := s.rowNotice(key("A1")); ok {
		t.Fatal("moving selection must cancel a pending confirmation")
	}
	if got := selectedName(t, &s); got != "A2" {
		t.Fatalf("selected=%q, want visual-next A2 (not raw-next B1)", got)
	}
}

// TestGroupedRenderAndNavigationAgreeOnSessionOrder is the end-to-end
// guarantee: the session order View() actually renders top-to-bottom and
// the order repeated Down presses visit must be identical. This is the
// user-facing contract the bug broke (a visible row being skipped);
// asserting it directly, rather than only against the visualRowIndices
// helper, catches any future regression where render and navigation drift
// apart again regardless of how either is implemented internally.
func TestGroupedRenderAndNavigationAgreeOnSessionOrder(t *testing.T) {
	s := NewState()
	s.SetRows(interleavedDirectoryRows())
	want := []string{"A1", "A2", "B1", "B2"}

	view := s.View(80, 20)
	var rendered []string
	for _, line := range strings.Split(view, "\n") {
		for _, id := range want {
			if strings.Contains(line, id) {
				rendered = append(rendered, id)
				break
			}
		}
	}
	if strings.Join(rendered, ",") != strings.Join(want, ",") {
		t.Fatalf("rendered session order=%v, want %v\n%s", rendered, want, view)
	}

	s.selectIndex(0)
	navigated := []string{selectedName(t, &s)}
	for range want[1:] {
		s.Handle(KeyEvent{Key: KeyDown})
		navigated = append(navigated, selectedName(t, &s))
	}
	if strings.Join(navigated, ",") != strings.Join(want, ",") {
		t.Fatalf("navigated session order=%v, want %v", navigated, want)
	}
}

// TestMultilineUpDownMovesCursorNotSelection fixes #14's input-priority
// requirement: with a multiline prompt, Up/Down move the in-prompt cursor
// and must never move session selection, resolved in State.Handle (not
// the terminal decoder, which emits the same physical KeyUp/KeyDown either
// way).
func TestMultilineUpDownMovesCursorNotSelection(t *testing.T) {
	s := NewState()
	s.SetRows([]session.Session{{Key: key("a")}, {Key: key("b")}})
	s.selectIndex(0)
	s.Composer.Prompt = "first\nsecond"
	s.Composer.Cursor = len([]rune(s.Composer.Prompt)) // end of "second" (col 6)

	intent := s.Handle(KeyEvent{Key: KeyUp})
	if intent.Kind != IntentNone {
		t.Fatalf("intent=%+v, want none", intent)
	}
	if s.SelectedIndex() != 0 {
		t.Fatalf("SelectedIndex=%d, want unchanged 0 (multiline Up must not move selection)", s.SelectedIndex())
	}
	if want := len([]rune("first")); s.Composer.Cursor != want { // clamped onto "first"'s own EOL (col 5)
		t.Fatalf("Cursor=%d, want %d (moved onto \"first\", clamped to its EOL)", s.Composer.Cursor, want)
	}

	s.Handle(KeyEvent{Key: KeyDown})
	if s.SelectedIndex() != 0 {
		t.Fatalf("SelectedIndex=%d, want still unchanged 0", s.SelectedIndex())
	}
	if want := len([]rune("first\nsecond")); s.Composer.Cursor != want {
		t.Fatalf("Cursor=%d, want %d (back onto \"second\")", s.Composer.Cursor, want)
	}
}

// TestSingleLineUpDownStillMovesSessionSelectionEvenWithText fixes that
// the multiline-priority carve-out is scoped to an actual embedded
// newline: a non-empty but single-line prompt must still let Up/Down
// drive session-list navigation exactly like an empty one (see
// TestNavigationMovesSelectionByKeyNotIndex for the empty-composer case).
func TestSingleLineUpDownStillMovesSessionSelectionEvenWithText(t *testing.T) {
	s := NewState()
	s.SetRows([]session.Session{{Key: key("a")}, {Key: key("b")}})
	s.selectIndex(0)
	s.Composer.Prompt = "no newline here"
	s.Composer.Cursor = 5

	s.Handle(KeyEvent{Key: KeyDown})
	if s.SelectedIndex() != 1 {
		t.Fatalf("SelectedIndex=%d, want 1 (single-line prompt must not block selection)", s.SelectedIndex())
	}
	if s.Composer.Cursor != 5 {
		t.Fatalf("Cursor=%d, want unchanged 5 (single-line Up/Down must not touch the composer)", s.Composer.Cursor)
	}
}

func TestComposerEditingInsertsAndDeletes(t *testing.T) {
	s := NewState()
	for _, r := range "hi" {
		s.Handle(KeyEvent{Key: KeyRune, Rune: r})
	}
	if s.Composer.Prompt != "hi" {
		t.Fatalf("prompt=%q", s.Composer.Prompt)
	}
	s.Handle(KeyEvent{Key: KeyBackspace})
	if s.Composer.Prompt != "h" {
		t.Fatalf("prompt=%q after backspace", s.Composer.Prompt)
	}
	s.Handle(KeyEvent{Key: KeyNewline})
	if s.Composer.Prompt != "h\n" {
		t.Fatalf("prompt=%q after newline, want embedded newline not dispatch", s.Composer.Prompt)
	}
}

func TestEnterDispatchesWhenPromptNonEmpty(t *testing.T) {
	s := NewState()
	s.Provider = session.ProviderCodex
	s.Composer.Prompt = "do the thing"
	intent := s.Handle(KeyEvent{Key: KeyEnter})
	if intent.Kind != IntentDispatch || intent.Provider != session.ProviderCodex || intent.Prompt != "do the thing" {
		t.Fatalf("intent=%+v", intent)
	}
}

func TestEnterOpensSelectionWhenPromptEmpty(t *testing.T) {
	s := NewState()
	s.SetRows([]session.Session{rowWith(key("a"), session.Actions{session.ActionOpen: {Available: true}})})
	intent := s.Handle(KeyEvent{Key: KeyEnter})
	if intent.Kind != IntentOpen || intent.Key != key("a") {
		t.Fatalf("intent=%+v", intent)
	}
}

func TestOpenFailsClosedWhenUnavailableWithReason(t *testing.T) {
	s := NewState()
	s.SetRows([]session.Session{rowWith(key("a"), session.Actions{session.ActionOpen: {Reason: "external writer is not owned"}})})
	intent := s.Handle(KeyEvent{Key: KeyCtrlO})
	if intent.Kind != IntentNone {
		t.Fatalf("intent=%+v, want none", intent)
	}
	if s.Error == "" || !strings.Contains(s.Error, "external writer is not owned") {
		t.Fatalf("Error=%q, want it to surface the action's own reason", s.Error)
	}
}

// TestStopOrArchiveRequiresTwoPressesAndFollowsSessionKey covers the
// two-press confirmation lifecycle end to end: first press arms it
// (no Intent yet), second press on the SAME session resolves it.
func TestStopOrArchiveRequiresTwoPressesAndFollowsSessionKey(t *testing.T) {
	s := NewState()
	s.SetRows([]session.Session{rowWith(key("a"), session.Actions{session.ActionArchive: {Available: true}})})
	first := s.Handle(KeyEvent{Key: KeyCtrlX})
	if first.Kind != IntentNone {
		t.Fatalf("first press must not act yet: %+v", first)
	}
	notice, ok := s.rowNotice(key("a"))
	if !ok || notice.Severity != SeverityAlert {
		t.Fatalf("armed confirmation must render as an alert row notice: %+v ok=%v", notice, ok)
	}
	second := s.Handle(KeyEvent{Key: KeyCtrlX})
	if second.Kind != IntentArchive || second.Key != key("a") {
		t.Fatalf("second press must resolve to Archive: %+v", second)
	}
}

func TestStopOrArchiveStopsDirectlyWhenStopAvailable(t *testing.T) {
	s := NewState()
	s.SetRows([]session.Session{rowWith(key("a"), session.Actions{session.ActionStop: {Available: true}})})
	intent := s.Handle(KeyEvent{Key: KeyCtrlX})
	if intent.Kind != IntentStop || intent.Key != key("a") {
		t.Fatalf("intent=%+v, want immediate Stop (no confirmation needed)", intent)
	}
}

func TestConfirmationCancelsOnEsc(t *testing.T) {
	s := NewState()
	s.SetRows([]session.Session{rowWith(key("a"), session.Actions{session.ActionArchive: {Available: true}})})
	s.Handle(KeyEvent{Key: KeyCtrlX})
	s.Handle(KeyEvent{Key: KeyEsc})
	if _, ok := s.rowNotice(key("a")); ok {
		t.Fatal("Esc must cancel a pending confirmation")
	}
}

func TestConfirmationCancelsOnSelectionMove(t *testing.T) {
	s := NewState()
	s.SetRows([]session.Session{
		rowWith(key("a"), session.Actions{session.ActionArchive: {Available: true}}),
		rowWith(key("b"), session.Actions{session.ActionArchive: {Available: true}}),
	})
	s.Handle(KeyEvent{Key: KeyCtrlX}) // arm on "a"
	s.Handle(KeyEvent{Key: KeyDown})
	if _, ok := s.rowNotice(key("a")); ok {
		t.Fatal("moving selection must cancel a pending confirmation")
	}
	if s.SelectedIndex() != 1 {
		t.Fatal("the navigation itself must still take effect")
	}
}

// TestConfirmationSurvivesReload is the DesignDoc's explicit row-notice
// guarantee: "確認状態は session identity に紐付けるため...Refresh...を
// またいでも同じ session に追従する".
func TestConfirmationSurvivesReload(t *testing.T) {
	s := NewState()
	s.SetRows([]session.Session{rowWith(key("a"), session.Actions{session.ActionArchive: {Available: true}})})
	s.Handle(KeyEvent{Key: KeyCtrlX})
	// A reload returns the same session (possibly reordered/re-fetched).
	s.SetRows([]session.Session{rowWith(key("a"), session.Actions{session.ActionArchive: {Available: true}})})
	if _, ok := s.rowNotice(key("a")); !ok {
		t.Fatal("a pending confirmation must survive a catalog reload for its still-present target")
	}
}

func TestRenameModeRoutesKeysToTheEditorNotSelection(t *testing.T) {
	s := NewState()
	s.SetRows([]session.Session{
		rowWith(key("a"), session.Actions{session.ActionRename: {Available: true}}),
		rowWith(key("b"), session.Actions{}),
	})
	s.Handle(KeyEvent{Key: KeyCtrlR})
	if !s.Rename.Active || s.Rename.Target != key("a") {
		t.Fatalf("rename did not start: %+v", s.Rename)
	}
	// While renaming, Down must edit... actually Down has no rename
	// binding, so it must be a no-op, not move session selection.
	s.Handle(KeyEvent{Key: KeyDown})
	if s.SelectedIndex() != 0 {
		t.Fatal("session selection must not move while renaming")
	}
	for _, r := range "X" {
		s.Handle(KeyEvent{Key: KeyRune, Rune: r})
	}
	if s.Rename.Draft != "X" {
		t.Fatalf("rename draft=%q", s.Rename.Draft)
	}
	intent := s.Handle(KeyEvent{Key: KeyEnter})
	if intent.Kind != IntentRename || intent.Key != key("a") || intent.Name != "X" {
		t.Fatalf("intent=%+v", intent)
	}
}

func TestRenameRejectsEmptyName(t *testing.T) {
	s := NewState()
	s.SetRows([]session.Session{rowWith(key("a"), session.Actions{session.ActionRename: {Available: true}})})
	s.Handle(KeyEvent{Key: KeyCtrlR})
	// Delete the seeded draft (empty name) down to nothing.
	s.Rename.Draft = ""
	intent := s.Handle(KeyEvent{Key: KeyEnter})
	if intent.Kind != IntentNone || s.Error == "" {
		t.Fatalf("intent=%+v error=%q, want a rejected-empty-name error", intent, s.Error)
	}
}

func TestRenameEscCancelsWithoutIntent(t *testing.T) {
	s := NewState()
	s.SetRows([]session.Session{rowWith(key("a"), session.Actions{session.ActionRename: {Available: true}})})
	s.Handle(KeyEvent{Key: KeyCtrlR})
	intent := s.Handle(KeyEvent{Key: KeyEsc})
	if intent.Kind != IntentNone || s.Rename.Active {
		t.Fatalf("intent=%+v rename.Active=%v", intent, s.Rename.Active)
	}
}

func TestCtrlSlashCyclesScopeAndRequestsRefresh(t *testing.T) {
	s := NewState()
	if s.Scope != session.ScopeSame {
		t.Fatalf("initial scope=%v, want ScopeSame", s.Scope)
	}
	intent := s.Handle(KeyEvent{Key: KeyCtrlSlash})
	if intent.Kind != IntentRefresh || s.Scope != session.ScopeDescendants {
		t.Fatalf("intent=%+v scope=%v, want Refresh+ScopeDescendants", intent, s.Scope)
	}
	s.Handle(KeyEvent{Key: KeyCtrlSlash})
	if s.Scope != session.ScopeAll {
		t.Fatalf("scope=%v, want ScopeAll", s.Scope)
	}
	s.Handle(KeyEvent{Key: KeyCtrlSlash})
	if s.Scope != session.ScopeSame {
		t.Fatalf("scope=%v, want wrap back to ScopeSame", s.Scope)
	}
}

func TestCtrlGDoesNotChangeDirectoryScope(t *testing.T) {
	s := NewState()
	intent := s.Handle(KeyEvent{Key: KeyCtrlG})
	if intent.Kind != IntentNone || s.Scope != session.ScopeSame {
		t.Fatalf("intent=%+v scope=%v, want no intent and unchanged scope", intent, s.Scope)
	}
}

func TestCtrlTRequestsPinForSelectedSession(t *testing.T) {
	s := NewState()
	s.SetRows([]session.Session{{Key: key("a")}})
	intent := s.Handle(KeyEvent{Key: KeyCtrlT})
	if intent.Kind != IntentPin || intent.Key != key("a") {
		t.Fatalf("intent=%+v", intent)
	}
}

func TestCtrlLRequestsRefresh(t *testing.T) {
	s := NewState()
	if intent := s.Handle(KeyEvent{Key: KeyCtrlL}); intent.Kind != IntentRefresh {
		t.Fatalf("intent=%+v", intent)
	}
}

func TestShiftTabTogglesComposerProvider(t *testing.T) {
	s := NewState()
	if s.Provider != session.ProviderClaude {
		t.Fatalf("initial provider=%v, want claude", s.Provider)
	}
	s.Handle(KeyEvent{Key: KeyShiftTab})
	if s.Provider != session.ProviderCodex {
		t.Fatalf("provider=%v, want codex", s.Provider)
	}
	s.Handle(KeyEvent{Key: KeyShiftTab})
	if s.Provider != session.ProviderClaude {
		t.Fatalf("provider=%v, want claude", s.Provider)
	}
}

func TestEscQuitsOnlyOutsideRenameAndConfirmation(t *testing.T) {
	s := NewState()
	if intent := s.Handle(KeyEvent{Key: KeyEsc}); intent.Kind != IntentQuit {
		t.Fatalf("plain Esc must quit: %+v", intent)
	}
}

// TestHelpVisibleEscWithEmptyPromptOnlyHidesHelp fixes #14's fixed Esc
// priority: help visible always wins first, so Esc must only hide help --
// never fall through to the "empty prompt -> quit" behavior it would
// otherwise trigger.
func TestHelpVisibleEscWithEmptyPromptOnlyHidesHelp(t *testing.T) {
	s := NewState()
	s.HelpVisible = true
	intent := s.Handle(KeyEvent{Key: KeyEsc})
	if intent.Kind != IntentNone {
		t.Fatalf("intent=%+v, want none (help-hiding Esc must not quit)", intent)
	}
	if s.HelpVisible {
		t.Fatal("help must be hidden after Esc")
	}
}

// TestHelpVisibleEscWithNonEmptyPromptPreservesPrompt fixes that a
// help-hiding Esc must not also clear the prompt, even though a non-empty
// prompt would normally be Esc's next priority once help is out of the way.
func TestHelpVisibleEscWithNonEmptyPromptPreservesPrompt(t *testing.T) {
	s := NewState()
	s.HelpVisible = true
	s.Composer.Prompt = "hoge"
	intent := s.Handle(KeyEvent{Key: KeyEsc})
	if intent.Kind != IntentNone {
		t.Fatalf("intent=%+v, want none", intent)
	}
	if s.HelpVisible {
		t.Fatal("help must be hidden after Esc")
	}
	if s.Composer.Prompt != "hoge" {
		t.Fatalf("prompt=%q, want unchanged (help-hiding Esc must not clear it)", s.Composer.Prompt)
	}
	// The next Esc, with help now hidden, falls through to normal priority:
	// a non-empty prompt gets cleared, not quit.
	intent = s.Handle(KeyEvent{Key: KeyEsc})
	if intent.Kind != IntentNone {
		t.Fatalf("intent=%+v, want none (clearing the prompt, not quitting)", intent)
	}
	if s.Composer.Prompt != "" {
		t.Fatalf("prompt=%q, want cleared by the next Esc now that help is hidden", s.Composer.Prompt)
	}
}

// TestHelpVisibleEscWithPendingConfirmationPreservesConfirmation fixes that
// a help-hiding Esc must not also cancel a pending archive confirmation,
// even though Confirmation != nil would otherwise route Esc to
// handleConfirmationKey ahead of the normal-state Esc priority.
func TestHelpVisibleEscWithPendingConfirmationPreservesConfirmation(t *testing.T) {
	s := NewState()
	s.SetRows([]session.Session{rowWith(key("a"), session.Actions{session.ActionArchive: {Available: true}})})
	s.Handle(KeyEvent{Key: KeyCtrlX}) // arm the confirmation on "a"
	s.HelpVisible = true
	intent := s.Handle(KeyEvent{Key: KeyEsc})
	if intent.Kind != IntentNone {
		t.Fatalf("intent=%+v, want none", intent)
	}
	if s.HelpVisible {
		t.Fatal("help must be hidden after Esc")
	}
	if _, ok := s.rowNotice(key("a")); !ok {
		t.Fatal("pending confirmation must survive a help-hiding Esc")
	}
	// The next Esc, with help now hidden, falls through to normal priority
	// and cancels the confirmation as usual.
	s.Handle(KeyEvent{Key: KeyEsc})
	if _, ok := s.rowNotice(key("a")); ok {
		t.Fatal("the following Esc (help already hidden) must cancel the confirmation as usual")
	}
}

// TestHelpVisibleEscWithActiveRenamePreservesRename fixes that a
// help-hiding Esc must not also cancel an in-progress rename, even though
// Rename.Active would otherwise route Esc to handleRenameKey ahead of the
// normal-state Esc priority.
func TestHelpVisibleEscWithActiveRenamePreservesRename(t *testing.T) {
	s := NewState()
	s.SetRows([]session.Session{rowWith(key("a"), session.Actions{session.ActionRename: {Available: true}})})
	s.Handle(KeyEvent{Key: KeyCtrlR}) // start renaming "a"
	s.HelpVisible = true
	intent := s.Handle(KeyEvent{Key: KeyEsc})
	if intent.Kind != IntentNone {
		t.Fatalf("intent=%+v, want none", intent)
	}
	if s.HelpVisible {
		t.Fatal("help must be hidden after Esc")
	}
	if !s.Rename.Active || s.Rename.Target != key("a") {
		t.Fatalf("rename must survive a help-hiding Esc: %+v", s.Rename)
	}
	// The next Esc, with help now hidden, falls through to normal priority
	// and cancels the rename as usual.
	s.Handle(KeyEvent{Key: KeyEsc})
	if s.Rename.Active {
		t.Fatal("the following Esc (help already hidden) must cancel the rename as usual")
	}
}
