package agentview

import (
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

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
// cell marks it instead.
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

// composerLines renders prompt as one visual row per logical line (split
// on embedded "\n"), so an embedded newline shows as a separate terminal
// row instead of a literal control character folded into one
// horizontally-scrolled line. The first row carries prefix; continuation
// rows are left-padded to the same cell width so the prompt body stays
// visually aligned under it.
func composerLines(prompt string, cursor int, prefix string, width int) []string {
	lines := strings.Split(prompt, "\n")
	cursorLine, cursorCol := promptCursorPosition(lines, cursor)
	indent := strings.Repeat(" ", lineCells(prefix))
	available := max(1, width-lineCells(prefix))
	rows := make([]string, len(lines))
	for i, line := range lines {
		var rendered string
		if i == cursorLine {
			rendered = cursorWindow(line, cursorCol, available)
		} else {
			rendered = fitCells(line, available)
		}
		leader := prefix
		if i > 0 {
			leader = indent
		}
		rows[i] = clipLine(leader+rendered, width)
	}
	return rows
}

// promptCursorPosition converts cursor -- a rune index into the full
// prompt string, counting every rune including embedded "\n" -- into the
// (logical line index, rune offset within that line) pair matching lines
// (prompt split on "\n").
func promptCursorPosition(lines []string, cursor int) (line, col int) {
	remaining := cursor
	for i, l := range lines {
		n := len([]rune(l))
		if remaining <= n {
			return i, remaining
		}
		remaining -= n + 1 // +1 for the "\n" separating this line from the next
	}
	return len(lines) - 1, len([]rune(lines[len(lines)-1]))
}

// rowLeftFixed is the terminal-cell width of a session row's left prefix
// before the title starts: cursor(1) + separator(1) + status(1) +
// separator(1).
const rowLeftFixed = 4

// rowRightFixed is the terminal-cell width of the right block's fixed
// portion, before the CWD: the provider field plus the single separator
// space between it and the CWD.
const rowRightFixed = providerFieldWidth + 1

// splitRowWidth lays out a session row in strict priority order --
// provider field + CWD first, then an optional row notice, then the title
// gets whatever cells remain -- so the row fills the terminal width
// exactly. A notice never takes width from CWD/provider.
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
	if remaining >= 2 {
		return 0, remaining - 1, cwd
	}
	return 0, 0, cwd
}

// fitCells clips or space-pads value to exactly width terminal cells,
// tolerating embedded ANSI SGR sequences (zero-width).
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

// displayCWD renders row's working directory per the active
// directory-depth mode: depth 1-3 show that many trailing path components
// (HOME is never counted as a component); CWDDepthAll shows the
// shortHome-abbreviated full path.
func displayCWD(path string, depth int) string {
	if depth == CWDDepthAll {
		return shortHome(path)
	}
	home, _ := os.UserHomeDir()
	return trailingComponents(path, home, depth)
}

// withTrailingSlash appends a directory separator "/" to a displayed CWD,
// unless value already ends in one.
func withTrailingSlash(value string) string {
	if strings.HasSuffix(value, "/") {
		return value
	}
	return value + "/"
}

// trailingComponents returns the last n path components of path (HOME
// stripped and not counted as a component).
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
// "~".
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
// characters from the left (prefixing an ellipsis) so the tail stays
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
// ANSI CSI escape sequences (zero visible cells).
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
// CSI sequence.
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
