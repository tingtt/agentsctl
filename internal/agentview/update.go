package agentview

import (
	"strings"

	"github.com/tingtt/agentsctl/internal/session"
)

// IntentKind is the UX-level meaning State.Handle resolved a KeyEvent
// into, given the current State -- the "Agent View state transition"
// half of the DesignDoc's terminal-bytes -> KeyEvent -> Agent View state
// -> Intent pipeline. Nothing downstream of Handle ever branches on a
// KeyEvent again.
type IntentKind int

const (
	IntentNone IntentKind = iota
	IntentDispatch
	IntentOpen
	IntentStop
	IntentArchive
	IntentRename
	IntentPin
	IntentRefresh
	IntentQuit
)

// Intent is one resolved user action for the orchestration loop
// (agentview_unix.go) to carry out via sessionctl.Controller. Everything
// State needed to decide provider-specific availability (row.Actions) has
// already been consulted inside Handle -- Intent itself carries only the
// target identity/payload, never a re-derivable provider condition.
type Intent struct {
	Kind     IntentKind
	Key      session.Key
	Provider session.ProviderID // IntentDispatch's target provider
	Prompt   string             // IntentDispatch
	Name     string             // IntentRename
}

// Handle resolves one physically-decoded KeyEvent into an Intent, given
// the current State -- the only place a key's meaning is decided. The
// same KeyEsc means "cancel rename" while renaming, "cancel confirmation"
// while one is pending, and "quit" otherwise; input_unix.go's decoder
// knows none of this.
func (s *State) Handle(ev KeyEvent) Intent {
	if s.Rename.Active {
		return s.handleRenameKey(ev)
	}
	if s.Confirmation != nil {
		return s.handleConfirmationKey(ev)
	}
	return s.handleNormalKey(ev)
}

func (s *State) handleNormalKey(ev KeyEvent) Intent {
	switch {
	case bindingProviderCycle.Matches(ev.Key):
		if s.Provider == session.ProviderClaude {
			s.Provider = session.ProviderCodex
		} else {
			s.Provider = session.ProviderClaude
		}
		return Intent{}
	case bindingNavigate.Matches(ev.Key):
		// #14: a multiline prompt gives Up/Down to in-prompt cursor
		// movement instead of session-list navigation -- resolved here in
		// State.Handle, not the terminal decoder (which only ever emits
		// physical KeyUp/KeyDown regardless of prompt content).
		if s.Composer.IsMultiline() {
			switch ev.Key {
			case KeyUp:
				s.Composer.CursorUp()
			case KeyDown:
				s.Composer.CursorDown()
			}
			return Intent{}
		}
		switch ev.Key {
		case KeyUp:
			if i := s.SelectedIndex(); i > 0 {
				s.selectIndex(i - 1)
				s.Confirmation = nil
			}
		case KeyDown:
			if i := s.SelectedIndex(); i >= 0 && i+1 < len(s.Rows) {
				s.selectIndex(i + 1)
				s.Confirmation = nil
			}
		}
		return Intent{}
	case ev.Key == KeyBackspace:
		s.Composer.Backspace()
		return Intent{}
	case ev.Key == KeyDelete:
		s.Composer.Delete()
		return Intent{}
	case ev.Key == KeyHome:
		s.Composer.Home()
		return Intent{}
	case ev.Key == KeyEnd:
		s.Composer.End()
		return Intent{}
	case ev.Key == KeyLeft:
		s.Composer.Left()
		return Intent{}
	case ev.Key == KeyRight:
		s.Composer.Right()
		return Intent{}
	case bindingStash.Matches(ev.Key):
		s.Composer.ToggleStash()
		return Intent{}
	case bindingSubmit.Matches(ev.Key):
		if strings.TrimSpace(s.Composer.Prompt) != "" {
			return Intent{Kind: IntentDispatch, Provider: s.Provider, Prompt: s.Composer.Prompt}
		}
		return s.openSelected()
	case bindingNewline.Matches(ev.Key):
		// Never dispatches (see the "submit" case above); the only way to
		// put an embedded newline into the composer.
		s.Composer.InsertAtCursor("\n")
		return Intent{}
	case bindingOpen.Matches(ev.Key):
		return s.openSelected()
	case bindingScope.Matches(ev.Key):
		s.Scope = nextScope(s.Scope)
		s.Confirmation = nil
		return Intent{Kind: IntentRefresh}
	case bindingStopArchive.Matches(ev.Key):
		return s.stopOrArchive()
	case bindingRename.Matches(ev.Key):
		row, ok := s.SelectedRow()
		if !ok {
			s.Error = "No session selected"
			return Intent{}
		}
		if !row.Actions.Available(session.ActionRename) {
			s.Error = capabilityReason(row, session.ActionRename, "rename")
			return Intent{}
		}
		s.startRename(row)
		return Intent{}
	case bindingPin.Matches(ev.Key):
		row, ok := s.SelectedRow()
		if !ok {
			s.Error = "No session selected"
			return Intent{}
		}
		return Intent{Kind: IntentPin, Key: row.Key}
	case bindingRefresh.Matches(ev.Key):
		return Intent{Kind: IntentRefresh}
	case bindingEscape.Matches(ev.Key):
		// #14's Esc priority: hide help first (never touching the prompt),
		// then clear a non-empty prompt, and only quit once both are
		// already empty/hidden.
		if s.HelpVisible {
			s.HelpVisible = false
			return Intent{}
		}
		if s.Composer.Prompt != "" {
			s.Composer.Clear()
			return Intent{}
		}
		return Intent{Kind: IntentQuit}
	case ev.Key == KeyRune && ev.Rune == '?' && s.Composer.Prompt == "" && !s.HelpVisible:
		// "?" opens help only on an empty prompt with help not already
		// shown; otherwise (prompt non-empty, or help already visible) it
		// is a plain prompt rune -- see the generic KeyRune case below.
		s.HelpVisible = true
		return Intent{}
	case ev.Key == KeyRune:
		s.Composer.InsertAtCursor(string(ev.Rune))
		return Intent{}
	}
	return Intent{}
}

func (s *State) handleConfirmationKey(ev KeyEvent) Intent {
	switch {
	case bindingStopArchive.Matches(ev.Key):
		return s.stopOrArchive()
	case bindingEscape.Matches(ev.Key):
		// The row confirmation disappearing is the feedback; no
		// notification (see State.Error's doc comment).
		s.Confirmation = nil
		s.Error = ""
		return Intent{}
	case bindingNavigate.Matches(ev.Key):
		// Moving the selection cancels a pending confirmation (no
		// "cancelled" notice -- it simply disappearing from its row as
		// selection moves off it is enough), then falls through to the
		// normal navigation this key would otherwise perform.
		s.Confirmation = nil
		return s.handleNormalKey(ev)
	}
	return Intent{}
}

func (s *State) handleRenameKey(ev KeyEvent) Intent {
	switch {
	case bindingEscape.Matches(ev.Key):
		// The inline editor closing and the row reverting to its
		// committed name is the feedback; no notification.
		s.Rename.cancel()
		s.Error = ""
	case ev.Key == KeyHome:
		s.Rename.home()
	case ev.Key == KeyEnd:
		s.Rename.end()
	case ev.Key == KeyLeft:
		s.Rename.left()
	case ev.Key == KeyRight:
		s.Rename.right()
	case ev.Key == KeyBackspace:
		s.Rename.backspace()
	case ev.Key == KeyDelete:
		s.Rename.delete()
	case bindingSubmit.Matches(ev.Key):
		if strings.TrimSpace(s.Rename.Draft) == "" {
			s.Error = "Name must not be empty"
			return Intent{}
		}
		return Intent{Kind: IntentRename, Key: s.Rename.Target, Name: s.Rename.Draft}
	case ev.Key == KeyRune:
		s.Rename.insert(string(ev.Rune))
	}
	return Intent{}
}

// openSelected resolves the common Open intent (see the DesignDoc's "Open
// as common Agent View intent"): Agent View only ever asks to open the
// selected session, never which provider transport to use.
func (s *State) openSelected() Intent {
	row, ok := s.SelectedRow()
	if !ok {
		s.Error = "No session selected"
		return Intent{}
	}
	if !row.Actions.Available(session.ActionOpen) {
		s.Error = capabilityReason(row, session.ActionOpen, "open")
		return Intent{}
	}
	return Intent{Kind: IntentOpen, Key: row.Key}
}

// stopOrArchive is Ctrl+X's single entry point, choosing Stop, arming an
// Archive confirmation, or resolving one, based purely on the selected
// row's own Actions -- never a provider-specific re-derivation (see the
// DesignDoc's "provider-specific condition を Agent View 内で再判定しな
// い").
func (s *State) stopOrArchive() Intent {
	row, ok := s.SelectedRow()
	if !ok {
		s.Error = "No session selected"
		return Intent{}
	}
	if row.Actions.Available(session.ActionStop) {
		s.Confirmation = nil
		return Intent{Kind: IntentStop, Key: row.Key}
	}
	if !row.Actions.Available(session.ActionArchive) {
		s.Error = capabilityReason(row, session.ActionArchive, "stop or archive")
		return Intent{}
	}
	if s.Confirmation == nil || s.Confirmation.Key != row.Key {
		s.Confirmation = &PendingConfirmation{Key: row.Key, Action: session.ActionArchive}
		s.Error = ""
		return Intent{}
	}
	s.Confirmation = nil
	s.Error = ""
	return Intent{Kind: IntentArchive, Key: row.Key}
}

func capabilityReason(row session.Session, action session.ActionID, label string) string {
	return "Cannot " + label + ": " + row.Actions.Reason(action)
}
