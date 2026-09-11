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
// Owner"). Selection is keyed by session.Key (see selectedKey/hasSelection
// and SelectedRow), never by row index, so it survives refresh, pin/
// unpin, reordering, and provider reload intact.
type State struct {
	Rows []session.Session

	selectedKey  session.Key
	hasSelection bool

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
}

// NewState returns a freshly-initialized State: Claude as the initial
// composer provider target, matching the pre-refactor default.
func NewState() State {
	return State{Provider: session.ProviderClaude, Warnings: map[session.ProviderID]error{}, UsageUpdatedAt: map[session.ProviderID]time.Time{}}
}

// SetRows installs rows as the current catalog snapshot, preserving
// selection identity (see the DesignDoc's "selection identity は
// session.Key"): if the previously-selected session (or, while renaming,
// the rename target) is still present, selection stays on it regardless
// of its new position. If it disappeared, selection moves to the first
// surviving session after it in the previous visual order, or walks
// backward from its previous position if none survive after it. The
// selected session remains identified by key; visual positions are used
// only to choose a replacement identity.
//
// Pending confirmations are NOT cleared here: the DesignDoc requires a
// row notice to follow its session across a Refresh, not just a local
// Pin/reorder (see PendingConfirmation) -- it naturally stops rendering
// once its target session is no longer present in rows.
func (s *State) SetRows(rows []session.Session) {
	target, tracking := s.selectedKey, s.hasSelection
	if s.Rename.Active {
		target, tracking = s.Rename.Target, true
	}
	oldRows := s.Rows
	oldVisualIndices := visualRowIndices(oldRows)
	targetVisualIndex := -1
	for i, rowIndex := range oldVisualIndices {
		if oldRows[rowIndex].Key == target {
			targetVisualIndex = i
			break
		}
	}

	s.Rows = rows
	if tracking {
		for _, r := range rows {
			if r.Key == target {
				s.selectedKey, s.hasSelection = target, true
				return
			}
		}
	}
	if tracking && targetVisualIndex >= 0 {
		remaining := make(map[session.Key]struct{}, len(rows))
		for _, row := range rows {
			remaining[row.Key] = struct{}{}
		}
		for _, rowIndex := range oldVisualIndices[targetVisualIndex+1:] {
			candidate := oldRows[rowIndex].Key
			if _, ok := remaining[candidate]; ok {
				s.selectedKey, s.hasSelection = candidate, true
				return
			}
		}
		for i := targetVisualIndex - 1; i >= 0; i-- {
			candidate := oldRows[oldVisualIndices[i]].Key
			if _, ok := remaining[candidate]; ok {
				s.selectedKey, s.hasSelection = candidate, true
				return
			}
		}
	}
	if indices := visualRowIndices(rows); len(indices) > 0 {
		s.selectedKey, s.hasSelection = rows[indices[0]].Key, true
		return
	}
	s.selectedKey, s.hasSelection = session.Key{}, false
}

// SelectedRow returns the currently-selected session, if any.
func (s State) SelectedRow() (session.Session, bool) {
	if !s.hasSelection {
		return session.Session{}, false
	}
	for _, r := range s.Rows {
		if r.Key == s.selectedKey {
			return r, true
		}
	}
	return session.Session{}, false
}

// ComposerCWD is the directory context the composer displays and any new
// prompt dispatches into (see the DesignDoc's composer cwd section /
// #14): the selected session's own CWD, so both display and dispatch
// follow selection as it moves across directories. StartupCWD is only a
// safe fallback for when no session is selectable at all (an empty
// catalog) -- it never overrides an actual selection, even one outside
// the current listing scope's own anchor directory.
func (s State) ComposerCWD() string {
	if row, ok := s.SelectedRow(); ok {
		return row.CWD
	}
	return s.StartupCWD
}

// SelectedIndex returns the row index of the current selection for
// rendering/navigation purposes, or -1 if there is none. Index is always
// a value derived from selectedKey, never the other way around.
func (s State) SelectedIndex() int {
	if !s.hasSelection {
		return -1
	}
	for i, r := range s.Rows {
		if r.Key == s.selectedKey {
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
	s.selectedKey, s.hasSelection = s.Rows[i].Key, true
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
// and Rename use this instead of a full reload. Pin also re-sorts (pin
// state affects ordering) and follows the row to its new position; Rename
// never affects ordering.
func (s *State) ApplyPatch(p sessionctl.Patch) {
	if p.Pinned != nil {
		// Re-sorting never disturbs selection: SelectedIndex is derived
		// from selectedKey on every call (see SelectedIndex), so the
		// selected session simply reports a new index after this.
		rows := append([]session.Session(nil), s.Rows...)
		for i := range rows {
			if rows[i].Key == p.Key {
				rows[i].Pinned = *p.Pinned
			}
		}
		session.SortOverview(rows)
		s.Rows = rows
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
