package agentview

import (
	"sort"
	"time"

	"github.com/tingtt/agentsctl/internal/session"
	"github.com/tingtt/agentsctl/internal/sessionctl"
)

// State is Agent View's Root Owner: the single mutable UI state struct
// every input decision, render, and orchestrated operation reads and
// writes through (see the DesignDoc's "Treat Agent View state as a Root
// Owner"). The list cursor is either a session.Key or an Agent View-only
// group control identity, never a row index, so it survives refresh, pin,
// reordering, and provider reload intact. Unpinning the selected pinned
// row deliberately replaces that identity; see ApplyPatch.
type State struct {
	Rows []session.Session

	cursor    listItemID
	hasCursor bool

	// groupStates owns transient fold and directory-page state by stable
	// group identity. It is intentionally process-local and is not part of
	// localstate or the session domain.
	groupStates map[groupID]groupDisplayState

	// Provider is the composer's current dispatch target, cycled by
	// Shift+Tab.
	Provider session.ProviderID

	Composer Composer

	// Scope selects which sessions' CWDs are shown (same directory ->
	// descendants + worktree directories -> all -> same directory, cycled
	// by Ctrl+/). session.ScopeSame is the zero value, so a fresh State
	// starts scoped to the current directory without an explicit default
	// here.
	Scope session.DirectoryScope

	// StartupCWD is the directory agentsctl was started in -- the listing
	// scope anchor (see session.Scope.CurrentDirectory) -- and, per #14,
	// only ComposerCWD's fallback for when no session is selectable at
	// all. It never changes for the life of a Runtime; see
	// Runtime.reload, the single place that keeps it in sync with
	// Runtime.CWD.
	StartupCWD string

	// Error holds the most recent action failure. It is the only thing
	// ever rendered in the composer-top notification area, reserved for
	// errors exclusively -- an operation whose result is already visible
	// elsewhere in the UI (pin/unpin reordering a row, a rename changing
	// its title, an archive removing it) gets no notification at all,
	// error or otherwise. See RowNotice for the session-scoped, non-error
	// counterpart.
	Error    string
	Warnings map[session.ProviderID]error

	Rename Rename

	// Confirmation is the single pending two-press confirmation, if any
	// (see PendingConfirmation).
	Confirmation *PendingConfirmation

	// HelpVisible toggles #14's help view in place of the contextual
	// footer/usage lines. Only "?" on an empty composer prompt sets it
	// (see State.Handle); Esc's meaning is entirely governed by it while
	// true (hide help, never touching the prompt -- see the DesignDoc's
	// Esc priority order).
	HelpVisible bool

	// Usage holds the most recently loaded provider usage rows (see
	// sessionctl.Controller.Usage), rendered as #14's composer usage line.
	// A provider absent here either doesn't implement
	// sessionctl.UsageSource or failed to report usage on the last
	// reload -- either way it is simply omitted, never rendered as 0%.
	Usage []session.Usage

	// UsageUpdatedAt tracks, per provider, the wall-clock time of its most
	// recent successful entry in Usage (see ApplyUsageUpdate) -- how the
	// composer usage line tells a provider it just heard from apart from
	// one whose last known reading has gone stale, rendering the latter as
	// unknown ("?%") rather than a possibly-misleading old percentage (see
	// usageStaleAfter in footer.go). A provider absent here has never
	// reported successfully at all.
	UsageUpdatedAt map[session.ProviderID]time.Time

	// LastAttachedKey/HasLastAttached identify the session most recently
	// Opened from the overview, regardless of how that Open ended (an
	// explicit detach, or the session/process exiting on its own): title
	// styling only cares "which session was open right before this
	// return to the overview". Only a successful Open updates it (see
	// MarkAttached). Runtime-only UI state, tracked by session.Key so it
	// survives row reordering intact.
	LastAttachedKey session.Key
	HasLastAttached bool

	// CatalogLoading is true whenever the latest requested catalog reload
	// cycle (Runtime.requestReload) has not yet had its Snapshot applied
	// (see Runtime.eventLoop's catalogCh case) -- rendered as a small
	// "loading sessions…" header indicator (see View), never a blocking
	// modal. Every other control -- Rows, Composer, help, pin, quit --
	// stays interactive while it is true; see the DesignDoc's non-blocking
	// catalog refresh section.
	CatalogLoading bool
}

// NewState returns a freshly-initialized State: Claude as the initial
// composer provider target, matching the pre-refactor default.
func NewState() State {
	return State{
		Provider:       session.ProviderClaude,
		Warnings:       map[session.ProviderID]error{},
		UsageUpdatedAt: map[session.ProviderID]time.Time{},
		groupStates:    map[groupID]groupDisplayState{},
	}
}

// SetRows installs rows as the current catalog snapshot, preserving
// selection identity (see the DesignDoc's "selection identity は
// session.Key"): if the previously-selected session (or, while renaming,
// the rename target) is still present, selection stays on it regardless
// of its new position and its group opens far enough to keep it visible.
// A control cursor is preserved by group/control identity while meaningful.
// If the cursor target disappears, selection moves to the first surviving
// selectable item after it in the previous visual order, or walks backward
// if none survive after it. Visual positions are used only to choose a
// replacement identity.
//
// Selection also follows a provider-stated identity transition: when a row
// in rows lists the tracked key in its PreviousKeys (e.g. a Codex Starting
// row whose managed run has been bound to its thread) and the continuity
// passes session.IdentityTransitions' validation, the tracked key moves to
// that row's current Key, taking precedence over the nearby-session
// fallback above. The same continuity carries the rename target and
// LastAttachedKey (see followIdentity). It only ever consumes validated
// provider-stated transitions; it never derives an old/new relationship
// itself.
//
// Pending confirmations are NOT cleared here: the DesignDoc requires a
// row notice to follow its session across a Refresh, not just a local
// Pin/reorder (see PendingConfirmation) -- it naturally stops rendering
// once its target session is no longer present in rows. The exception is
// a confirmation armed on a key that just went through an identity
// transition: it is dropped (see followIdentity).
func (s *State) SetRows(rows []session.Session) {
	oldModel := s.selectableList()
	target, tracking := s.cursor, s.hasCursor
	if s.Rename.Active {
		target, tracking = sessionItemID(s.Rename.Target), true
	}

	s.Rows = rows
	moved := session.IdentityTransitions(rows)
	s.followIdentity(moved)
	if tracking && target.kind == listItemSession {
		if next, ok := moved[target.sessionKey]; ok {
			target = sessionItemID(next)
		}
		if s.ensureSessionVisible(target.sessionKey) {
			s.cursor, s.hasCursor = target, true
			return
		}
	}

	newModel := s.selectableList()
	if tracking {
		if _, ok := newModel.item(target); ok {
			s.cursor, s.hasCursor = target, true
			return
		}
		if next, ok := nearbySurvivingItem(oldModel, newModel, target); ok {
			s.cursor, s.hasCursor = next, true
			return
		}
	}
	if len(newModel.items) > 0 {
		s.cursor, s.hasCursor = newModel.items[0].id, true
		return
	}
	s.cursor, s.hasCursor = listItemID{}, false
}

func nearbySurvivingItem(oldModel, newModel selectableList, target listItemID) (listItemID, bool) {
	position := -1
	for i, item := range oldModel.items {
		if item.id == target {
			position = i
			break
		}
	}
	if position < 0 {
		return listItemID{}, false
	}
	for _, item := range oldModel.items[position+1:] {
		if _, ok := newModel.item(item.id); ok {
			return item.id, true
		}
	}
	for i := position - 1; i >= 0; i-- {
		if _, ok := newModel.item(oldModel.items[i].id); ok {
			return oldModel.items[i].id, true
		}
	}
	return listItemID{}, false
}

// followIdentity re-keys the transient UI state that holds a session.Key
// other than the selection itself (which SetRows resolves separately, as
// its precedence over the nearby-session fallback matters):
//
//   - Rename.Target and LastAttachedKey follow the session, since both
//     mean "this session" regardless of which key names it.
//   - A pending confirmation is dropped instead: it was armed against the
//     pre-transition row's action availability (a Starting Codex run offers
//     no Archive at all), so a two-press destructive gate must not carry
//     over to the differently-actionable canonical row.
func (s *State) followIdentity(moved map[session.Key]session.Key) {
	if len(moved) == 0 {
		return
	}
	if s.Rename.Active {
		if next, ok := moved[s.Rename.Target]; ok {
			s.Rename.Target = next
		}
	}
	if s.HasLastAttached {
		if next, ok := moved[s.LastAttachedKey]; ok {
			s.LastAttachedKey = next
		}
	}
	if s.Confirmation != nil {
		if _, ok := moved[s.Confirmation.Key]; ok {
			s.Confirmation = nil
		}
	}
}

// SelectedRow returns the currently-selected session, if any.
func (s State) SelectedRow() (session.Session, bool) {
	if !s.hasCursor || s.cursor.kind != listItemSession {
		return session.Session{}, false
	}
	for _, r := range s.Rows {
		if r.Key == s.cursor.sessionKey {
			return r, true
		}
	}
	return session.Session{}, false
}

// ComposerCWD is the directory context the composer displays and any new
// prompt dispatches into (see the DesignDoc's composer cwd section /
// #14): a selected session uses its own CWD, a directory control uses its
// stable normalized directory, and Pinned's directory-ambiguous control
// uses StartupCWD. StartupCWD also remains the empty-catalog fallback.
func (s State) ComposerCWD() string {
	if row, ok := s.SelectedRow(); ok {
		return row.CWD
	}
	if s.hasCursor && s.cursor.kind != listItemSession && !s.cursor.group.pinned {
		return s.cursor.group.directory
	}
	return s.StartupCWD
}

// SelectedIndex returns the row index of the current selection for
// rendering/navigation purposes, or -1 if there is none. A control cursor
// has no session-row index and also returns -1.
func (s State) SelectedIndex() int {
	if !s.hasCursor || s.cursor.kind != listItemSession {
		return -1
	}
	for i, r := range s.Rows {
		if r.Key == s.cursor.sessionKey {
			return i
		}
	}
	return -1
}

// selectIndex moves selection to Rows[i] if i is in range.
func (s *State) selectIndex(i int) {
	if i < 0 || i >= len(s.Rows) {
		return
	}
	s.cursor, s.hasCursor = sessionItemID(s.Rows[i].Key), true
	s.ensureSessionVisible(s.Rows[i].Key)
}

func (s State) selectableList() selectableList {
	return deriveSelectableList(s.Rows, s.groupStates)
}

func (s *State) setGroupState(id groupID, state groupDisplayState) {
	if s.groupStates == nil {
		s.groupStates = map[groupID]groupDisplayState{}
	}
	s.groupStates[id] = state
}

// ensureSessionVisible expands the selected session's group only as far as
// needed to keep its identity represented in the selectable list.
func (s *State) ensureSessionVisible(key session.Key) bool {
	for _, group := range groupRows(s.Rows) {
		for position, rowIndex := range group.indices {
			if s.Rows[rowIndex].Key != key {
				continue
			}
			state := s.groupStates[group.id]
			state.folded = false
			if group.id.pinned {
				state.visibleCount = 0
			} else {
				visible := state.visibleCount
				if visible <= 0 {
					visible = directoryPageSize
				}
				required := (position/directoryPageSize + 1) * directoryPageSize
				state.visibleCount = max(visible, required)
			}
			s.setGroupState(group.id, state)
			return true
		}
	}
	return false
}

// MarkAttached records key as the session most recently Opened from the
// overview. Callers must only invoke this after Open actually completed
// without error and control returned to the overview.
func (s *State) MarkAttached(key session.Key) {
	s.LastAttachedKey = key
	s.HasLastAttached = true
}

// ApplyPatch applies a local, already-confirmed sessionctl.Patch directly
// to the matching row -- see sessionctl.Result's doc comment for why Pin
// and Rename use this instead of a full reload. Pin changes re-sort because
// pin state affects ordering. Pinning preserves selection identity, while
// unpinning the selected pinned row selects its previous Pinned-group
// neighbor (or the following group's first row). Rename never affects
// ordering.
func (s *State) ApplyPatch(p sessionctl.Patch) {
	if p.Pinned != nil {
		var replacement listItemID
		var replaceSelection bool
		if !*p.Pinned {
			replacement, replaceSelection = s.selectionReplacementForUnpin(p.Key)
		}
		rows := append([]session.Session(nil), s.Rows...)
		for i := range rows {
			if rows[i].Key == p.Key {
				rows[i].Pinned = *p.Pinned
			}
		}
		session.SortOverview(rows)
		s.Rows = rows
		if replaceSelection {
			s.cursor, s.hasCursor = replacement, true
		} else if s.hasCursor && s.cursor.kind == listItemSession {
			s.ensureSessionVisible(s.cursor.sessionKey)
		}
		return
	}
	if p.Name != nil {
		for i := range s.Rows {
			if s.Rows[i].Key == p.Key {
				s.Rows[i].Name = *p.Name
			}
		}
	}
}

// selectionReplacementForUnpin returns the replacement identity for unpinning key
// when key is the selected pinned row. Candidates come from the current
// rendered grouping, before the patch can move key into an unpinned group:
// the next Pinned row, the previous Pinned row, then the first row of the
// following group.
func (s State) selectionReplacementForUnpin(key session.Key) (listItemID, bool) {
	if !s.hasCursor || s.cursor != sessionItemID(key) {
		return listItemID{}, false
	}

	model := s.selectableList()
	for groupIndex, group := range model.groups {
		for position, item := range group.items {
			if item.id.kind != listItemSession {
				continue
			}
			row := s.Rows[item.rowIndex]
			if row.Key != key || !row.Pinned {
				continue
			}
			if position+1 < len(group.items) {
				return group.items[position+1].id, true
			}
			if position > 0 {
				return group.items[position-1].id, true
			}
			for _, following := range model.groups[groupIndex+1:] {
				if len(following.items) > 0 {
					return following.items[0].id, true
				}
			}
			return listItemID{}, false
		}
	}
	return listItemID{}, false
}

// ApplyUsageUpdate incorporates one provider's incremental usage result
// (see sessionctl.Controller.UsageStream) into Usage: a successful reading
// upserts that provider's entry and records its arrival time in
// UsageUpdatedAt, an error removes the entry (leaving UsageUpdatedAt
// untouched, so a still-recent prior success doesn't immediately look
// unknown just because this one refresh failed) -- matching Controller.
// Usage's own "omit on failure, never a fake 0%" contract, just applied
// per provider instead of only at the end of one batch call. Usage is
// never reset wholesale here: a provider not yet updated in the current
// refresh cycle keeps showing its last known reading (see Runtime.reload's
// doc comment) rather than flickering to blank while a slower provider is
// still in flight -- until it goes stale on its own (usageStaleAfter).
func (s *State) ApplyUsageUpdate(provider session.ProviderID, usage session.Usage, err error) {
	next := make([]session.Usage, 0, len(s.Usage)+1)
	for _, u := range s.Usage {
		if u.Provider != provider {
			next = append(next, u)
		}
	}
	if err == nil {
		next = append(next, usage)
		if s.UsageUpdatedAt == nil {
			s.UsageUpdatedAt = map[session.ProviderID]time.Time{}
		}
		s.UsageUpdatedAt[provider] = time.Now()
	}
	sort.Slice(next, func(i, j int) bool { return next[i].Provider < next[j].Provider })
	s.Usage = next
}

// nextScope cycles the session-list directory scope: same directory ->
// descendants + worktree directories -> all -> same directory, bound to
// Ctrl+/.
func nextScope(scope session.DirectoryScope) session.DirectoryScope {
	switch scope {
	case session.ScopeSame:
		return session.ScopeDescendants
	case session.ScopeDescendants:
		return session.ScopeAll
	default:
		return session.ScopeSame
	}
}

// scopeLabel is the short header text for scope.
func scopeLabel(scope session.DirectoryScope) string {
	switch scope {
	case session.ScopeDescendants:
		return "descendants"
	case session.ScopeAll:
		return "all"
	default:
		return "same"
	}
}
