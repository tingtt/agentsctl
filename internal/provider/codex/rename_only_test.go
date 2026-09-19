package codex

import "testing"

func TestParseRenameOnly(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		wantName   string
		wantRename bool
	}{
		{"simple", "/rename foo", "foo", true},
		{"name with spaces", "/rename foo bar", "foo bar", true},
		{"inner spacing is preserved", "/rename foo  bar", "foo  bar", true},
		{"utf-8 name", "/rename 日本語の名前", "日本語の名前", true},
		{"tab separator", "/rename\tfoo", "foo", true},
		{"surrounding whitespace", "  \t/rename   foo bar \t ", "foo bar", true},
		{"trailing newline is not multi-line", "/rename foo\n", "foo", true},
		{"missing name", "/rename", "", true},
		{"blank name", "/rename    ", "", true},
		{"missing name with trailing newline", "/rename\n", "", true},
		{"command word must end", "/renamex foo", "", false},
		{"command word must end for utf-8", "/rename日本語", "", false},
		{"not at start", "hello /rename foo", "", false},
		{"multi-line with task", "/rename foo\nimplement issue #46", "", false},
		{"multi-line crlf", "/rename foo\r\nbar", "", false},
		{"name on next line", "/rename\nfoo", "", false},
		{"ordinary prompt", "implement issue #46", "", false},
		{"empty", "", "", false},
		{"other slash command", "/review foo", "", false},
		{"different case", "/Rename foo", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			name, isRename := parseRenameOnly(tc.input)
			if name != tc.wantName || isRename != tc.wantRename {
				t.Fatalf("parseRenameOnly(%q) = (%q, %v), want (%q, %v)", tc.input, name, isRename, tc.wantName, tc.wantRename)
			}
		})
	}
}
