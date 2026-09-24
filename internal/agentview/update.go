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
	IntentOpenPromptEditor
	IntentOpen
	IntentStop
	IntentArchive
	IntentRename
	IntentPin
	IntentRefresh
	IntentUpdate
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
	Version  string             // IntentUpdate: the exact advertised release to install
}

// HandleInput resolves one decoded InputEvent: a key goes through Handle, a
// paste through HandlePaste. The two never convert into each other -- a
// paste's newlines are content, not Enter.
func (s *State) HandleInput(ev InputEvent) Intent {
	if ev.Kind == InputPaste {
		return s.HandlePaste(ev.Paste)
	}
	return s.Handle(ev.Key)
}

// HandlePaste applies one bracketed paste to the current text-edit target
// (the inline rename editor while one is active, otherwise the composer) at
// its cursor. A paste only ever mutates an editor: it never returns a
// dispatch, open, or rename intent, however many newlines it contains --
// submitting is a separate, later physical Enter.
func (s *State) HandlePaste(text string) Intent {
	text = normalizePastedNewlines(text)
	switch {
	case s.Rename.Active:
		// Session names are single-line (Claude applies a rename by typing
		// "/rename <name>" into a PTY, where a newline would submit early),
		// so newlines fold into spaces rather than being stored or submitting.
		s.Rename.insert(foldPastedLines(text))
	case s.Confirmation != nil:
		// Like typed runes, text has no meaning while a confirmation is pending.
	default:
		s.Composer.InsertAtCursor(text)
	}
	return Intent{}
}

// normalizePastedNewlines converts pasted text to the editor's internal "\n"
// newline: CRLF first (so it never becomes two newlines), then a lone CR.
func normalizePastedNewlines(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	return strings.ReplaceAll(text, "\r", "\n")
}

// foldPastedLines joins the non-empty lines of newline-normalized text with
// one space each, dropping leading, trailing, and repeated newlines.
func foldPastedLines(text string) string {
	var lines []string
	for _, line := range strings.Split(text, "\n") {
		if line != "" {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, " ")
}

// Handle resolves one physically-decoded KeyEvent into an Intent, given
// the current State -- the only place a key's meaning is decided. The
// same KeyEsc means "cancel rename" while renaming, "cancel confirmation"
// while one is pending, and "quit" otherwise; input_unix.go's decoder
// knows none of this.
//
// Esc while HelpVisible is intercepted here, ahead of Rename/Confirmation
// routing, per #14's fixed priority: help visible always hides help first
// -- regardless of a pending rename or archive confirmation -- touching
// neither the prompt nor that rename/confirmation state. Only once help is
// hidden does a later Esc reach handleRenameKey/handleConfirmationKey/
// handleNormalKey to cancel rename, cancel confirmation, clear the prompt,
// or quit.
func (s *State) Handle(ev KeyEvent) Intent {
	if s.HelpVisible && bindingEscape.Matches(ev.Key) {
		s.HelpVisible = false
		return Intent{}
	}
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
			if s.moveSelection(-1) {
				s.Confirmation = nil
			}
		case KeyDown:
			if s.moveSelection(1) {
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
	case ev.Key == KeyLeft && s.Composer.Prompt != "":
		s.Composer.Left()
		return Intent{}
	case ev.Key == KeyRight && s.Composer.Prompt != "":
		s.Composer.Right()
		return Intent{}
	case s.isIssue48Navigation(ev):
		s.handleIssue48Navigation(ev)
		return Intent{}
	case bindingStash.Matches(ev.Key):
		s.Composer.ToggleStash()
		return Intent{}
	case bindingSubmit.Matches(ev.Key):
		if strings.TrimSpace(s.Composer.Prompt) != "" {
			// agentsctl-owned commands are resolved here and never reach a
			// provider's Dispatch.
			if intent, handled := s.handleUpdateCommand(s.Composer.Prompt); handled {
				return intent
			}
			return Intent{Kind: IntentDispatch, Provider: s.Provider, Prompt: s.Composer.Prompt}
		}
		if s.expandSelectedControl() {
			return Intent{}
		}
		return s.openSelected()
	case bindingNewline.Matches(ev.Key):
		// Never dispatches (see the "submit" case above); the only way to
		// put an embedded newline into the composer.
		s.Composer.InsertAtCursor("\n")
		return Intent{}
	case bindingPromptEditor.Matches(ev.Key):
		return Intent{Kind: IntentOpenPromptEditor}
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
		// Help-visible Esc is intercepted by Handle before routing here
		// (see Handle's doc comment) -- HelpVisible is always false by the
		// time execution reaches this branch. What remains is #14's next
		// priority: clear a non-empty prompt, or quit once it's empty too.
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

func (s State) isIssue48Navigation(ev KeyEvent) bool {
	return s.Composer.Prompt == "" && (bindingFoldExpand.Matches(ev.Key) || bindingGroupNavigate.MatchesEvent(ev))
}

func (s *State) handleIssue48Navigation(ev KeyEvent) bool {
	switch {
	case ev.Key == KeyLeft:
		return s.foldSelectedGroup()
	case ev.Key == KeyRight:
		return s.expandSelectedControl()
	case ev.Rune == '{':
		return s.moveGroup(-1)
	case ev.Rune == '}':
		return s.moveGroup(1)
	default:
		return false
	}
}

func (s *State) foldSelectedGroup() bool {
	if !s.hasCursor || s.cursor.kind != listItemSession {
		return false
	}
	model := s.selectableList()
	_, group, ok := model.groupForItem(s.cursor)
	if !ok {
		return false
	}
	position := -1
	for i, rowIndex := range group.indices {
		if s.Rows[rowIndex].Key == s.cursor.sessionKey {
			position = i
			break
		}
	}
	if position < 0 {
		return false
	}
	state := s.groupStates[group.id]
	if group.id.pinned || position < directoryPageSize {
		state.folded = true
		s.setGroupState(group.id, state)
		s.cursor = controlItemID(group.id, listItemShowSessions)
		return true
	}
	state.folded = false
	state.visibleCount = position / directoryPageSize * directoryPageSize
	s.setGroupState(group.id, state)
	s.cursor = controlItemID(group.id, listItemShowMore)
	return true
}

func (s *State) expandSelectedControl() bool {
	if !s.hasCursor || s.cursor.kind == listItemSession {
		return false
	}
	model := s.selectableList()
	_, group, ok := model.groupForItem(s.cursor)
	if !ok || len(group.indices) == 0 {
		return false
	}
	state := s.groupStates[group.id]
	switch s.cursor.kind {
	case listItemShowSessions:
		state.folded = false
		if !group.id.pinned {
			state.visibleCount = directoryPageSize
		}
		s.setGroupState(group.id, state)
		s.cursor = sessionItemID(s.Rows[group.indices[0]].Key)
		return true
	case listItemShowMore:
		visible := 0
		for _, item := range group.items {
			if item.id.kind == listItemSession {
				visible++
			}
		}
		if visible >= len(group.indices) {
			return false
		}
		state.folded = false
		state.visibleCount = visible + directoryPageSize
		s.setGroupState(group.id, state)
		s.cursor = sessionItemID(s.Rows[group.indices[visible]].Key)
		return true
	default:
		return false
	}
}

func (s *State) moveGroup(delta int) bool {
	if !s.hasCursor || delta == 0 {
		return false
	}
	model := s.selectableList()
	current, _, ok := model.groupForItem(s.cursor)
	if !ok {
		return false
	}
	target := current + delta
	if target < 0 || target >= len(model.groups) {
		group := model.groups[current]
		if len(group.items) == 0 {
			return false
		}
		targetItem := group.items[0]
		if delta > 0 {
			targetItem = group.items[len(group.items)-1]
		}
		if targetItem.id == s.cursor {
			return false
		}
		s.cursor = targetItem.id
		s.hasCursor = true
		return true
	}
	group := model.groups[target]
	if len(group.items) == 0 {
		return false
	}
	if delta > 0 {
		s.cursor = group.items[0].id
		s.hasCursor = true
		return true
	}
	for i := len(group.items) - 1; i >= 0; i-- {
		if group.items[i].id.kind == listItemShowMore {
			continue
		}
		s.cursor = group.items[i].id
		s.hasCursor = true
		return true
	}
	return false
}

// moveSelection steps the list cursor through the same selectable model View
// renders. Headings and separators are absent, while group controls are real
// destinations alongside sessions.
func (s *State) moveSelection(delta int) bool {
	model := s.selectableList()
	pos := -1
	for i, item := range model.items {
		if s.hasCursor && item.id == s.cursor {
			pos = i
			break
		}
	}
	if pos == -1 {
		return false
	}
	next := pos + delta
	if next < 0 || next >= len(model.items) {
		return false
	}
	s.cursor, s.hasCursor = model.items[next].id, true
	return true
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
	case s.isIssue48Navigation(ev):
		if s.handleIssue48Navigation(ev) {
			s.Confirmation = nil
		}
		return Intent{}
	}
	return Intent{}
}

func (s *State) handleRenameKey(ev KeyEvent) Intent {
	switch {
	case bindingEscape.Matches(ev.Key):
		// The inline editor closing and the row reverting to its
		// committed name is the feedback; no notification.
		s.finishRename()
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
