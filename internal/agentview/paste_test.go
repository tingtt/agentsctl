package agentview

import (
	"testing"

	"github.com/tingtt/agentsctl/internal/session"
)

func pasteState(prompt string, cursor int) State {
	s := NewState()
	s.Composer = Composer{Prompt: prompt, Cursor: cursor}
	return s
}

func TestPasteSplicesAtCursorAndMovesCursorToEnd(t *testing.T) {
	s := pasteState("foobar", 3)
	if intent := s.HandlePaste("hello\nworld"); intent.Kind != IntentNone {
		t.Fatalf("intent=%+v, want none", intent)
	}
	if s.Composer.Prompt != "foohello\nworldbar" {
		t.Fatalf("Prompt=%q", s.Composer.Prompt)
	}
	if want := len([]rune("foohello\nworld")); s.Composer.Cursor != want {
		t.Fatalf("Cursor=%d, want %d (right after the pasted text)", s.Composer.Cursor, want)
	}
}

func TestPasteNormalizesNewlines(t *testing.T) {
	cases := []struct{ name, paste, want string }{
		{"CR", "a\rb", "a\nb"},
		{"CRLF is one newline", "a\r\nb", "a\nb"},
		{"LF", "a\nb", "a\nb"},
		{"mixed", "a\rb\nc\r\nd", "a\nb\nc\nd"},
		{"blank CRLF lines", "a\r\n\r\nb", "a\n\nb"},
		{"trailing CRLF kept", "a\r\n", "a\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := NewState()
			s.HandlePaste(tc.paste)
			if s.Composer.Prompt != tc.want {
				t.Fatalf("Prompt=%q, want %q", s.Composer.Prompt, tc.want)
			}
		})
	}
}

func TestPasteUsesRuneIndexesForUnicode(t *testing.T) {
	s := pasteState("日本語の文", 3)
	s.HandlePaste("こんにちは\n世界")
	if s.Composer.Prompt != "日本語こんにちは\n世界の文" {
		t.Fatalf("Prompt=%q", s.Composer.Prompt)
	}
	if want := len([]rune("日本語こんにちは\n世界")); s.Composer.Cursor != want {
		t.Fatalf("Cursor=%d, want rune index %d", s.Composer.Cursor, want)
	}
}

func TestPasteNeverDispatchesButLaterPlainEnterDispatchesOnce(t *testing.T) {
	s := NewState()
	s.Composer = Composer{Prompt: "ask: ", Cursor: 5}
	// Every newline flavor, many times over: still no intent.
	for _, paste := range []string{"one\r\ntwo\r\n", "\r\r\r", "\n\n", "日本語\rx\ny\r\n"} {
		if intent := s.HandlePaste(paste); intent.Kind != IntentNone {
			t.Fatalf("paste %q produced intent %+v", paste, intent)
		}
	}
	if intent := s.HandleInput(pasteInput("last\nline")); intent.Kind != IntentNone {
		t.Fatalf("HandleInput(paste) produced intent %+v", intent)
	}
	full := s.Composer.Prompt
	if full != "ask: one\ntwo\n\n\n\n\n\n日本語\nx\ny\nlast\nline" {
		t.Fatalf("Prompt=%q", full)
	}

	intent := s.HandleInput(keyInput(KeyEvent{Key: KeyEnter}))
	if intent.Kind != IntentDispatch || intent.Prompt != full {
		t.Fatalf("Enter after paste: intent=%+v, want one dispatch of the full pasted text", intent)
	}
}

func TestPasteDoesNotReplaceExistingTextOrStash(t *testing.T) {
	s := pasteState("keep", 4)
	s.Composer.Stash = "stashed"
	s.HandlePaste(" more")
	if s.Composer.Prompt != "keep more" || s.Composer.Stash != "stashed" {
		t.Fatalf("Composer=%+v", s.Composer)
	}
}

func TestPasteIsIgnoredWhileConfirmationIsPending(t *testing.T) {
	s := NewState()
	s.Confirmation = &PendingConfirmation{Key: key("a"), Action: session.ActionArchive}
	s.HandlePaste("text\nmore")
	if s.Composer.Prompt != "" || s.Confirmation == nil {
		t.Fatalf("Composer=%q Confirmation=%v, want both untouched", s.Composer.Prompt, s.Confirmation)
	}
}

func renameState(name string, cursor int) State {
	s := NewState()
	s.Rename = Rename{Active: true, Target: key("a"), Original: name, Draft: name, Cursor: cursor}
	return s
}

func TestPasteIntoRenameInsertsAtCursor(t *testing.T) {
	s := renameState("foobar", 3)
	if intent := s.HandlePaste("XY"); intent.Kind != IntentNone {
		t.Fatalf("intent=%+v", intent)
	}
	if s.Rename.Draft != "fooXYbar" || s.Rename.Cursor != 5 {
		t.Fatalf("Rename=%+v", s.Rename)
	}
	if s.Composer.Prompt != "" {
		t.Fatalf("composer got the rename paste: %q", s.Composer.Prompt)
	}
}

func TestPasteIntoRenameKeepsUTF8(t *testing.T) {
	s := renameState("名前", 1)
	s.HandlePaste("日本語")
	if s.Rename.Draft != "名日本語前" || s.Rename.Cursor != 4 {
		t.Fatalf("Rename=%+v", s.Rename)
	}
}

func TestPasteIntoRenameNeverSubmitsAndStaysSingleLine(t *testing.T) {
	cases := []struct{ name, paste, want string }{
		{"LF", "a\nb", "a b"},
		{"CRLF", "a\r\nb", "a b"},
		{"CR", "a\rb", "a b"},
		{"trailing newline from a copied line", "name\r\n", "name"},
		{"leading and repeated newlines", "\n\na\r\n\r\nb\n", "a b"},
		{"newlines only", "\r\n\n", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := renameState("", 0)
			if intent := s.HandleInput(pasteInput(tc.paste)); intent.Kind != IntentNone {
				t.Fatalf("paste produced intent %+v", intent)
			}
			if !s.Rename.Active || s.Rename.Draft != tc.want {
				t.Fatalf("Rename=%+v, want still active with draft %q", s.Rename, tc.want)
			}
		})
	}
}

func TestPasteThenEnterSubmitsRenameOnce(t *testing.T) {
	s := renameState("x", 1)
	s.HandlePaste("y\nz")
	intent := s.HandleInput(keyInput(KeyEvent{Key: KeyEnter}))
	if intent.Kind != IntentRename || intent.Name != "xy z" || intent.Key != key("a") {
		t.Fatalf("intent=%+v", intent)
	}
}
