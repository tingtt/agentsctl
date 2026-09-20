package agentview

import "unicode"

// reservedCommands are the composer commands agentsctl owns itself. The
// composer marks one visually while it is typed; this is token syntax only,
// not a check that the rest of the prompt makes the command executable.
// Whether a command is also handled inside agentsctl rather than dispatched
// to a provider is a separate matter (see updateCommand).
var reservedCommands = map[string]bool{
	"/rename":     true,
	updateCommand: true,
}

// runeSpan is a half-open [start, end) range of rune indexes. The zero
// value is the empty span.
type runeSpan struct {
	start, end int
}

// reservedCommandSpan returns the rune span of prompt's first
// non-whitespace token when that token is exactly a reserved command. The
// token ends at whitespace or end-of-input, so "/rename-foo" and
// "/rename日本語" do not match.
func reservedCommandSpan(prompt string) (runeSpan, bool) {
	runes := []rune(prompt)
	start := 0
	for start < len(runes) && unicode.IsSpace(runes[start]) {
		start++
	}
	end := start
	for end < len(runes) && !unicode.IsSpace(runes[end]) {
		end++
	}
	if !reservedCommands[string(runes[start:end])] {
		return runeSpan{}, false
	}
	return runeSpan{start: start, end: end}, true
}

// paintReservedCommand renders runes, which begin at rune index offset of
// the prompt span was computed on, coloring the part inside span with
// colorCyan. Segments outside span are returned unstyled, and no empty
// styled segment is emitted.
func paintReservedCommand(runes []rune, offset int, span runeSpan) string {
	from := min(max(span.start-offset, 0), len(runes))
	to := min(max(span.end-offset, from), len(runes))
	if from == to {
		return string(runes)
	}
	return string(runes[:from]) + styleText(string(runes[from:to]), colorCyan) + string(runes[to:])
}
