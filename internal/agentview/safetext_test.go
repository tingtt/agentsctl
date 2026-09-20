package agentview

import (
	"bytes"
	"errors"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/tingtt/agentsctl/internal/session"
)

// trustedSGR is the only escape sequence Agent View itself emits inside View.
var trustedSGR = regexp.MustCompile("\x1b\\[[0-9;]*m")

// assertOnlyTrustedControls fails if out contains any control byte other than
// the newline row separator and Agent View's own SGR styling: no raw ESC
// (CSI/OSC/DCS/...), BEL, TAB, CR, DEL, or C1 control.
//
// The output must also be valid UTF-8 outside that styling. Ranging over an
// invalid string yields U+FFFD for every bad byte, so a raw 0x9B (8-bit CSI)
// would look like a harmless replacement rune to the rune loop below; the
// validity check is what catches it.
func assertOnlyTrustedControls(t *testing.T, out string) {
	t.Helper()
	clean := trustedSGR.ReplaceAllString(out, "")
	if !utf8.ValidString(clean) {
		t.Fatalf("rendered output is not valid UTF-8 (raw invalid byte, e.g. an 8-bit C1 control, reached the terminal):\n%q", out)
	}
	for _, r := range clean {
		if r != '\n' && (r < 0x20 || r >= 0x7f && r < 0xa0) {
			t.Fatalf("rendered output carries untrusted control %U:\n%q", r, out)
		}
	}
}

func composerView(prompt string, cursor int) string {
	s := NewState()
	s.Composer = Composer{Prompt: prompt, Cursor: cursor}
	return s.View(80, 24)
}

func TestSafeRuneMapsControlsToOnePrintableRune(t *testing.T) {
	cases := map[rune]rune{
		0x00: '␀', '\t': '␉', '\n': '␊', '\r': '␍', 0x07: '␇', 0x1b: '␛', 0x1f: '␟',
		0x7f: '␡', 0x80: '\uFFFD', 0x9b: '\uFFFD', 0x9d: '\uFFFD', 0x9f: '\uFFFD',
		' ': ' ', 'a': 'a', '~': '~', 0xa0: 0xa0, '日': '日',
	}
	for in, want := range cases {
		if got := safeRune(in); got != want {
			t.Errorf("safeRune(%U)=%U, want %U", in, got, want)
		}
	}
	for r := rune(0); r < 0x20; r++ {
		if got := safeRune(r); got < 0x2400 || got > 0x241f {
			t.Errorf("C0 %U did not map into control pictures: %U", r, got)
		}
	}
}

func TestSafeTextKeepsPrintableTextUntouched(t *testing.T) {
	for _, text := range []string{"", "plain", "日本語 こんにちは", "emoji 👍 é ñ", "~/work/repo"} {
		if got := safeText(text); got != text {
			t.Errorf("safeText(%q)=%q, want unchanged", text, got)
		}
	}
	if got := safeText("a\x1b[2Jb\tc"); got != "a␛[2Jb␉c" {
		t.Errorf("safeText=%q", got)
	}
}

// Invalid UTF-8 is model data too, but must not reach the terminal as-is: a raw
// 0x80..0x9F byte is an 8-bit C1 control on terminals that honor it, and Go
// ranges over an invalid byte as U+FFFD, which hides it from a rune loop.
func TestSafeTextCanonicalizesInvalidUTF8(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"raw 8-bit CSI", "c1\x9b2J", "c1\uFFFD2J"},
		{"raw 8-bit OSC and ST", "\x9d0;t\x9c", "\uFFFD0;t\uFFFD"},
		{"every raw C1 byte", "\x80\x85\x9f", "\uFFFD\uFFFD\uFFFD"},
		{"stray continuation and truncated sequence", "a\xbfb\xe6\x97", "a\uFFFDb\uFFFD\uFFFD"},
		{"invalid byte next to valid text and a control", "日\xffx\x1b", "日\uFFFDx␛"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := safeText(tc.in)
			if got != tc.want {
				t.Fatalf("safeText(%q)=%q, want %q", tc.in, got, tc.want)
			}
			if !utf8.ValidString(got) {
				t.Fatalf("safeText(%q)=%q is not valid UTF-8", tc.in, got)
			}
			// Valid UTF-8 cannot hold a lone 0x80..0x9F byte, but it can hold
			// them as continuation bytes of ordinary characters (日, ─), so
			// the check is on runes, not on bytes.
			for _, r := range got {
				if r >= 0x80 && r < 0xa0 {
					t.Fatalf("safeText(%q)=%q contains C1 rune %U", tc.in, got, r)
				}
			}
		})
	}
	// Byte level: the raw 8-bit CSI byte is gone from the output.
	if got := safeText("c1\x9b2J"); bytes.IndexByte([]byte(got), 0x9b) >= 0 || !bytes.Equal([]byte(got), []byte("c1\xef\xbf\xbd2J")) {
		t.Fatalf("safeText(c1<0x9B>2J) bytes=% x, want 63 31 ef bf bd 32 4a", got)
	}
	// The model string is not rewritten: only the returned display text is.
	in := "c1\x9b2J"
	_ = safeText(in)
	if in != "c1\x9b2J" || !bytes.Contains([]byte(in), []byte{0x9b}) {
		t.Fatal("safeText changed its input")
	}
}

func TestPastedCSIDoesNotReachTerminalAsCommand(t *testing.T) {
	const prompt = "hello \x1b[2J world"
	out := composerView(prompt, len(prompt))
	if strings.Contains(out, "\x1b[2J") {
		t.Fatalf("user CSI clear-screen reached the terminal:\n%q", out)
	}
	if !strings.Contains(out, "hello ␛[2J world") {
		t.Fatalf("ESC has no visible safe representation:\n%q", out)
	}
	assertOnlyTrustedControls(t, out)
	// Agent View's own styling is intact.
	if !strings.Contains(out, "\x1b[0m") || !strings.Contains(out, "\x1b[30;47m") {
		t.Fatalf("trusted application ANSI (styling, cursor) is missing:\n%q", out)
	}
}

func TestPastedSGRDoesNotRestyleAgentView(t *testing.T) {
	out := composerView("before\x1b[31mafter", len("before\x1b[31mafter"))
	if strings.Contains(out, "before\x1b") || strings.Contains(out, "\x1b[31mafter") {
		t.Fatalf("user SGR reached the terminal:\n%q", out)
	}
	if !strings.Contains(out, "before␛[31mafter") {
		t.Fatalf("missing safe representation:\n%q", out)
	}
	assertOnlyTrustedControls(t, out)
}

func TestPastedOSCAndBELDoNotReachTerminal(t *testing.T) {
	for _, payload := range []string{
		"\x1b]0;injected title\x07",          // OSC terminated by BEL
		"\x1b]52;c;aGk=\x1b\\",               // OSC terminated by ST
		"\x1bP1$r\x1b\\",                     // DCS
		"\x9b2J\x9d0;title\x9c",              // C1 CSI / OSC / ST
		"\x07\x08\x0b\x0c\x0e\x0f\r\x7f\x00", // remaining controls
	} {
		out := composerView("x"+payload+"y", 0)
		assertOnlyTrustedControls(t, out)
	}
	const osc = "\x1b]0;injected title\x07"
	out := composerView(osc, len(osc))
	if strings.Contains(out, "\x1b]") || strings.Contains(out, "\x07") {
		t.Fatalf("raw OSC/BEL reached the terminal:\n%q", out)
	}
	if !strings.Contains(out, "␛]0;injected title␇") {
		t.Fatalf("missing safe representation:\n%q", out)
	}
}

func TestRenderedFrameKeepsTrustedFramingWhileUserTextIsSafe(t *testing.T) {
	frame := terminalFrame(composerView("a\x1b[2J", 0))
	// The frame's own clear-screen prefix is application-generated and stays;
	// the user's identical sequence does not add a second one.
	if got := strings.Count(frame, "\x1b[2J"); got != 1 {
		t.Fatalf("clear-screen sequences in frame=%d, want only the application's own", got)
	}
}

func TestPasteKeepsModelBytesAndDispatchPayload(t *testing.T) {
	const pasted = "foo\tbar\x1b[2J baz\x07"
	s := NewState()
	s.HandlePaste(pasted)
	if s.Composer.Prompt != pasted {
		t.Fatalf("Prompt=%q, want the pasted bytes preserved", s.Composer.Prompt)
	}
	out := s.View(80, 24)
	assertOnlyTrustedControls(t, out)
	if !strings.Contains(out, "foo␉bar␛[2J baz␇") {
		t.Fatalf("display=%q", out)
	}
	intent := s.HandleInput(keyInput(KeyEvent{Key: KeyEnter}))
	if intent.Kind != IntentDispatch || intent.Prompt != pasted {
		t.Fatalf("intent=%+v, want dispatch of the original bytes including TAB", intent)
	}
	if s.Composer.Prompt != pasted {
		t.Fatalf("rendering or dispatch changed the model: %q", s.Composer.Prompt)
	}
}

func TestSafeDisplayKeepsCursorAndReservedSpanAlignedToModelRunes(t *testing.T) {
	// Cursor right after the ESC, on the ESC, and at the end.
	prompt := "foo\x1bbar"
	cases := []struct {
		cursor int
		want   string
	}{
		{3, "foo" + cursorStyle("␛") + "bar"},
		{4, "foo␛" + cursorStyle("b") + "ar"},
		{7, "foo␛bar" + cursorStyle(" ")},
	}
	for _, tc := range cases {
		row := composerLines(prompt, tc.cursor, "❯ ", 80)[0]
		if !strings.Contains(row, tc.want) {
			t.Errorf("cursor %d: row=%q, want it to contain %q", tc.cursor, row, tc.want)
		}
	}

	// A reserved command followed by TAB: the span still covers exactly
	// "/rename" (TAB is whitespace on the model text) and TAB shows as ␉.
	row := composerLines("/rename\tfoo\x1b", 8, "❯ ", 80)[0]
	if want := styleText("/rename", colorCyan) + "␉" + cursorStyle("f") + "oo␛"; !strings.Contains(row, want) {
		t.Fatalf("row=%q, want it to contain %q", row, want)
	}
	if strings.Contains(row, "\t") {
		t.Fatalf("raw TAB in row=%q", row)
	}
	// Same on a non-cursor row (the cursor is on the second line).
	rows := composerLines("/rename\tx\nz", 11, "❯ ", 80)
	if !strings.Contains(rows[0], styleText("/rename", colorCyan)+"␉x") {
		t.Fatalf("first row=%q", rows[0])
	}
}

func TestSafeDisplayScrollsByModelRunesWithControls(t *testing.T) {
	prompt := strings.Repeat("\x1b", 40) + "tail"
	row := composerLines(prompt, len([]rune(prompt)), "❯ ", 20)[0]
	assertOnlyTrustedControls(t, row)
	if !strings.Contains(row, "tail") || lineCells(row) > 20 {
		t.Fatalf("row=%q cells=%d, want the cursor window at the tail within 20 cells", row, lineCells(row))
	}
}

func renameView(name string, draft string, cursor int) string {
	target := session.Session{Key: key("a"), Name: name, CWD: "/work", Actions: session.Actions{session.ActionRename: {Available: true}}}
	s := NewState()
	s.SetRows([]session.Session{target})
	s.startRename(target)
	s.Rename.Draft, s.Rename.Cursor = draft, cursor
	return s.View(80, 24)
}

func TestRenameEditorDoesNotEmitPastedControls(t *testing.T) {
	out := renameView("old", "foo\x1b[2Jbar\t\x07", 3)
	if strings.Contains(out, "\x1b[2J") {
		t.Fatalf("rename draft CSI reached the terminal:\n%q", out)
	}
	assertOnlyTrustedControls(t, out)
	if !strings.Contains(out, "foo"+cursorStyle("␛")) || !strings.Contains(out, "[2Jbar␉␇") {
		t.Fatalf("cursor/safe display wrong:\n%q", out)
	}
}

func TestPastedRenameKeepsControlBytesInTheModel(t *testing.T) {
	s := NewState()
	target := session.Session{Key: key("a"), Name: "", CWD: "/work", Actions: session.Actions{session.ActionRename: {Available: true}}}
	s.SetRows([]session.Session{target})
	s.startRename(target)
	s.HandlePaste("a\tb\x1b[2J")
	if s.Rename.Draft != "a\tb\x1b[2J" {
		t.Fatalf("Draft=%q, want model bytes preserved", s.Rename.Draft)
	}
	assertOnlyTrustedControls(t, s.View(80, 24))
}

func TestSessionTitleFromProviderDoesNotEmitControls(t *testing.T) {
	for _, name := range []string{"foo\x1b[2Jbar", "t\x1b]0;x\x07", "c1\x9b2J"} {
		row := session.Session{Key: key("a"), Name: name, CWD: "/work"}
		s := NewState()
		s.SetRows([]session.Session{row})
		out := s.View(80, 24)
		assertOnlyTrustedControls(t, out)
	}
	// Summary is a title fallback and is safe too.
	s := NewState()
	s.SetRows([]session.Session{{Key: key("a"), Summary: "sum\x1b[2Jmary", CWD: "/work"}})
	out := s.View(80, 24)
	assertOnlyTrustedControls(t, out)
	if !strings.Contains(out, "sum␛[2Jmary") {
		t.Fatalf("title display=%q", out)
	}
	if s.Rows[0].Summary != "sum\x1b[2Jmary" {
		t.Fatal("rendering rewrote the session model")
	}
}

func TestInvalidUTF8ControlByteInTitleIsShownAsReplacementRune(t *testing.T) {
	const name = "c1\x9b2J"
	s := NewState()
	s.SetRows([]session.Session{{Key: key("a"), Name: name, CWD: "/work"}})
	out := s.View(80, 24)
	assertOnlyTrustedControls(t, out) // valid UTF-8, so no lone 0x9B byte
	if !strings.Contains(out, "c1\uFFFD2J") {
		t.Fatalf("title display=%q, want c1�2J", out)
	}
	if s.Rows[0].Name != name {
		t.Fatalf("rendering rewrote the session model: %q", s.Rows[0].Name)
	}
}

func TestInvalidUTF8InExternalTextDoesNotReachTerminal(t *testing.T) {
	s := NewState()
	s.SetRows([]session.Session{{Key: key("a"), Name: "n", CWD: "/wo\x9b2Jrk"}})
	s.Error = "failed: \x9d0;x\x9c"
	s.Warnings = map[session.ProviderID]error{
		session.ProviderClaude: errors.New("bad \x9b2J"),
		session.ProviderCodex:  errors.New("bad \x9b2J"),
	}
	out := s.View(120, 24)
	assertOnlyTrustedControls(t, out)
	if strings.Count(out, "\uFFFD") < 5 {
		t.Fatalf("invalid bytes are not shown as replacement runes:\n%q", out)
	}
	if s.Error != "failed: \x9d0;x\x9c" || s.Rows[0].CWD != "/wo\x9b2Jrk" {
		t.Fatal("rendering rewrote the source strings")
	}
}

func TestExternalTextInErrorsWarningsAndDirectoriesDoesNotEmitControls(t *testing.T) {
	s := NewState()
	s.SetRows([]session.Session{{Key: key("a"), Name: "n", CWD: "/wo\x1b[2Jrk"}})
	s.Error = "rename \"a\x1b]0;x\x07\" failed"
	s.Warnings = map[session.ProviderID]error{
		session.ProviderClaude: errors.New("bad \x1b[2J"),
		session.ProviderCodex:  errors.New("bad \x9b2J"),
	}
	s.Composer.Prompt = "" // footer path with warnings
	assertOnlyTrustedControls(t, s.View(120, 24))
}
