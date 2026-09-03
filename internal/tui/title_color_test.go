package tui

import (
	"strings"
	"testing"

	"github.com/tingtt/agentsctl/internal/session"
)

func TestClaudeTitleUsesFixedTrueColor(t *testing.T) {
	if got := providerTitleColor(session.ProviderClaude); got != "\x1b[38;2;217;119;87m" {
		t.Fatalf("Claude title color=%q, want the #D97757 truecolor escape", got)
	}
}

func TestCodexTitleUsesFixedTrueColor(t *testing.T) {
	if got := providerTitleColor(session.ProviderCodex); got != "\x1b[38;2;83;104;235m" {
		t.Fatalf("Codex title color=%q, want the #5368EB truecolor escape", got)
	}
}

func TestSessionRowsCarryProviderTrueColorInTitle(t *testing.T) {
	m := NewModel()
	m.SetRows([]session.Session{
		{Key: session.Key{Provider: session.ProviderClaude, ID: "a"}, Name: "claude-session", Activity: session.ActivityIdle, CWD: "/work"},
		{Key: session.Key{Provider: session.ProviderCodex, ID: "b"}, Name: "codex-session", Activity: session.ActivityIdle, CWD: "/work"},
	})
	view := m.View(80, 12)
	if !strings.Contains(view, "\x1b[38;2;217;119;87m") {
		t.Fatalf("Claude row missing #D97757 truecolor escape:\n%s", view)
	}
	if !strings.Contains(view, "\x1b[38;2;83;104;235m") {
		t.Fatalf("Codex row missing #5368EB truecolor escape:\n%s", view)
	}
	if !strings.Contains(view, "\x1b[38;2;217;119;87mclaude-session") {
		t.Fatalf("Claude title text is not immediately preceded by its provider color:\n%s", view)
	}
	if !strings.Contains(view, "\x1b[38;2;83;104;235mcodex-session") {
		t.Fatalf("Codex title text is not immediately preceded by its provider color:\n%s", view)
	}
}

func TestStatusGlyphSurvivesRunnerColumnRemoval(t *testing.T) {
	m := NewModel()
	m.SetRows([]session.Session{
		{Key: session.Key{Provider: session.ProviderClaude, ID: "a"}, Name: "s", Activity: session.ActivityFailed, CWD: "/work"},
	})
	view := m.View(80, 12)
	if !strings.Contains(view, statusIcon(session.ActivityFailed)) {
		t.Fatalf("status glyph is missing from the row:\n%s", view)
	}
}

func TestJapaneseColoredTitleDoesNotShiftCWDColumn(t *testing.T) {
	cwd := "/work/project"
	rows := []session.Session{
		{Key: session.Key{Provider: session.ProviderClaude, ID: "a"}, Name: "short", Activity: session.ActivityIdle, CWD: cwd},
		{Key: session.Key{Provider: session.ProviderClaude, ID: "b"}, Name: "日本語のタイトル", Activity: session.ActivityWorking, CWD: cwd},
		{Key: session.Key{Provider: session.ProviderCodex, ID: "c"}, Name: "mix 混在 title", Activity: session.ActivityFailed, CWD: cwd},
	}
	m := NewModel()
	m.SetRows(rows)
	view := m.View(80, 12)
	var offsets []int
	for i := range rows {
		line := rowLine(t, view, i+3)
		byteIdx := strings.Index(line, "project")
		if byteIdx < 0 {
			t.Fatalf("cwd text not found: %q", line)
		}
		offsets = append(offsets, lineCells(line[:byteIdx]))
	}
	for i := 1; i < len(offsets); i++ {
		if offsets[i] != offsets[0] {
			t.Fatalf("cwd column shifted with provider-colored CJK titles: offsets=%v\n%s", offsets, view)
		}
	}
}

func TestClippingIgnoresANSIBytesInProviderColoredTitle(t *testing.T) {
	m := NewModel()
	m.SetRows([]session.Session{
		{Key: session.Key{Provider: session.ProviderClaude, ID: "a"}, Name: strings.Repeat("n", 60), Activity: session.ActivityIdle, CWD: "/work"},
	})
	// A very narrow viewport forces title clipping; the row must still
	// render as valid, well-formed lines (no ANSI bytes counted as
	// visible width, no line exceeding the terminal width).
	view := m.View(24, 10)
	for _, line := range strings.Split(strings.TrimRight(view, "\n"), "\n") {
		if cells := lineCells(line); cells > 24 {
			t.Fatalf("line exceeds width after clipping provider-colored title: cells=%d %q", cells, line)
		}
	}
}

func TestRenameCursorPreservesProviderColorBeforeAndAfter(t *testing.T) {
	m := NewModel()
	m.Rows = []session.Session{{Key: session.Key{Provider: session.ProviderClaude, ID: "c"}, Name: "old", Activity: session.ActivityIdle, Capabilities: session.Capabilities{Rename: true}}}
	m.Update("rename")
	m.Update("home")
	m.Update("right") // cursor now sits on the second rune ('l'), with "d" trailing
	view := m.View(80, 12)
	want := coloredCursorSuffix(session.ProviderClaude, "l", "d")
	if !strings.Contains(view, "o"+want) {
		t.Fatalf("rename cursor did not preserve Claude's provider color across the cursor glyph:\n%s\nwant substring: %q", view, "o"+want)
	}
}

func TestEndOfTitleCursorRendersWithProviderColor(t *testing.T) {
	m := NewModel()
	m.Rows = []session.Session{{Key: session.Key{Provider: session.ProviderCodex, ID: "c"}, Name: "old", Activity: session.ActivityIdle, Capabilities: session.Capabilities{Rename: true}}}
	m.Update("rename") // cursor starts at the end
	view := m.View(80, 12)
	if !strings.Contains(view, providerTitleColor(session.ProviderCodex)+"old"+cursorStyle(" ")) {
		t.Fatalf("end-of-title cursor did not render inside Codex's provider color:\n%s", view)
	}
}
