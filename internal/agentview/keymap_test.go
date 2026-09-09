package agentview

import (
	"testing"

	"github.com/tingtt/agentsctl/internal/session"
)

// TestBindingMatchesOwnsPhysicalKeyMembership fixes that Matches is the
// membership test for a shortcut's physical Key(s): true for every Key the
// binding lists, false for a representative sample of Keys it doesn't --
// so State.Handle's bindingX.Matches(ev.Key) branches (update.go) and this
// test both exercise the same membership Binding owns.
func TestBindingMatchesOwnsPhysicalKeyMembership(t *testing.T) {
	if !bindingPin.Matches(KeyCtrlT) {
		t.Fatal("bindingPin must match KeyCtrlT")
	}
	if bindingPin.Matches(KeyCtrlR) {
		t.Fatal("bindingPin must not match KeyCtrlR")
	}
	if !bindingEscape.Matches(KeyEsc) {
		t.Fatal("bindingEscape must match KeyEsc")
	}
	if bindingEscape.Matches(KeyCtrlX) {
		t.Fatal("bindingEscape must not match KeyCtrlX")
	}
	if !bindingNavigate.Matches(KeyUp) || !bindingNavigate.Matches(KeyDown) {
		t.Fatal("bindingNavigate must match both KeyUp and KeyDown")
	}
	if bindingNavigate.Matches(KeyLeft) {
		t.Fatal("bindingNavigate must not match KeyLeft")
	}
	if !bindingStopArchive.Matches(KeyCtrlX) {
		t.Fatal("bindingStopArchive must match KeyCtrlX")
	}
	if !bindingPromptEditor.Matches(KeyCtrlG) || bindingScope.Matches(KeyCtrlG) {
		t.Fatal("Ctrl+G must belong only to the prompt editor binding")
	}
	if !bindingScope.Matches(KeyCtrlSlash) || bindingPromptEditor.Matches(KeyCtrlSlash) {
		t.Fatal("Ctrl+/ must belong only to the directory-scope binding")
	}
}

// TestEscPriorityOrder fixes #14's Esc priority order end to end: help
// visible always wins (hiding help without touching the prompt), then a
// non-empty prompt is cleared, and only once both are already empty/hidden
// does Esc quit. Also covers the pre-existing rename/confirmation-modal
// cases, which Esc must still resolve before ever reaching normal-mode
// logic.
func TestEscPriorityOrder(t *testing.T) {
	// Help visible, with a non-empty prompt: Esc hides help only, leaving
	// the prompt untouched.
	help := NewState()
	help.Composer.Prompt = "hello"
	help.HelpVisible = true
	if intent := help.Handle(KeyEvent{Key: KeyEsc}); intent.Kind != IntentNone || help.HelpVisible {
		t.Fatalf("help+Esc: intent=%+v HelpVisible=%v, want hidden with no Intent", intent, help.HelpVisible)
	}
	if help.Composer.Prompt != "hello" {
		t.Fatalf("help+Esc must not touch the prompt: %q", help.Composer.Prompt)
	}

	// Help hidden, non-empty prompt: Esc clears the prompt, no Intent.
	clear := NewState()
	clear.Composer.Prompt = "hello"
	if intent := clear.Handle(KeyEvent{Key: KeyEsc}); intent.Kind != IntentNone {
		t.Fatalf("prompt+Esc: intent=%+v, want none", intent)
	}
	if clear.Composer.Prompt != "" {
		t.Fatalf("prompt+Esc must clear the prompt: %q", clear.Composer.Prompt)
	}

	// Help hidden, empty prompt: Esc quits.
	quit := NewState()
	if intent := quit.Handle(KeyEvent{Key: KeyEsc}); intent.Kind != IntentQuit {
		t.Fatalf("empty+Esc: intent=%+v, want IntentQuit", intent)
	}

	// Rename active: Esc cancels the rename, no Intent (unaffected by help
	// priority -- Handle dispatches to rename handling before ever
	// consulting HelpVisible).
	rename := NewState()
	rename.SetRows([]session.Session{rowWith(key("a"), session.Actions{session.ActionRename: {Available: true}})})
	rename.Handle(KeyEvent{Key: KeyCtrlR})
	if intent := rename.Handle(KeyEvent{Key: KeyEsc}); intent.Kind != IntentNone || rename.Rename.Active {
		t.Fatalf("rename+Esc: intent=%+v rename.Active=%v, want cancelled rename with no Intent", intent, rename.Rename.Active)
	}

	// Confirmation pending: Esc cancels the confirmation, no Intent.
	confirm := NewState()
	confirm.SetRows([]session.Session{rowWith(key("a"), session.Actions{session.ActionArchive: {Available: true}})})
	confirm.Handle(KeyEvent{Key: KeyCtrlX})
	if intent := confirm.Handle(KeyEvent{Key: KeyEsc}); intent.Kind != IntentNone || confirm.Confirmation != nil {
		t.Fatalf("confirmation+Esc: intent=%+v Confirmation=%+v, want cancelled confirmation with no Intent", intent, confirm.Confirmation)
	}
}

// TestQuestionMarkTogglesHelpOnlyOnEmptyPrompt fixes #14's "?" behavior:
// it opens help only when the prompt is empty and help isn't already
// shown; otherwise it's a plain prompt rune.
func TestQuestionMarkTogglesHelpOnlyOnEmptyPrompt(t *testing.T) {
	empty := NewState()
	if intent := empty.Handle(KeyEvent{Key: KeyRune, Rune: '?'}); intent.Kind != IntentNone || !empty.HelpVisible {
		t.Fatalf("empty prompt+?: intent=%+v HelpVisible=%v, want help shown", intent, empty.HelpVisible)
	}
	if empty.Composer.Prompt != "" {
		t.Fatalf("? must not be inserted into the prompt when it opens help: %q", empty.Composer.Prompt)
	}

	nonEmpty := NewState()
	nonEmpty.Composer.Prompt = "hi"
	nonEmpty.Composer.Cursor = 2
	nonEmpty.Handle(KeyEvent{Key: KeyRune, Rune: '?'})
	if nonEmpty.HelpVisible {
		t.Fatal("? on a non-empty prompt must not open help")
	}
	if nonEmpty.Composer.Prompt != "hi?" {
		t.Fatalf("? on a non-empty prompt must be inserted as a plain rune: %q", nonEmpty.Composer.Prompt)
	}

	alreadyOpen := NewState()
	alreadyOpen.HelpVisible = true
	alreadyOpen.Handle(KeyEvent{Key: KeyRune, Rune: '?'})
	if alreadyOpen.Composer.Prompt != "?" {
		t.Fatalf("? while help is already visible must be a plain prompt rune: %q", alreadyOpen.Composer.Prompt)
	}
}

// TestTypingWhileHelpVisibleDoesNotCloseIt fixes that help stays open
// while the user keeps typing -- only Esc closes it.
func TestTypingWhileHelpVisibleDoesNotCloseIt(t *testing.T) {
	s := NewState()
	s.HelpVisible = true
	for _, r := range "hello" {
		s.Handle(KeyEvent{Key: KeyRune, Rune: r})
	}
	if !s.HelpVisible {
		t.Fatal("typing must not close help")
	}
	if s.Composer.Prompt != "hello" {
		t.Fatalf("prompt=%q, want typed text to still land in the composer", s.Composer.Prompt)
	}
}

// TestBindingsCoverEveryShortcutKey is the drift guard: every physical Key
// input_unix.go can decode is either a documented shortcut (present in
// allBindings, and reachable from State.Handle's normal-mode switch) or an
// explicitly-listed non-shortcut key (raw composer/rename text editing, or
// a key with no assigned meaning at all). A newly-added Key that is
// neither listed here nor added to allBindings fails this test, so
// State.Handle and the displayed shortcuts cannot silently drift apart the
// way two independently-hardcoded copies of "what does Ctrl+X do" could
// (see the DesignDoc's shortcut source-of-truth requirement).
func TestBindingsCoverEveryShortcutKey(t *testing.T) {
	// The complete Key enum (input_unix.go). Keep in sync by construction:
	// a Key added there and omitted here is still caught, just as
	// KeyUnknown below, because it will be neither a documented shortcut
	// nor a listed non-shortcut key.
	all := []Key{
		KeyRune, KeyEnter, KeyNewline, KeyBackspace, KeyDelete, KeyHome, KeyEnd,
		KeyLeft, KeyRight, KeyUp, KeyDown, KeyShiftTab, KeyEsc, KeyCtrlG, KeyCtrlSlash, KeyCtrlX,
		KeyCtrlR, KeyCtrlT, KeyCtrlS, KeyCtrlL, KeyCtrlO, KeyUnknown,
	}
	// Composer/rename text-editing motions and typing, plus keys with no
	// assigned meaning: not "shortcuts" in the footer/help sense, so they
	// carry no Binding.
	nonShortcut := map[Key]bool{
		KeyRune: true, KeyBackspace: true, KeyDelete: true, KeyHome: true,
		KeyEnd: true, KeyLeft: true, KeyRight: true, KeyUnknown: true,
	}
	documented := map[Key]bool{}
	for _, b := range allBindings {
		for _, k := range b.Keys {
			documented[k] = true
		}
	}
	for _, k := range all {
		if nonShortcut[k] || documented[k] {
			continue
		}
		t.Errorf("Key %d is neither a documented shortcut (allBindings) nor listed as a non-shortcut key -- update keymap.go or this test's nonShortcut set", k)
	}
	// And the reverse: every documented shortcut must be one of the
	// recognized Key values above (catches a stale/typo'd Key in a
	// Binding).
	for k := range documented {
		found := false
		for _, a := range all {
			if a == k {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("Binding references Key %d, which is not in the complete Key enum", k)
		}
	}
}
