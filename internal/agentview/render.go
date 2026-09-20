package agentview

import (
	"fmt"
	"strings"
)

// displayLine is one rendered terminal row. Selectable rows carry the exact
// Agent View cursor identity used by navigation; headings and separators do
// not.
type displayLine struct {
	text       string
	itemID     listItemID
	selectable bool
}

// View renders the full Agent View frame: the session list, composer, and
// footer, fit to width x height terminal cells. It performs no I/O and
// calls no sessionctl operation -- a pure transformation of State, so it
// can be tested by constructing a State and comparing rendered text (see
// render_test.go), without a real terminal or provider.
func (s State) View(width, height int) string {
	headerText := fmt.Sprintf("agentsctl · %s", scopeLabel(s.Scope))
	if s.CatalogLoading {
		headerText += styleText(" · loading sessions…", colorGray)
	}
	header := []string{clipLine(headerText, width), ""}
	model := s.selectableList()
	list := make([]displayLine, 0, len(model.items)+len(model.groups)*2)
	for _, g := range model.groups {
		list = append(list, displayLine{text: clipLine(styleText(g.title, colorGray), width)})
		for _, item := range g.items {
			selected := s.hasCursor && s.cursor == item.id
			if item.id.kind != listItemSession {
				label := "Show more"
				if item.id.kind == listItemShowSessions {
					label = "Show sessions"
				}
				cursor := " "
				if selected {
					cursor = ">"
				}
				line := cursor + " " + styleText(label, colorGray)
				if selected {
					line = styleText(fitCells(line, width), selectedRowBackgroundCode)
				}
				list = append(list, displayLine{text: clipLine(line, width), itemID: item.id, selectable: true})
				continue
			}

			i := item.rowIndex
			row := s.Rows[i]
			cursor := " "
			if selected {
				cursor = ">"
			}
			if s.Confirmation != nil && row.Key == s.Confirmation.Key {
				cursor = "x"
			}
			var cwdPlain string
			if g.showCWD {
				cwdPlain = displayCWD(row.CWD)
			}
			notice, hasNotice := s.rowNotice(row.Key)
			noticeCells := 0
			if hasNotice {
				noticeCells = lineCells(notice.Message)
			}
			titleWidth, noticeWidth, cwdWidth := splitRowWidth(width, lineCells(cwdPlain), noticeCells)
			var name string
			if s.Rename.Active && row.Key == s.Rename.Target {
				name = cursorWindow(s.Rename.Draft, s.Rename.Cursor, titleWidth)
			} else {
				name = fitCells(row.DisplayName(), titleWidth)
			}
			lastAttached := s.HasLastAttached && row.Key == s.LastAttachedKey
			name = styleText(name, titleStyleCodes(selected, lastAttached)...)
			noticeSegment := ""
			if noticeWidth > 0 {
				noticeSegment = styleText(clipLine(notice.Message, noticeWidth), noticeColor(notice.Severity)) + " "
			}
			provider := styleText(providerLabel(row.Key.Provider), providerColor(row.Key.Provider))
			line := cursor + " " + statusIcon(row.Activity) + " " + name + noticeSegment
			if cwdWidth > 0 {
				cwd := styleText(fitCells(truncateLeftCells(cwdPlain, cwdWidth), cwdWidth), colorGray)
				line += cwd + " "
			}
			line += provider
			if selected {
				line = styleText(line, selectedRowBackgroundCode)
			}
			list = append(list, displayLine{text: clipLine(line, width), itemID: item.id, selectable: true})
		}
		list = append(list, displayLine{text: ""})
	}

	footer := s.composerLines(width)
	// The composer-top notification area is reserved for Error
	// exclusively -- there is no generic non-error notice here.
	if s.Error != "" {
		footer = append([]string{clipLine("! "+s.Error, width)}, footer...)
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
	start := viewportStart(list, s.cursor, s.hasCursor, listHeight)
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

// composerLines renders #14's composer block: a top rule carrying the
// composer's directory context, the prompt itself, a bottom rule, and
// then either the help view or the contextual footer + usage lines --
// never both (see State.HelpVisible).
func (s State) composerLines(width int) []string {
	lines := make([]string, 0, 8)
	lines = append(lines, topRule(s.ComposerCWD(), width))
	for _, line := range composerLines(s.Composer.Prompt, s.Composer.Cursor, "❯ ", width) {
		lines = append(lines, styleText(line, colorWhite))
	}
	lines = append(lines, bottomRule(width))
	if s.HelpVisible {
		return append(lines, helpLines(width)...)
	}
	lines = append(lines, clipLine("  "+contextualFooterText(s), width))
	lines = append(lines, clipLine("  "+usageLineText(s), width))
	return lines
}
