package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/tingtt/agentsctl/internal/session"
)

type ActionKind string

const (
	ActionNone     ActionKind = ""
	ActionDispatch ActionKind = "dispatch"
	ActionAttach   ActionKind = "attach"
	ActionStop     ActionKind = "stop"
	ActionArchive  ActionKind = "archive"
	ActionRename   ActionKind = "rename"
	ActionPin      ActionKind = "pin"
	ActionRefresh  ActionKind = "refresh"
	ActionQuit     ActionKind = "quit"
)

type Action struct {
	Kind       ActionKind
	Provider   session.ProviderID
	Prompt     string
	Name       string
	Session    *session.Session
	SessionKey *session.Key
}
type Model struct {
	Provider     session.ProviderID
	Prompt       string
	PromptCursor int
	Stash        string
	Rows         []session.Session
	Selected     int
	// Scope selects which sessions' CWDs are shown, cycled by Ctrl+G
	// (cwd -> cwd/** -> all -> cwd; see nextScope). Unlike the bool it
	// replaces, this can never collapse two different filters into one
	// state — session.ScopeCWD is the zero value, so a fresh Model starts
	// scoped to the current directory without an explicit default here.
	Scope    session.DirectoryScope
	CWDDepth int
	// Error holds the most recent action failure (a rejected rename, an
	// unavailable capability, an empty selection, ...). It is the only
	// thing ever rendered in the composer-top notification area, which is
	// reserved for errors exclusively — an operation whose result is
	// already visible elsewhere in the UI (pin/unpin reordering a row, a
	// rename changing its title, an archive removing it, the CWD depth
	// cycle changing every row's displayed CWD) gets no notification at
	// all, error or otherwise. See rowNotice for the session-scoped,
	// non-error counterpart. Cleared on entering rename/archive-confirm
	// mode, on cancelling out of either, and whenever an action dispatched
	// through App.act succeeds (see app_unix.go's Run loop) — so a stale
	// error from a previous failed action does not linger once the user
	// has moved on to something that worked.
	Error          string
	Warnings       map[session.ProviderID]error
	RenameDraft    string
	RenameCursor   int
	Renaming       bool
	RenameTarget   session.Key
	RenameOriginal string
	// LastAttachedKey/HasLastAttached identify the session most recently
	// attached to from the overview — regardless of how that attachment
	// ended (an explicit agentsctl Ctrl+] detach, or the attached client/
	// session exiting on its own): what the session list wants to convey
	// is simply "which session was open right before this return to the
	// overview", not anything about the PTY detach mechanism. Only a
	// successful attach (no error) updates it — see MarkAttached, the only
	// place that sets it, and app_unix.go's act(), the only caller.
	// Runtime-only UI state (never persisted to the state file); tracked
	// by session.Key so it survives row reordering (pin/unpin, refresh)
	// intact.
	LastAttachedKey session.Key
	HasLastAttached bool
	archiveConfirm  *session.Key
}

// MarkAttached records key as the session most recently attached to from
// the overview. Callers must only invoke this after attach actually
// completed without error and control returned to the overview — an
// attach error must leave LastAttachedKey unchanged — but it must be
// called regardless of *how* the attachment ended (explicit Ctrl+] detach
// or the attached client/session exiting on its own): both are "this
// session was the one last open", which is all the title style (see
// titleStyleCodes) conveys.
func (m *Model) MarkAttached(key session.Key) {
	m.LastAttachedKey = key
	m.HasLastAttached = true
}

// CWDDepthAll is the sentinel Model.CWDDepth value selecting the "all"
// directory-depth display mode (the shortHome-abbreviated full path,
// rather than a fixed number of trailing path components).
const CWDDepthAll = 0

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

// scopeLabel is the short header text for scope, distinguishing all three
// states without adding a dedicated notification (see Model.Scope).
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

type displayLine struct {
	text     string
	rowIndex int
}

func NewModel() Model {
	return Model{Provider: session.ProviderClaude, Warnings: map[session.ProviderID]error{}, CWDDepth: 2}
}
func (m *Model) SetRows(rows []session.Session) {
	m.archiveConfirm = nil
	var selected session.Key
	selectedValid := false
	if m.Renaming {
		selected = m.RenameTarget
		selectedValid = true
	} else if m.Selected >= 0 && m.Selected < len(m.Rows) {
		selected = m.Rows[m.Selected].Key
		selectedValid = true
	}
	m.Rows = rows
	if selectedValid {
		for i := range rows {
			if rows[i].Key == selected {
				m.Selected = i
				return
			}
		}
	}
	if m.Selected >= len(rows) {
		m.Selected = max(0, len(rows)-1)
	}
}

// ApplyPin updates the pinned state of the row identified by key within
// the Model's current rows, re-sorts them with the same ordering Catalog
// uses (session.SortOverview), and moves the selection to key's new
// position. Pin/unpin is a local-only operation — it must never require a
// provider List refresh to reflect the new Pinned/Other grouping, so
// callers apply it directly to whatever rows the Model already holds
// rather than reloading the catalog.
func (m *Model) ApplyPin(key session.Key, pinned bool) {
	rows := append([]session.Session(nil), m.Rows...)
	for i := range rows {
		if rows[i].Key == key {
			rows[i].Pinned = pinned
		}
	}
	session.SortOverview(rows)
	m.Rows = rows
	for i := range rows {
		if rows[i].Key == key {
			m.Selected = i
			return
		}
	}
}
func (m *Model) Update(key string) Action {
	if m.Renaming {
		return m.updateRename(key)
	}
	if m.archiveConfirm != nil {
		return m.updateArchiveConfirmation(key)
	}
	switch key {
	case "shift+tab":
		if m.Provider == session.ProviderClaude {
			m.Provider = session.ProviderCodex
		} else {
			m.Provider = session.ProviderClaude
		}
		return Action{}
	case "up":
		if m.Selected > 0 {
			m.Selected--
			m.cancelArchiveConfirmation()
		}
		return Action{}
	case "down":
		if m.Selected+1 < len(m.Rows) {
			m.Selected++
			m.cancelArchiveConfirmation()
		}
		return Action{}
	case "backspace":
		r := []rune(m.Prompt)
		m.clampPromptCursor(r)
		if m.PromptCursor > 0 {
			r = append(r[:m.PromptCursor-1], r[m.PromptCursor:]...)
			m.PromptCursor--
			m.Prompt = string(r)
		}
		return Action{}
	case "delete":
		r := []rune(m.Prompt)
		m.clampPromptCursor(r)
		if m.PromptCursor < len(r) {
			r = append(r[:m.PromptCursor], r[m.PromptCursor+1:]...)
			m.Prompt = string(r)
		}
		return Action{}
	case "home":
		m.PromptCursor = 0
		return Action{}
	case "end":
		m.PromptCursor = len([]rune(m.Prompt))
		return Action{}
	case "left":
		m.PromptCursor = max(0, min(m.PromptCursor, len([]rune(m.Prompt)))-1)
		return Action{}
	case "right":
		m.PromptCursor = min(len([]rune(m.Prompt)), m.PromptCursor+1)
		return Action{}
	case "stash":
		if m.Prompt != "" || m.Stash != "" {
			m.Prompt, m.Stash = m.Stash, m.Prompt
			m.PromptCursor = len([]rune(m.Prompt))
		}
		return Action{}
	case "enter":
		if strings.TrimSpace(m.Prompt) != "" {
			return Action{Kind: ActionDispatch, Provider: m.Provider, Prompt: m.Prompt}
		}
		return m.selectedOrStatus(ActionAttach, "open")
	case "open":
		return m.selectedOrStatus(ActionAttach, "open")
	case "scope-cycle":
		m.Scope = nextScope(m.Scope)
		m.Selected = 0
		m.archiveConfirm = nil
		return Action{Kind: ActionRefresh}
	case "stop-or-archive":
		return m.stopOrArchive()
	case "rename":
		row, ok := m.selectedRow()
		if !ok {
			m.Error = "No session selected"
			return Action{}
		}
		if !row.Capabilities.Rename {
			m.Error = capabilityReason(row, "rename")
			return Action{}
		}
		m.Renaming = true
		m.RenameTarget = row.Key
		m.RenameOriginal = row.Name
		m.RenameDraft = row.Name
		m.RenameCursor = len([]rune(row.Name))
		m.Error = ""
		return Action{}
	case "pin":
		return m.selected(ActionPin)
	case "depth-cycle":
		// The CWD column itself changing for every row is the feedback;
		// no notification (see Model.Error's doc comment).
		m.CWDDepth = nextCWDDepth(m.CWDDepth)
		return Action{}
	case "refresh":
		return Action{Kind: ActionRefresh}
	case "quit":
		return Action{Kind: ActionQuit}
	default:
		if isTextInput(key) {
			runes := []rune(m.Prompt)
			m.clampPromptCursor(runes)
			insert := []rune(key)
			before := append([]rune(nil), runes[:m.PromptCursor]...)
			after := append([]rune(nil), runes[m.PromptCursor:]...)
			m.Prompt = string(append(append(before, insert...), after...))
			m.PromptCursor += len(insert)
		}
		return Action{}
	}
}

func (m *Model) clampPromptCursor(runes []rune) {
	m.PromptCursor = min(max(m.PromptCursor, 0), len(runes))
}

func (m *Model) cancelArchiveConfirmation() {
	m.archiveConfirm = nil
}

func (m *Model) updateRename(key string) Action {
	runes := []rune(m.RenameDraft)
	switch key {
	case "quit":
		// The inline editor closing and the row reverting to its committed
		// name is the feedback; no notification (see Model.Error's doc
		// comment).
		m.clearRename()
		m.Error = ""
	case "home":
		m.RenameCursor = 0
	case "end":
		m.RenameCursor = len(runes)
	case "left":
		m.RenameCursor = max(0, m.RenameCursor-1)
	case "right":
		m.RenameCursor = min(len(runes), m.RenameCursor+1)
	case "backspace":
		if m.RenameCursor > 0 {
			runes = append(runes[:m.RenameCursor-1], runes[m.RenameCursor:]...)
			m.RenameCursor--
			m.RenameDraft = string(runes)
		}
	case "delete":
		if m.RenameCursor < len(runes) {
			runes = append(runes[:m.RenameCursor], runes[m.RenameCursor+1:]...)
			m.RenameDraft = string(runes)
		}
	case "enter":
		if strings.TrimSpace(m.RenameDraft) == "" {
			m.Error = "Name must not be empty"
			return Action{}
		}
		key := m.RenameTarget
		return Action{Kind: ActionRename, Provider: key.Provider, SessionKey: &key, Name: m.RenameDraft}
	default:
		if isTextInput(key) {
			insert := []rune(key)
			before := append([]rune(nil), runes[:m.RenameCursor]...)
			after := append([]rune(nil), runes[m.RenameCursor:]...)
			m.RenameDraft = string(append(append(before, insert...), after...))
			m.RenameCursor += len(insert)
		}
	}
	return Action{}
}

func (m *Model) clearRename() {
	m.Renaming = false
	m.RenameTarget = session.Key{}
	m.RenameOriginal = ""
	m.RenameDraft = ""
	m.RenameCursor = 0
}

func (m *Model) updateArchiveConfirmation(key string) Action {
	switch key {
	case "stop-or-archive":
		return m.stopOrArchive()
	case "quit":
		// The red row confirmation disappearing is the feedback; no
		// notification (see Model.Error's doc comment).
		m.cancelArchiveConfirmation()
		m.Error = ""
	case "up", "down":
		// Moving the selection cancels a pending archive confirmation (no
		// "cancelled" notice here, unlike Esc — the confirmation simply
		// disappearing from its row as selection moves off it is enough).
		// Update's normal dispatch only reaches the main switch's own
		// up/down cases when archiveConfirm is already nil, so the
		// cancellation and the move must both happen here.
		m.cancelArchiveConfirmation()
		return m.Update(key)
	}
	return Action{}
}

func (m *Model) stopOrArchive() Action {
	row, ok := m.selectedRow()
	if !ok {
		m.Error = "No session selected"
		return Action{}
	}
	if row.Capabilities.Stop {
		m.archiveConfirm = nil
		return m.selected(ActionStop)
	}
	if !row.Capabilities.Archive {
		m.Error = capabilityReason(row, "stop or archive")
		return Action{}
	}
	if m.archiveConfirm == nil || *m.archiveConfirm != row.Key {
		key := row.Key
		m.archiveConfirm = &key
		m.Error = ""
		return Action{}
	}
	m.archiveConfirm = nil
	m.Error = ""
	return m.selected(ActionArchive)
}

func (m *Model) selectedRow() (session.Session, bool) {
	if m.Selected < 0 || m.Selected >= len(m.Rows) {
		return session.Session{}, false
	}
	return m.Rows[m.Selected], true
}

func (m *Model) selectedOrStatus(kind ActionKind, name string) Action {
	row, ok := m.selectedRow()
	if !ok {
		m.Error = "No session selected"
		return Action{}
	}
	allowed := kind == ActionAttach && row.Capabilities.Attach
	if !allowed {
		m.Error = capabilityReason(row, name)
		return Action{}
	}
	return m.selected(kind)
}

// noticeSeverity is a row-scoped notice's color/urgency. Only alert is
// currently produced (archive confirmation), but View's rendering path
// handles both so a future non-error info notice needs no new plumbing —
// just another case in rowNotice below.
type noticeSeverity int

const (
	noticeInfo noticeSeverity = iota
	noticeAlert
)

// noticeColor maps a noticeSeverity to its ANSI SGR color code: info is
// cyan, alert is red.
func noticeColor(severity noticeSeverity) string {
	if severity == noticeAlert {
		return colorRed
	}
	return colorCyan
}

// rowNotice returns the row-scoped, non-error notice for the session
// identified by key, if any. Text is derived here from current state on
// every call — never cached as a display string — so it can never go stale
// across selection moves, pin/reorder, or a refresh: those all act on the
// same session.Key-keyed state (currently just archiveConfirm) this reads.
func (m Model) rowNotice(key session.Key) (string, noticeSeverity, bool) {
	if m.archiveConfirm != nil && *m.archiveConfirm == key {
		return "Press Ctrl+X again to archive", noticeAlert, true
	}
	return "", noticeInfo, false
}

func capabilityReason(row session.Session, action string) string {
	if row.Capabilities.Reason != "" {
		return "Cannot " + action + ": " + row.Capabilities.Reason
	}
	return "Cannot " + action + ": action is unavailable in the current state"
}
func (m *Model) selected(kind ActionKind) Action {
	if len(m.Rows) == 0 {
		return Action{}
	}
	row := m.Rows[m.Selected]
	return Action{Kind: kind, Provider: row.Key.Provider, Session: &row, Prompt: m.Prompt}
}
func (m Model) View(width, height int) string {
	header := []string{clipLine(fmt.Sprintf("agentsctl · %s", scopeLabel(m.Scope)), width), ""}
	list := make([]displayLine, 0, len(m.Rows)+4)
	groups := []struct {
		title  string
		pinned bool
	}{{"Pinned", true}, {"Other", false}}
	for _, g := range groups {
		shown := false
		for i, row := range m.Rows {
			if row.Pinned != g.pinned {
				continue
			}
			if !shown {
				list = append(list, displayLine{text: clipLine(g.title, width), rowIndex: -1})
				shown = true
			}
			cursor := " "
			if i == m.Selected {
				cursor = ">"
			}
			if m.archiveConfirm != nil && row.Key == *m.archiveConfirm {
				cursor = "x"
			}
			// Layout: "> [status] title...spacer...[notice] [provider] cwd/".
			// The right block (an optional notice, then the provider field
			// and cwd, right-aligned to the terminal edge) is sized from its
			// own content first; the title gets whatever cells are left,
			// padded so the row fills the terminal width exactly. Provider
			// identity lives in the right block's label, not the title's
			// color — the title's color/weight instead conveys selection and
			// last-attached state (see titleStyleCodes). A notice never
			// shrinks the provider/cwd block: splitRowWidth reserves cwd's
			// width first, then gives a notice whatever is left before the
			// title, clipping or dropping the notice — never cwd/provider —
			// under width pressure.
			cwdPlain := withTrailingSlash(displayCWD(row.CWD, m.CWDDepth))
			noticeText, severity, hasNotice := m.rowNotice(row.Key)
			noticeCells := 0
			if hasNotice {
				noticeCells = lineCells(noticeText)
			}
			titleWidth, noticeWidth, cwdWidth := splitRowWidth(width, lineCells(cwdPlain), noticeCells)
			var name string
			if m.Renaming && row.Key == m.RenameTarget {
				name = cursorWindow(m.RenameDraft, m.RenameCursor, titleWidth)
			} else {
				name = fitCells(row.DisplayName(), titleWidth)
			}
			selected := i == m.Selected
			lastAttached := m.HasLastAttached && row.Key == m.LastAttachedKey
			name = styleText(name, titleStyleCodes(selected, lastAttached)...)
			noticeSegment := ""
			if noticeWidth > 0 {
				noticeSegment = styleText(clipLine(noticeText, noticeWidth), noticeColor(severity)) + " "
			}
			cwd := fitCells(truncateLeftCells(cwdPlain, cwdWidth), cwdWidth)
			provider := styleText(providerLabel(row.Key.Provider), providerColor(row.Key.Provider))
			line := cursor + " " + statusIcon(row.Activity) + " " + name + noticeSegment + provider + " " + cwd
			list = append(list, displayLine{text: clipLine(line, width), rowIndex: i})
		}
		if shown {
			list = append(list, displayLine{text: "", rowIndex: -1})
		}
	}
	unavailable := ""
	if err := m.Warnings[m.Provider]; err != nil {
		unavailable = " (unavailable: " + err.Error() + ")"
	}
	promptPrefix := composerPrefix(m.Provider, unavailable)
	footer := []string{
		clipLine(promptPrefix+cursorWindow(m.Prompt, m.PromptCursor, max(1, width-lineCells(promptPrefix))), width),
		clipLine("Shift+Tab / Enter send/open / Ctrl+S stash / Ctrl+O / Ctrl+T pin / Ctrl+/ depth", width),
		clipLine("↑↓ / Ctrl+G scope / Ctrl+R rename / Ctrl+X stop/archive / Ctrl+L refresh / Esc quit", width),
	}
	// The composer-top notification area is reserved for Error exclusively
	// (see Model.Error's doc comment) — there is no generic non-error
	// notice here.
	if m.Error != "" {
		footer = append([]string{clipLine("! "+m.Error, width)}, footer...)
	}
	if height < 0 {
		height = 0
	}
	reserved := len(header) + len(footer)
	if reserved > height {
		drop := min(len(header), reserved-height)
		header = header[drop:]
	}
	if len(header)+len(footer) > height {
		footer = footer[len(footer)-height:]
		header = nil
	}
	listHeight := max(0, height-len(header)-len(footer))
	start := viewportStart(list, m.Selected, listHeight)
	end := min(len(list), start+listHeight)
	var b strings.Builder
	for _, line := range header {
		b.WriteString(line)
		b.WriteByte('\n')
	}
	for _, line := range list[start:end] {
		b.WriteString(line.text)
		b.WriteByte('\n')
	}
	for rendered := end - start; rendered < listHeight; rendered++ {
		b.WriteByte('\n')
	}
	for _, line := range footer {
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// cursorStyle renders a single glyph in reverse video (white background,
// black text), representing the insertion point immediately before it.
// Shared by both the rename editor and the prompt composer so their
// cursor rendering stays identical.
func cursorStyle(glyph string) string {
	return "\x1b[30;47m" + glyph + "\x1b[0m"
}

// cursorWindow renders value with a horizontally-scrolled window around a
// rune-index cursor, fit into width terminal cells. The rune at the
// cursor position is recolored via cursorStyle to mark the insertion
// point; if cursor is at the end of value, a trailing reverse-video space
// cell marks it instead. This is the single cursor-rendering/-windowing
// implementation shared by the rename editor and the prompt composer.
func cursorWindow(value string, cursor, width int) string {
	if width <= 0 {
		return ""
	}
	runes := []rune(value)
	cursor = min(max(cursor, 0), len(runes))
	glyph := " "
	var suffix []rune
	if cursor < len(runes) {
		glyph = string(runes[cursor])
		suffix = runes[cursor+1:]
	}
	budget := max(0, width-lineCells(glyph))
	start := max(0, cursor-budget)
	for start < cursor && lineCells(string(runes[start:cursor])) > budget {
		start++
	}
	prefix := string(runes[start:cursor])
	result := clipLine(prefix+cursorStyle(glyph)+string(suffix), width)
	return fitCells(result, width)
}

// rowLeftFixed is the terminal-cell width of a session row's left prefix
// before the title starts: cursor(1) + separator(1) + status(1) +
// separator(1).
const rowLeftFixed = 4

// rowRightFixed is the terminal-cell width of the right block's fixed
// portion, before the CWD: the provider field plus the single separator
// space between it and the CWD. The CWD itself is variable-width.
const rowRightFixed = providerFieldWidth + 1

// splitRowWidth lays out a session row in strict priority order — provider
// field + CWD first, then an optional row notice, then the title gets
// whatever cells remain — so the row fills the terminal width exactly. The
// CWD (with its trailing "/" already counted in cwdCells) is shown in full
// whenever the terminal is wide enough; only when the terminal is too
// narrow for the full CWD alongside the fixed left/provider cells does the
// CWD itself give up width (to be left-truncated by the caller), at which
// point both notice and title get none. A notice (noticeCells; pass 0 for
// none) never takes width from CWD/provider: it is sized from whatever
// remains after they're placed, and is clipped or dropped entirely before
// the title ever loses width to it.
func splitRowWidth(width, cwdCells, noticeCells int) (title, notice, cwd int) {
	available := width - rowLeftFixed - rowRightFixed
	if available < 0 {
		available = 0
	}
	if cwdCells > available {
		return 0, 0, available
	}
	cwd = cwdCells
	remaining := available - cwd
	if noticeCells == 0 {
		return remaining, 0, cwd
	}
	// +1 for the separator space between the notice and the provider field.
	if noticeFixed := noticeCells + 1; noticeFixed <= remaining {
		return remaining - noticeFixed, noticeCells, cwd
	}
	// Not enough room for the full notice alongside its separator; give it
	// whatever is left (clipped) with no title, or drop it entirely if
	// there isn't even room for one cell of it plus the separator.
	if remaining >= 2 {
		return 0, remaining - 1, cwd
	}
	return 0, 0, cwd
}

// fitCells clips or space-pads value to exactly width terminal cells,
// tolerating embedded ANSI SGR sequences (their bytes are zero-width and
// pass through untouched).
func fitCells(value string, width int) string {
	clipped := clipLine(value, width)
	return padCells(clipped, width)
}

func padCells(value string, width int) string {
	if pad := width - lineCells(value); pad > 0 {
		return value + strings.Repeat(" ", pad)
	}
	return value
}

// styleText wraps text in a single ANSI SGR escape built from codes (e.g.
// styleText(s, "1", "97") for bold+white), the shared style-composition
// primitive for every foreground/weight span in a row: status glyphs,
// provider labels, and session titles. text may already contain an
// embedded cursorStyle segment (the rename editor's cursor, or the
// prompt composer's); that segment closes with its own "\x1b[0m" reset,
// which would otherwise wipe codes' style for anything after it, so the
// style is re-opened immediately after every embedded reset before the
// whole thing is closed with one final reset — codes never leaks past the
// text it wraps into the row's spacer, the right block, or the next row.
// Empty codes are dropped, so a "no style" caller (e.g. an unknown
// provider) gets text back unchanged. skipANSI/lineCells treat all of
// this as zero-width, so it never affects column alignment.
func styleText(text string, codes ...string) string {
	var active []string
	for _, c := range codes {
		if c != "" {
			active = append(active, c)
		}
	}
	if len(active) == 0 {
		return text
	}
	prefix := "\x1b[" + strings.Join(active, ";") + "m"
	reopened := strings.ReplaceAll(text, "\x1b[0m", "\x1b[0m"+prefix)
	return prefix + reopened + "\x1b[0m"
}

// composerPrefix builds the prompt composer's "<provider> > " prefix. The
// provider label is fixed to providerFieldWidth visible cells (via
// providerLabel) and colored via providerColor — the same mapping the
// session list's provider label uses — so Shift+Tab switching providers
// never moves the column the prompt body starts at. unavailable (when
// non-empty) is appended after the fixed-width label and before " > ";
// it is not itself constrained to any width, since only the
// available-provider case needs the start-column guarantee.
func composerPrefix(provider session.ProviderID, unavailable string) string {
	return styleText(providerLabel(provider), providerColor(provider)) + unavailable + " > "
}

// displayCWD renders row's working directory per the active directory-
// depth mode: depth 1-3 show that many trailing path components (HOME is
// never counted as a component); CWDDepthAll shows the shortHome-
// abbreviated full path, matching the pre-existing behavior.
func displayCWD(path string, depth int) string {
	if depth == CWDDepthAll {
		return shortHome(path)
	}
	home, _ := os.UserHomeDir()
	return trailingComponents(path, home, depth)
}

// withTrailingSlash appends a directory separator "/" to a displayed CWD
// (matching common shell prompt conventions), unless value already ends
// in one — guarding both root ("/", which must stay "/" and never become
// "//") and any other already-slash-terminated input against a doubled
// slash. Applied after displayCWD, so the slash is part of the CWD's
// visible width for clipping/right-alignment purposes, and — since
// truncateLeftCells only ever drops characters from the left — it always
// survives left-truncation in a narrow terminal.
func withTrailingSlash(value string) string {
	if strings.HasSuffix(value, "/") {
		return value
	}
	return value + "/"
}

// trailingComponents returns the last n path components of path (HOME
// stripped and not counted as a component). If path has n or fewer
// components, all of them are returned unchanged — no "/" or "…" is
// forced in. A path equal to HOME itself has zero components and renders
// as "~". Split out from displayCWD, mirroring shortHome/shortenHome,
// so tests can exercise it with a fixed home instead of the process's
// real one.
func trailingComponents(path, home string, n int) string {
	rel := filepath.Clean(path)
	if home != "" {
		home = filepath.Clean(home)
		if rel == home {
			return "~"
		}
		if strings.HasPrefix(rel, home+string(filepath.Separator)) {
			rel = rel[len(home)+1:]
		}
	}
	var parts []string
	for _, p := range strings.Split(rel, string(filepath.Separator)) {
		if p != "" {
			parts = append(parts, p)
		}
	}
	if len(parts) > n {
		parts = parts[len(parts)-n:]
	}
	return strings.Join(parts, "/")
}

// shortHome renders path with the user's home directory abbreviated to
// "~", matching common shell prompt conventions.
func shortHome(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	return shortenHome(path, home)
}

func shortenHome(path, home string) string {
	home = filepath.Clean(home)
	clean := filepath.Clean(path)
	if clean == home {
		return "~"
	}
	if strings.HasPrefix(clean, home+string(filepath.Separator)) {
		return "~" + clean[len(home):]
	}
	return clean
}

// truncateLeftCells fits value into width terminal cells by dropping
// characters from the left (prefixing an ellipsis) so the tail — the
// part a user most needs, e.g. the current directory name — stays
// visible.
func truncateLeftCells(value string, width int) string {
	if width <= 0 {
		return ""
	}
	if lineCells(value) <= width {
		return value
	}
	const ellipsis = "…"
	ellipsisCells := lineCells(ellipsis)
	if width <= ellipsisCells {
		return tailCells(value, width)
	}
	return ellipsis + tailCells(value, width-ellipsisCells)
}

// tailCells returns the longest suffix of value that fits in width cells.
func tailCells(value string, width int) string {
	runes := []rune(value)
	total := 0
	start := len(runes)
	for start > 0 {
		c := runeCells(runes[start-1])
		if total+c > width {
			break
		}
		total += c
		start--
	}
	return string(runes[start:])
}

// lineCells returns the visible terminal-cell width of value, skipping
// ANSI CSI escape sequences (which occupy zero visible cells) so styled
// text can be measured and padded/clipped like plain text.
func lineCells(value string) int {
	width := 0
	for i := 0; i < len(value); {
		if value[i] == 0x1b {
			i = skipANSI(value, i)
			continue
		}
		r, size := utf8.DecodeRuneInString(value[i:])
		width += runeCells(r)
		i += size
	}
	return width
}

// skipANSI returns the index just past an ANSI CSI escape sequence
// starting at i (value[i] must be ESC), or i+1 if it isn't a recognized
// CSI sequence, so scanning always makes forward progress.
func skipANSI(value string, i int) int {
	if i+1 >= len(value) || value[i+1] != '[' {
		return i + 1
	}
	j := i + 2
	for j < len(value) && !(value[j] >= 0x40 && value[j] <= 0x7e) {
		j++
	}
	if j < len(value) {
		return j + 1
	}
	return j
}

func isTextInput(key string) bool {
	runes := []rune(key)
	return len(runes) == 1 && runes[0] >= 0x20 && runes[0] != 0x7f
}

func viewportStart(lines []displayLine, selected, height int) int {
	if height <= 0 || len(lines) <= height {
		return 0
	}
	selectedLine := 0
	for i, line := range lines {
		if line.rowIndex == selected {
			selectedLine = i
			break
		}
	}
	start := selectedLine - height/2
	if start < 0 {
		start = 0
	}
	if start+height > len(lines) {
		start = len(lines) - height
	}
	return start
}

func clipLine(value string, width int) string {
	if width <= 0 {
		return ""
	}
	used := 0
	var b strings.Builder
	for i := 0; i < len(value); {
		if value[i] == 0x1b {
			j := skipANSI(value, i)
			b.WriteString(value[i:j])
			i = j
			continue
		}
		r, size := utf8.DecodeRuneInString(value[i:])
		cells := runeCells(r)
		if used+cells > width {
			break
		}
		b.WriteRune(r)
		used += cells
		i += size
	}
	return b.String()
}

func runeCells(r rune) int {
	if r == 0 || r < 32 || r >= 0x7f && r < 0xa0 {
		return 0
	}
	if r >= 0x1100 && (r <= 0x115f || r == 0x2329 || r == 0x232a || r >= 0x2e80 && r <= 0xa4cf || r >= 0xac00 && r <= 0xd7a3 || r >= 0xf900 && r <= 0xfaff || r >= 0xfe10 && r <= 0xfe19 || r >= 0xfe30 && r <= 0xfe6f || r >= 0xff00 && r <= 0xff60 || r >= 0xffe0 && r <= 0xffe6 || r >= 0x1f300 && r <= 0x1faff || r >= 0x20000 && r <= 0x3fffd) {
		return 2
	}
	return 1
}
