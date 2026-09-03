package tui

import (
	"bufio"
	"strings"
	"testing"

	"github.com/tingtt/agentsctl/internal/session"
)

func TestDefaultDirectoryDepthIsTwo(t *testing.T) {
	if got := NewModel().CWDDepth; got != 2 {
		t.Fatalf("default CWDDepth=%d, want 2", got)
	}
}

func TestCtrlSlashCyclesDirectoryDepth(t *testing.T) {
	m := NewModel()
	want := []int{3, CWDDepthAll, 1, 2, 3}
	for i, w := range want {
		m.Update("depth-cycle")
		if m.CWDDepth != w {
			t.Fatalf("step %d: CWDDepth=%d, want %d", i, m.CWDDepth, w)
		}
	}
}

func TestDirectoryDepthTrailingComponents(t *testing.T) {
	const home = "/Users/tingtt"
	path := home + "/ghq/github.com/tingtt/agentsctl"
	cases := []struct {
		depth int
		want  string
	}{
		{1, "agentsctl"},
		{2, "tingtt/agentsctl"},
		{3, "github.com/tingtt/agentsctl"},
	}
	for _, tc := range cases {
		if got := trailingComponents(path, home, tc.depth); got != tc.want {
			t.Fatalf("depth=%d got=%q want=%q", tc.depth, got, tc.want)
		}
	}
	// depth = all uses the shortHome-abbreviated full path, not trailing
	// components; displayCWD is the entry point that dispatches to it.
	// shortHome reads the process's real $HOME, so pin it to home here.
	// displayCWD itself never appends the row-rendering trailing "/" --
	// that's withTrailingSlash's job, applied by the caller (model.View)
	// after displayCWD returns; see TestWithTrailingSlash and
	// TestDisplayCWDDepthsCarryTrailingSlashThroughRowRendering in
	// title_color_test.go for that.
	t.Setenv("HOME", home)
	if got := displayCWD(path, CWDDepthAll); got != "~/ghq/github.com/tingtt/agentsctl" {
		t.Fatalf("all mode = %q, want shortHome-abbreviated full path", got)
	}
}

func TestHomeItselfIsNotCountedAsAPathLevel(t *testing.T) {
	const home = "/Users/tingtt"
	// HOME itself has zero components below it, regardless of depth.
	for _, depth := range []int{1, 2, 3} {
		if got := trailingComponents(home, home, depth); got != "~" {
			t.Fatalf("depth=%d got=%q want=%q", depth, got, "~")
		}
	}
}

func TestShallowPathsShowAllExistingComponents(t *testing.T) {
	const home = "/Users/tingtt"
	if got := trailingComponents(home+"/agentsctl", home, 3); got != "agentsctl" {
		t.Fatalf("shallow path got=%q, want single component with no forced slash/ellipsis", got)
	}
	if got := trailingComponents(home+"/a/b", home, 5); got != "a/b" {
		t.Fatalf("shallow path got=%q, want the existing two components", got)
	}
}

func TestAbsolutePathOutsideHomeUsesTrailingComponents(t *testing.T) {
	const home = "/Users/tingtt"
	path := "/opt/work/github.com/tingtt/agentsctl"
	if got := trailingComponents(path, home, 2); got != "tingtt/agentsctl" {
		t.Fatalf("got=%q, want %q", got, "tingtt/agentsctl")
	}
}

func TestDirectoryDepthHandlesJapaneseComponents(t *testing.T) {
	const home = "/Users/tingtt"
	path := home + "/プロジェクト/エージェント/agentsctl"
	if got := trailingComponents(path, home, 2); got != "エージェント/agentsctl" {
		t.Fatalf("got=%q", got)
	}
	if got := trailingComponents(path, home, 1); got != "agentsctl" {
		t.Fatalf("got=%q", got)
	}
}

func TestAllModeStillLeftTruncatesWhenTerminalIsNarrow(t *testing.T) {
	const home = "/Users/tingtt"
	t.Setenv("HOME", home)
	long := home + "/ghq/github.com/tingtt/a-fairly-long-repository-name"
	full := displayCWD(long, CWDDepthAll)
	if full != "~/ghq/github.com/tingtt/a-fairly-long-repository-name" {
		t.Fatalf("fixture assumption broke: full=%q", full)
	}
	got := truncateLeftCells(full, 20)
	if !strings.HasPrefix(got, "…") {
		t.Fatalf("narrow all-mode CWD was not left-truncated: %q", got)
	}
	if !strings.HasSuffix(got, "repository-name") {
		t.Fatalf("narrow all-mode CWD dropped the tail directory: %q", got)
	}

	// Same interaction through the full row renderer.
	m := NewModel()
	m.CWDDepth = CWDDepthAll
	m.SetRows([]session.Session{
		{Key: session.Key{Provider: session.ProviderCodex, ID: "a"}, Name: "s", Activity: session.ActivityIdle, CWD: long},
	})
	view := m.View(50, 10)
	line := rowLine(t, view, 3) // header, blank, "Other", row
	if !strings.Contains(line, "…") || !strings.Contains(line, "repository-name") {
		t.Fatalf("rendered row did not left-truncate the all-mode CWD:\n%s", line)
	}
}

func TestReadKeyDecodesCtrlSlashAsDepthCycle(t *testing.T) {
	// Ctrl+/ arrives as the C0 code 0x1F on the terminals this project
	// targets (macOS Terminal.app, iTerm2 — both xterm-compatible), the
	// same convention Ctrl+_ (readline undo) relies on.
	r := bufio.NewReader(strings.NewReader("\x1f"))
	got, err := readKey(r)
	if err != nil || got != "depth-cycle" {
		t.Fatalf("got=%q err=%v", got, err)
	}
}

func TestDepthCycleDoesNotTriggerCatalogRefresh(t *testing.T) {
	m := NewModel()
	if action := m.Update("depth-cycle"); action.Kind != ActionNone {
		t.Fatalf("depth-cycle triggered action=%+v, want no provider refresh", action)
	}
}

func TestDepthCycleSetsTransientStatus(t *testing.T) {
	m := NewModel()
	m.Update("depth-cycle") // 2 -> 3
	if m.Status != "Directory depth: 3" {
		t.Fatalf("status=%q", m.Status)
	}
	m.Update("depth-cycle") // 3 -> all
	if m.Status != "Directory depth: all" {
		t.Fatalf("status=%q", m.Status)
	}
}
