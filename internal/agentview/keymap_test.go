package agentview

import (
	"slices"
	"strings"
	"testing"

	"github.com/tingtt/agentsctl/internal/session"
)

// equalBinding reports whether got and want are the same Binding, physical
// Keys included -- Label/Desc alone can't catch a Keys-only drift (e.g.
// footerLine1 losing sync with a named binding's Keys while keeping its
// Label/Desc).
func equalBinding(got, want Binding) bool {
	return got.Label == want.Label &&
		got.Desc == want.Desc &&
		slices.Equal(got.Keys, want.Keys)
}

// TestFooterLinesMatchCentralizedBindings fixes the exact footer text
// against the Binding definitions it is built from, so a label/description
// typo shows up as a Binding change, not a silently-diverged literal
// string in render.go.
func TestFooterLinesMatchCentralizedBindings(t *testing.T) {
	if got, want := footerText(footerLine1), "Shift+Tab / Enter send/open / Option+Enter/Shift+Enter newline / Ctrl+S stash / Ctrl+O / Ctrl+T pin / Ctrl+/ depth"; got != want {
		t.Fatalf("footerLine1 = %q, want %q", got, want)
	}
	if got, want := footerText(footerLine2), "↑↓ / Ctrl+G scope / Ctrl+R rename / Ctrl+X stop/archive / Ctrl+L refresh / Esc quit"; got != want {
		t.Fatalf("footerLine2 = %q, want %q", got, want)
	}
}

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
}

// TestFooterComposedFromNamedBindings fixes that footerLine1/footerLine2
// are built only from the named binding variables -- not a second,
// independently-hardcoded physical-key list -- by checking each footer
// slot is == (same Label/Desc/Keys) to its named counterpart.
func TestFooterComposedFromNamedBindings(t *testing.T) {
	want1 := []Binding{
		bindingProviderCycle, bindingSubmit, bindingNewline, bindingStash,
		bindingOpen, bindingPin, bindingDepth,
	}
	if len(footerLine1) != len(want1) {
		t.Fatalf("footerLine1 has %d entries, want %d", len(footerLine1), len(want1))
	}
	for i, b := range want1 {
		if !equalBinding(footerLine1[i], b) {
			t.Fatalf("footerLine1[%d] = %+v, want %+v", i, footerLine1[i], b)
		}
	}
	want2 := []Binding{
		bindingNavigate, bindingScope, bindingRename, bindingStopArchive,
		bindingRefresh, bindingEscape,
	}
	if len(footerLine2) != len(want2) {
		t.Fatalf("footerLine2 has %d entries, want %d", len(footerLine2), len(want2))
	}
	for i, b := range want2 {
		if !equalBinding(footerLine2[i], b) {
			t.Fatalf("footerLine2[%d] = %+v, want %+v", i, footerLine2[i], b)
		}
	}
}

// TestEscBindingMeaningIsStateDependent is the state-dependent-semantics
// guard: bindingEscape's physical Key (KeyEsc) is the same in all three
// cases below, yet State.Handle resolves it to a different Intent (or
// none) depending purely on State -- proof that Binding owns only "this is
// the Esc key", never "Esc means quit".
func TestEscBindingMeaningIsStateDependent(t *testing.T) {
	// Rename active: Esc cancels the rename, no Intent.
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

	// Normal mode: Esc quits.
	normal := NewState()
	if intent := normal.Handle(KeyEvent{Key: KeyEsc}); intent.Kind != IntentQuit {
		t.Fatalf("normal+Esc: intent=%+v, want IntentQuit", intent)
	}
}

// TestKnownBindingLabels fixes a representative sample's exact Label/Desc,
// including a bare binding (no Desc) and a multi-key binding (↑↓), so a
// future edit to keymap.go can't silently change what the footer shows for
// these without a test failing.
func TestKnownBindingLabels(t *testing.T) {
	cases := []struct {
		bindings []Binding
		label    string
		want     Binding
	}{
		{footerLine1, "Ctrl+O", Binding{Label: "Ctrl+O", Keys: []Key{KeyCtrlO}}},
		{footerLine1, "Ctrl+T", Binding{Label: "Ctrl+T", Desc: "pin", Keys: []Key{KeyCtrlT}}},
		{footerLine2, "↑↓", Binding{Label: "↑↓", Keys: []Key{KeyUp, KeyDown}}},
		{footerLine2, "Esc", Binding{Label: "Esc", Desc: "quit", Keys: []Key{KeyEsc}}},
	}
	for _, c := range cases {
		var found *Binding
		for i := range c.bindings {
			if c.bindings[i].Label == c.label {
				found = &c.bindings[i]
				break
			}
		}
		if found == nil {
			t.Fatalf("no binding labeled %q", c.label)
		}
		if found.Desc != c.want.Desc || len(found.Keys) != len(c.want.Keys) {
			t.Fatalf("binding %q = %+v, want %+v", c.label, *found, c.want)
		}
		for i, k := range found.Keys {
			if k != c.want.Keys[i] {
				t.Fatalf("binding %q Keys = %v, want %v", c.label, found.Keys, c.want.Keys)
			}
		}
	}
}

// TestFooterUsesCentralizedDefinitions fixes that View's rendered footer
// rows are exactly footerText(footerLine1)/footerText(footerLine2) --
// render.go must read the shared Binding definitions, not its own copy of
// the text.
func TestFooterUsesCentralizedDefinitions(t *testing.T) {
	s := NewState()
	view := s.View(200, 12)
	lines := strings.Split(strings.TrimRight(view, "\n"), "\n")
	if len(lines) < 2 {
		t.Fatalf("view has too few lines: %q", view)
	}
	last2 := lines[len(lines)-2:]
	if last2[0] != footerText(footerLine1) {
		t.Fatalf("footer line 1 = %q, want %q", last2[0], footerText(footerLine1))
	}
	if last2[1] != footerText(footerLine2) {
		t.Fatalf("footer line 2 = %q, want %q", last2[1], footerText(footerLine2))
	}
}

// TestBindingsCoverEveryShortcutKey is the drift guard: every physical Key
// input_unix.go can decode is either a documented shortcut (present in
// footerLine1/footerLine2, and reachable from State.Handle's normal-mode
// switch) or an explicitly-listed non-shortcut key (raw composer/rename
// text editing, or a key with no assigned meaning at all). A newly-added
// Key that is neither listed here nor added to a footer line fails this
// test, so State.Handle and the footer cannot silently drift apart the way
// two independently-hardcoded copies of "what does Ctrl+X do" could (see
// the DesignDoc's shortcut source-of-truth requirement).
func TestBindingsCoverEveryShortcutKey(t *testing.T) {
	// The complete Key enum (input_unix.go). Keep in sync by construction:
	// a Key added there and omitted here is still caught, just as
	// KeyUnknown below, because it will be neither a documented shortcut
	// nor a listed non-shortcut key.
	all := []Key{
		KeyRune, KeyEnter, KeyNewline, KeyBackspace, KeyDelete, KeyHome, KeyEnd,
		KeyLeft, KeyRight, KeyUp, KeyDown, KeyShiftTab, KeyEsc, KeyCtrlG, KeyCtrlX,
		KeyCtrlR, KeyCtrlT, KeyCtrlS, KeyCtrlL, KeyCtrlO, KeyCtrlSlash, KeyUnknown,
	}
	// Composer/rename text-editing motions and typing, plus keys with no
	// assigned meaning: not "shortcuts" in the footer/help sense, so they
	// carry no Binding.
	nonShortcut := map[Key]bool{
		KeyRune: true, KeyBackspace: true, KeyDelete: true, KeyHome: true,
		KeyEnd: true, KeyLeft: true, KeyRight: true, KeyUnknown: true,
	}
	documented := map[Key]bool{}
	for _, bindings := range [][]Binding{footerLine1, footerLine2} {
		for _, b := range bindings {
			for _, k := range b.Keys {
				documented[k] = true
			}
		}
	}
	for _, k := range all {
		if nonShortcut[k] || documented[k] {
			continue
		}
		t.Errorf("Key %d is neither a documented shortcut (footerLine1/footerLine2) nor listed as a non-shortcut key -- update keymap.go or this test's nonShortcut set", k)
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
