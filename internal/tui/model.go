package tui

import (
	"fmt"
	"strings"

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
	ActionRefresh  ActionKind = "refresh"
	ActionQuit     ActionKind = "quit"
)

type Action struct {
	Kind     ActionKind
	Provider session.ProviderID
	Prompt   string
	Session  *session.Session
}
type Model struct {
	Provider       session.ProviderID
	Prompt         string
	Rows           []session.Session
	Selected       int
	AllDirectories bool
	Status         string
	Warnings       map[session.ProviderID]error
	RenameDraft    string
	RenameCursor   int
	Renaming       bool
	archiveConfirm string
}

type displayLine struct {
	text     string
	rowIndex int
}

func NewModel() Model {
	return Model{Provider: session.ProviderClaude, Warnings: map[session.ProviderID]error{}}
}
func (m *Model) SetRows(rows []session.Session) {
	if m.archiveConfirm != "" {
		m.archiveConfirm = ""
		if m.Status == "Press Ctrl+X again to archive" {
			m.Status = ""
		}
	}
	var selected session.Key
	selectedValid := m.Selected >= 0 && m.Selected < len(m.Rows)
	if selectedValid {
		selected = m.Rows[m.Selected].Key
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
func (m *Model) Update(key string) Action {
	if m.Renaming {
		return m.updateRename(key)
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
		if len(r) > 0 {
			m.Prompt = string(r[:len(r)-1])
		}
		return Action{}
	case "enter":
		if strings.TrimSpace(m.Prompt) != "" {
			return Action{Kind: ActionDispatch, Provider: m.Provider, Prompt: m.Prompt}
		}
		return m.selectedOrStatus(ActionAttach, "open")
	case "open":
		return m.selectedOrStatus(ActionAttach, "open")
	case "folders":
		m.AllDirectories = !m.AllDirectories
		m.Selected = 0
		m.archiveConfirm = ""
		return Action{Kind: ActionRefresh}
	case "stop-or-archive":
		return m.stopOrArchive()
	case "rename":
		row, ok := m.selectedRow()
		if !ok {
			m.Status = "No session selected"
			return Action{}
		}
		if !row.Capabilities.Rename {
			m.Status = capabilityReason(row, "rename")
			return Action{}
		}
		m.Renaming = true
		m.RenameDraft = row.Name
		m.RenameCursor = len([]rune(row.Name))
		m.Status = ""
		return Action{}
	case "refresh":
		return Action{Kind: ActionRefresh}
	case "quit":
		if m.archiveConfirm != "" {
			m.archiveConfirm = ""
			m.Status = "Archive cancelled"
			return Action{}
		}
		return Action{Kind: ActionQuit}
	default:
		if key != "" {
			m.Prompt += key
		}
		return Action{}
	}
}

func (m *Model) cancelArchiveConfirmation() {
	m.archiveConfirm = ""
	if m.Status == "Press Ctrl+X again to archive" {
		m.Status = ""
	}
}

func (m *Model) updateRename(key string) Action {
	runes := []rune(m.RenameDraft)
	switch key {
	case "quit":
		m.Renaming = false
		m.RenameDraft = ""
		m.RenameCursor = 0
		m.Status = "Rename cancelled"
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
	case "enter":
		if strings.TrimSpace(m.RenameDraft) == "" {
			m.Status = "Name must not be empty"
			return Action{}
		}
		a := m.selected(ActionRename)
		a.Prompt = m.RenameDraft
		return a
	default:
		if key != "" {
			insert := []rune(key)
			before := append([]rune(nil), runes[:m.RenameCursor]...)
			after := append([]rune(nil), runes[m.RenameCursor:]...)
			m.RenameDraft = string(append(append(before, insert...), after...))
			m.RenameCursor += len(insert)
		}
	}
	return Action{}
}

func (m *Model) stopOrArchive() Action {
	row, ok := m.selectedRow()
	if !ok {
		m.Status = "No session selected"
		return Action{}
	}
	if row.Capabilities.Stop {
		m.archiveConfirm = ""
		return m.selected(ActionStop)
	}
	if !row.Capabilities.Archive {
		m.Status = capabilityReason(row, "stop or archive")
		return Action{}
	}
	if m.archiveConfirm != row.Key.String() {
		m.archiveConfirm = row.Key.String()
		m.Status = "Press Ctrl+X again to archive"
		return Action{}
	}
	m.archiveConfirm = ""
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
		m.Status = "No session selected"
		return Action{}
	}
	allowed := kind == ActionAttach && row.Capabilities.Attach
	if !allowed {
		m.Status = capabilityReason(row, name)
		return Action{}
	}
	return m.selected(kind)
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
	view := "current folder"
	if m.AllDirectories {
		view = "all folders"
	}
	header := []string{clipLine(fmt.Sprintf("agentsctl · %s", view), width), ""}
	if m.Renaming {
		header[1] = clipLine("Rename: "+renameCursor(m.RenameDraft, m.RenameCursor), width)
	}
	list := make([]displayLine, 0, len(m.Rows)+8)
	groups := []struct {
		title      string
		activities []session.Activity
	}{{"Needs input", []session.Activity{session.ActivityNeedsInput, session.ActivityWaitingQuota}}, {"Working", []session.Activity{session.ActivityStarting, session.ActivityWorking}}, {"Ready", []session.Activity{session.ActivityIdle, session.ActivityUnknown}}, {"Completed", []session.Activity{session.ActivityCompleted, session.ActivityFailed}}}
	for _, g := range groups {
		shown := false
		for i, row := range m.Rows {
			if !contains(g.activities, row.Activity) {
				continue
			}
			if !shown {
				list = append(list, displayLine{text: g.title, rowIndex: -1})
				shown = true
			}
			cursor := " "
			if i == m.Selected {
				cursor = ">"
			}
			if row.Key.String() == m.archiveConfirm {
				cursor = "x"
			}
			line := fmt.Sprintf("%s %-6s %-24.24s %-18.18s %-28.28s", cursor, row.Key.Provider, row.DisplayName(), row.Activity, shortHome(row.CWD))
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
	footer := []string{
		clipLine(fmt.Sprintf("%s%s > %s", m.Provider, unavailable, m.Prompt), width),
		clipLine("Shift+Tab provider / Enter dispatch or attach / Ctrl+O open / Ctrl+A folders", width),
		clipLine("↑↓ select / Ctrl+R rename / Ctrl+X stop or archive / Ctrl+L refresh / Esc quit", width),
	}
	if m.Status != "" {
		footer = append([]string{clipLine("! "+m.Status, width)}, footer...)
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

func renameCursor(value string, cursor int) string {
	runes := []rune(value)
	cursor = min(max(cursor, 0), len(runes))
	return string(runes[:cursor]) + "_" + string(runes[cursor:])
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
	for _, r := range value {
		cells := runeCells(r)
		if used+cells > width {
			break
		}
		b.WriteRune(r)
		used += cells
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
func contains(xs []session.Activity, v session.Activity) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}
func shortHome(v string) string { return v }
