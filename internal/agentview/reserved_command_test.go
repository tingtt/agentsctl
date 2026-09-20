package agentview

import (
	"strings"
	"testing"
)

// paintClasses replays the SGR sequences in row and returns one class rune
// per visible rune: 'c' for reserved-command cyan, 'X' for the reverse-video
// cursor cell, 'w' for anything else (normal composer white or unstyled).
// dangling reports whether a non-default style is still open at the end of
// row, i.e. would bleed into whatever is written next.
func paintClasses(row string) (classes string, dangling bool) {
	style := 'w'
	var b strings.Builder
	for i := 0; i < len(row); {
		if row[i] == 0x1b {
			j := skipANSI(row, i)
			switch seq := row[i:j]; seq {
			case "\x1b[0m":
				style = 'w'
			case "\x1b[" + colorWhite + "m":
				style = 'w'
			case "\x1b[" + colorCyan + "m":
				style = 'c'
			case "\x1b[30;47m":
				style = 'X'
			default:
				panic("unexpected SGR sequence in composer row: " + seq)
			}
			i = j
			continue
		}
		r := []rune(row[i:])[0]
		b.WriteByte(byte(style))
		i += len(string(r))
	}
	return b.String(), style != 'w'
}

// TestReservedCommandSpan fixes the token rule: only the first
// non-whitespace token, ended by whitespace or end-of-input, and only in
// exact case.
func TestReservedCommandSpan(t *testing.T) {
	tests := []struct {
		name   string
		prompt string
		want   runeSpan
		ok     bool
	}{
		{"bare", "/rename", runeSpan{0, 7}, true},
		{"with argument", "/rename foo", runeSpan{0, 7}, true},
		{"japanese argument", "/rename 日本語", runeSpan{0, 7}, true},
		{"leading whitespace", "  /rename foo", runeSpan{2, 9}, true},
		{"leading newline", "\n/rename foo", runeSpan{1, 8}, true},
		{"tab separator", "/rename\tfoo", runeSpan{0, 7}, true},
		{"hyphen suffix", "/rename-foo", runeSpan{}, false},
		{"japanese suffix", "/rename日本語", runeSpan{}, false},
		{"different case", "/Rename foo", runeSpan{}, false},
		{"not first token", "hello /rename foo", runeSpan{}, false},
		{"empty", "", runeSpan{}, false},
		{"whitespace only", "  ", runeSpan{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := reservedCommandSpan(tt.prompt)
			if got != tt.want || ok != tt.ok {
				t.Fatalf("reservedCommandSpan(%q) = %v, %v; want %v, %v", tt.prompt, got, ok, tt.want, tt.ok)
			}
		})
	}
}

// TestComposerLinesStylesReservedCommandAroundCursor fixes which cells are
// cyan (command token), normal (everything else) and reverse-video (cursor)
// for each cursor position, including inside the token the cursor splits.
func TestComposerLinesStylesReservedCommandAroundCursor(t *testing.T) {
	tests := []struct {
		name   string
		prompt string
		cursor int
		want   string
	}{
		{"cursor before command", "/rename foo", 0, "Xccccccwwww"},
		{"cursor inside command", "/rename foo", 3, "cccXcccwwww"},
		{"cursor right after command", "/rename foo", 7, "cccccccXwww"},
		{"cursor in argument", "/rename foo", 9, "cccccccwwXw"},
		{"cursor at end", "/rename foo", 11, "cccccccwwwwX"},
		{"bare command, cursor at end", "/rename", 7, "cccccccX"},
		{"japanese argument", "/rename 日本語", 0, "Xccccccwwwww"},
		{"leading whitespace", "  /rename foo", 13, "wwcccccccwwwwX"},
		{"tab separator", "/rename\tfoo", 0, "Xccccccwwwww"},
		{"hyphen suffix", "/rename-foo", 0, "Xwwwwwwwwww"},
		{"japanese suffix", "/rename日本語", 0, "Xwwwwwwwww"},
		{"different case", "/Rename foo", 0, "Xwwwwwwwwww"},
		{"not first token", "hello /rename foo", 0, "Xwwwwwwwwwwwwwwww"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rows := composerLines(tt.prompt, tt.cursor, "", 40)
			if len(rows) != 1 {
				t.Fatalf("got %d rows, want 1", len(rows))
			}
			got, dangling := paintClasses(rows[0])
			if dangling {
				t.Fatalf("row leaves a style open: %q", rows[0])
			}
			got = strings.TrimRight(got, "w") // drop the space padding to width
			want := strings.TrimRight(tt.want, "w")
			if got != want {
				t.Fatalf("classes = %q, want %q (row %q)", got, want, rows[0])
			}
		})
	}
}

// TestComposerLinesStylesReservedCommandAcrossLines fixes that the command
// is found across leading newlines and that the cursor on another logical
// line neither loses it nor spreads its color to continuation rows.
func TestComposerLinesStylesReservedCommandAcrossLines(t *testing.T) {
	tests := []struct {
		name   string
		prompt string
		cursor int
		want   []string
	}{
		{"cursor on next line", "/rename\nfoo", 8, []string{"ccccccc", "Xww"}},
		{"cursor on command line", "/rename\nfoo", 2, []string{"ccXcccc", "www"}},
		{"command after leading newline", "\n/rename foo", 11, []string{"", "cccccccwwwX"}},
		{"command word on a later line is not the first token", "foo\n/rename", 4, []string{"www", "Xwwwwww"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rows := composerLines(tt.prompt, tt.cursor, "", 40)
			if len(rows) != len(tt.want) {
				t.Fatalf("got %d rows, want %d", len(rows), len(tt.want))
			}
			for i, row := range rows {
				got, dangling := paintClasses(row)
				if dangling {
					t.Fatalf("row %d leaves a style open: %q", i, row)
				}
				if got = strings.TrimRight(got, "w"); got != strings.TrimRight(tt.want[i], "w") {
					t.Fatalf("row %d classes = %q, want %q (row %q)", i, got, tt.want[i], row)
				}
			}
		})
	}
}

// TestComposerLinesStylesPartiallyVisibleReservedCommand fixes a narrow
// composer: whatever part of the token is scrolled or clipped into view
// stays cyan, the width stays exact, and the clipped style is closed.
func TestComposerLinesStylesPartiallyVisibleReservedCommand(t *testing.T) {
	tests := []struct {
		name   string
		prompt string
		cursor int
		width  int
		want   string
	}{
		{"scrolled right into the token tail", "/rename foo", 11, 6, "cwwwwX"},
		{"scrolled so only the token tail shows", "/rename foo bar baz", 6, 4, "cccX"},
		{"clipped on the right", "/rename foo", 0, 4, "Xccc"},
		{"clipped inside the token", "/rename", 3, 5, "cccXc"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rows := composerLines(tt.prompt, tt.cursor, "", tt.width)
			if got := lineCells(rows[0]); got != tt.width {
				t.Fatalf("row is %d cells, want %d: %q", got, tt.width, rows[0])
			}
			got, dangling := paintClasses(rows[0])
			if dangling {
				t.Fatalf("row leaves a style open: %q", rows[0])
			}
			if got != tt.want {
				t.Fatalf("classes = %q, want %q (row %q)", got, tt.want, rows[0])
			}
		})
	}
}

// TestComposerRowKeepsRenamePromptUnstyled fixes that highlighting is a
// rendering concern: the raw prompt carries no ANSI, and the composer's
// outer white style composes with the cyan span without leaking it.
func TestComposerRowKeepsRenamePromptUnstyled(t *testing.T) {
	s := State{Composer: Composer{Prompt: "/rename foo", Cursor: 3}}
	if strings.Contains(s.Composer.Prompt, "\x1b") {
		t.Fatalf("prompt contains ANSI: %q", s.Composer.Prompt)
	}
	lines := s.composerLines(40)
	if len(lines) < 2 {
		t.Fatalf("composer rendered %d lines", len(lines))
	}
	got, dangling := paintClasses(lines[1])
	if dangling {
		t.Fatalf("prompt row leaves a style open: %q", lines[1])
	}
	want := "wwcccXcccwwww" // "❯ " prefix cells, then the prompt
	if strings.TrimRight(got, "w") != strings.TrimRight(want, "w") {
		t.Fatalf("classes = %q, want %q (row %q)", got, want, lines[1])
	}
	if s.Composer.Prompt != "/rename foo" {
		t.Fatalf("prompt mutated: %q", s.Composer.Prompt)
	}
}
