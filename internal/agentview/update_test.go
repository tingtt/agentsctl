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

func TestEscQuitsOnlyOutsideRenameAndConfirmation(t *testing.T) {
	s := NewState()
	if intent := s.Handle(KeyEvent{Key: KeyEsc}); intent.Kind != IntentQuit {
		t.Fatalf("plain Esc must quit: %+v", intent)
	}
}
