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
// portion, on top of the CWD: the provider field plus the single separator
// space between the CWD and it (the row renders title/notice -> CWD ->
// provider; see the DesignDoc's Pinned row layout).
const rowRightFixed = providerFieldWidth + 1

// splitRowWidth lays out a session row in strict priority order --
// provider field + CWD first, then an optional row notice, then the title
// gets whatever cells remain -- so the row fills the terminal width
// exactly. This priority order is a width *budget*, independent of the
// row's actual left-to-right text order (title/notice -> CWD -> provider,
// see render.go): the provider field is always reserved in full and never
// shrinks, and CWD is reserved next out of what's left, so both keep their
// full width under pressure before the title or notice give up any of
// theirs. A notice never takes width from CWD/provider. cwdCells == 0
// means the row carries no CWD column at all (see groupRows'
// showCWD -- most rows since #14 don't, their group heading says the
// directory instead), in which case no width is reserved for the
// separator space a CWD column would otherwise need either.
func splitRowWidth(width, cwdCells, noticeCells int) (title, notice, cwd int) {
	rightFixed := providerFieldWidth
	if cwdCells > 0 {
		rightFixed = rowRightFixed
	}
	available := width - rowLeftFixed - rightFixed
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

// displayCWD renders row's working directory as the shortHome-abbreviated
// full path (see Slice B / #14: the previous depth-limited display and its
// Ctrl+/ toggle are gone -- a session row shows either no CWD at all in a
// same-directory scope, or its full abbreviated path in a multi-directory
// scope).
func displayCWD(path string) string {
	return shortHome(path)
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

// clipLine clips value to width visible cells, tolerating embedded ANSI
// SGR sequences (zero-width, always copied whole). If clipping cuts value
// off before an ANSI sequence it contains ever closes its own style (e.g.
// a styleText-colored cwd/rule too long for a narrow terminal), a trailing
// reset is appended -- otherwise that dangling color would bleed into
// whatever this line's caller writes after it (see the DesignDoc's Width
// priority section: degrading gracefully under narrow width must not also
// corrupt unrelated later output).
func clipLine(value string, width int) string {
	if width <= 0 {
		return ""
	}
	used, sawANSI, truncated := 0, false, false
	var b strings.Builder
	for i := 0; i < len(value); {
		if value[i] == 0x1b {
			j := skipANSI(value, i)
			b.WriteString(value[i:j])
			sawANSI = true
			i = j
			continue
		}
		r, size := utf8.DecodeRuneInString(value[i:])
		cells := runeCells(r)
		if used+cells > width {
			truncated = true
			break
		}
		b.WriteRune(r)
		used += cells
		i += size
	}
	if truncated && sawANSI && !strings.HasSuffix(b.String(), "\x1b[0m") {
		b.WriteString("\x1b[0m")
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
