package agentview

import (
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

	// Scope selects which sessions' CWDs are shown (cwd -> cwd/** -> all
	// -> cwd, cycled by Ctrl+G). session.ScopeCWD is the zero value, so a
	// fresh State starts scoped to the current directory without an
	// explicit default here.
	Scope session.DirectoryScope
	// CWDDepth is the directory-path display depth (1-3 trailing
	// components, or CWDDepthAll), cycled by Ctrl+/.
	CWDDepth int

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

// CWDDepthAll is the sentinel State.CWDDepth value selecting the "all"
// directory-depth display mode.
const CWDDepthAll = 0

// NewState returns a freshly-initialized State: Claude as the initial
// composer provider target and a 2-component CWD display depth, matching
// the pre-refactor default.
func NewState() State {
	return State{Provider: session.ProviderClaude, Warnings: map[session.ProviderID]error{}, CWDDepth: 2}
}

// SetRows installs rows as the current catalog snapshot, preserving
// selection identity (see the DesignDoc's "selection identity は
// session.Key"): if the previously-selected session (or, while renaming,
// the rename target) is still present, selection stays on it regardless
// of its new position; otherwise selection falls back to the first row
// (rows are already sorted pinned-then-newest-first, so this is the most
// prominent remaining row) rather than an arbitrary index that used to
// point at the now-gone row.
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
	s.Rows = rows
	if tracking {
		for _, r := range rows {
			if r.Key == target {
				s.selectedKey, s.hasSelection = target, true
				return
			}
		}
	}
	if len(rows) > 0 {
		s.selectedKey, s.hasSelection = rows[0].Key, true
	} else {
		s.hasSelection = false
	}
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

// nextCWDDepth cycles the directory-depth display mode: 1 -> 2 -> 3 ->
// all -> 1.
func nextCWDDepth(depth int) int {
	switch depth {
	case 1:
		return 2
	case 2:
		return 3
	case 3:
		return CWDDepthAll
	default:
		return 1
	}
}

// nextScope cycles the session-list directory scope: cwd -> cwd/** -> all
// -> cwd, bound to Ctrl+G.
func nextScope(scope session.DirectoryScope) session.DirectoryScope {
	switch scope {
	case session.ScopeCWD:
		return session.ScopeSubtree
	case session.ScopeSubtree:
		return session.ScopeAll
	default:
		return session.ScopeCWD
	}
}

// scopeLabel is the short header text for scope.
func scopeLabel(scope session.DirectoryScope) string {
	switch scope {
	case session.ScopeSubtree:
		return "cwd/**"
	case session.ScopeAll:
		return "all"
	default:
		return "cwd"
	}
}
