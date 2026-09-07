package agentview

import (
	"fmt"
	"strings"

	"github.com/tingtt/agentsctl/internal/session"
)

// displayLine is one rendered terminal row: rowIndex is the Rows index it
// corresponds to, or -1 for a heading/blank spacer line not tied to any
// session.
type displayLine struct {
	text     string
	rowIndex int
}

// View renders the full Agent View frame: the session list, composer, and
// footer, fit to width x height terminal cells. It performs no I/O and
// calls no sessionctl operation -- a pure transformation of State, so it
// can be tested by constructing a State and comparing rendered text (see
// render_test.go), without a real terminal or provider.
func (s State) View(width, height int) string {
	header := []string{clipLine(fmt.Sprintf("agentsctl · %s", scopeLabel(s.Scope)), width), ""}
	list := make([]displayLine, 0, len(s.Rows)+4)
	selectedIndex := s.SelectedIndex()
	for _, g := range groupRows(s.Rows) {
		list = append(list, displayLine{text: clipLine(styleText(g.title, colorGray), width), rowIndex: -1})
		for _, i := range g.indices {
			row := s.Rows[i]
			cursor := " "
			if i == selectedIndex {
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
			selected := i == selectedIndex
			lastAttached := s.HasLastAttached && row.Key == s.LastAttachedKey
			name = styleText(name, titleStyleCodes(selected, lastAttached)...)
			noticeSegment := ""
			if noticeWidth > 0 {
				noticeSegment = styleText(clipLine(notice.Message, noticeWidth), noticeColor(notice.Severity)) + " "
			}
			provider := styleText(providerLabel(row.Key.Provider), providerColor(row.Key.Provider))
			line := cursor + " " + statusIcon(row.Activity) + " " + name + noticeSegment + provider
			if cwdWidth > 0 {
				cwd := styleText(fitCells(truncateLeftCells(cwdPlain, cwdWidth), cwdWidth), colorGray)
				line += " " + cwd
			}
			list = append(list, displayLine{text: clipLine(line, width), rowIndex: i})
		}
		list = append(list, displayLine{text: "", rowIndex: -1})
	}
	unavailable := ""
	if err := s.Warnings[s.Provider]; err != nil {
		unavailable = " (unavailable: " + err.Error() + ")"
	}
	promptPrefix := composerPrefix(s.Provider, unavailable)
	footer := append(composerLines(s.Composer.Prompt, s.Composer.Cursor, promptPrefix, width),
		clipLine(footerText(footerLine1), width),
		clipLine(footerText(footerLine2), width),
	)
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
	start := viewportStart(list, selectedIndex, listHeight)
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

// composerPrefix builds the prompt composer's "<provider> > " prefix. The
// provider label is fixed to providerFieldWidth visible cells so
// Shift+Tab switching providers never moves the column the prompt body
// starts at.
func composerPrefix(provider session.ProviderID, unavailable string) string {
	return styleText(providerLabel(provider), providerColor(provider)) + unavailable + " > "
}
