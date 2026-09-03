package tui

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/tingtt/agentsctl/internal/session"
	"golang.org/x/term"
)

func TestShiftTabPreservesPrompt(t *testing.T) {
	m := NewModel()
	m.Prompt = "keep me"
	m.Update("shift+tab")
	if m.Provider != session.ProviderCodex || m.Prompt != "keep me" {
		t.Fatalf("model=%+v", m)
	}
}

func TestSelectionFollowsSessionIdentityAcrossRefresh(t *testing.T) {
	m := NewModel()
	m.SetRows([]session.Session{
		{Key: session.Key{Provider: session.ProviderCodex, ID: "first"}},
		{Key: session.Key{Provider: session.ProviderCodex, ID: "selected"}},
	})
	m.Selected = 1
	m.SetRows([]session.Session{
		{Key: session.Key{Provider: session.ProviderCodex, ID: "selected"}},
		{Key: session.Key{Provider: session.ProviderCodex, ID: "first"}},
	})
	if m.Selected != 0 || m.Rows[m.Selected].Key.ID != "selected" {
		t.Fatalf("selection=%d row=%+v", m.Selected, m.Rows[m.Selected])
	}
}

func TestViewIsBoundedAndKeepsSelectionVisible(t *testing.T) {
	m := NewModel()
	for i := 0; i < 100; i++ {
		m.Rows = append(m.Rows, session.Session{Key: session.Key{Provider: session.ProviderCodex, ID: fmt.Sprint(i)}, Name: fmt.Sprintf("session-%03d", i), Activity: session.ActivityIdle, UpdatedAt: time.Now()})
	}
	m.Selected = 80
	view := m.View(80, 16)
	if lines := strings.Count(view, "\n"); lines != 16 {
		t.Fatalf("lines=%d", lines)
	}
	if !strings.Contains(view, "> codex  session-080") {
		t.Fatalf("selected row is outside viewport:\n%s", view)
	}
	if strings.Contains(view, "session-000") {
		t.Fatal("viewport rendered off-screen rows")
	}
}

func TestTerminalFrameUsesCarriageReturnAndOneScreenClear(t *testing.T) {
	frame := terminalFrame("one\ntwo\r\nthree\n")
	if !strings.HasPrefix(frame, "\x1b[2J\x1b[H") {
		t.Fatalf("prefix=%q", frame)
	}
	if strings.Contains(strings.ReplaceAll(frame, "\r\n", ""), "\n") {
		t.Fatalf("bare LF in raw-terminal frame: %q", frame)
	}
	if strings.Contains(frame, "\r\r\n") {
		t.Fatalf("existing CRLF was converted twice: %q", frame)
	}
}

func TestReadKeyLeavesBatchedKeyAfterShiftTab(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("\x1b[Z\r"))
	first, err := readKey(r)
	if err != nil || first != "shift+tab" {
		t.Fatalf("first=%q err=%v", first, err)
	}
	second, err := readKey(r)
	if err != nil || second != "enter" {
		t.Fatalf("second=%q err=%v", second, err)
	}
}
func TestEnterDispatchesComposerOrAttachesSelection(t *testing.T) {
	m := NewModel()
	m.Prompt = "fix tests"
	a := m.Update("enter")
	if a.Kind != ActionDispatch || a.Prompt != "fix tests" {
		t.Fatalf("action=%+v", a)
	}
	m.Prompt = ""
	m.Rows = []session.Session{{Key: session.Key{Provider: session.ProviderCodex, ID: "c"}, Capabilities: session.Capabilities{Attach: true}}}
	a = m.Update("enter")
	if a.Kind != ActionAttach || a.Session.Key.ID != "c" {
		t.Fatalf("action=%+v", a)
	}
}

func TestCodexAgentsKeyTransitions(t *testing.T) {
	m := NewModel()
	m.Rows = []session.Session{{Key: session.Key{Provider: session.ProviderCodex, ID: "c"}, Name: "old", Capabilities: session.Capabilities{Attach: true, Stop: true, Rename: true}}}
	if a := m.Update("open"); a.Kind != ActionAttach {
		t.Fatalf("open action=%+v", a)
	}
	if a := m.Update("stop-or-archive"); a.Kind != ActionStop {
		t.Fatalf("stop action=%+v", a)
	}
	if a := m.Update("folders"); a.Kind != ActionRefresh || !m.AllDirectories {
		t.Fatalf("folders action=%+v model=%+v", a, m)
	}
	if a := m.Update("rename"); a.Kind != ActionNone || !m.Renaming || m.RenameDraft != "old" {
		t.Fatalf("rename start action=%+v model=%+v", a, m)
	}
	m.Update("backspace")
	if a := m.Update("enter"); a.Kind != ActionRename || a.Prompt != "ol" {
		t.Fatalf("rename commit action=%+v", a)
	}
}

func TestCtrlXRequiresConfirmationBeforeArchive(t *testing.T) {
	m := NewModel()
	m.Rows = []session.Session{{Key: session.Key{Provider: session.ProviderClaude, ID: "done"}, Capabilities: session.Capabilities{Archive: true}}}
	if a := m.Update("stop-or-archive"); a.Kind != ActionNone || !strings.Contains(m.Status, "again") {
		t.Fatalf("first action=%+v status=%q", a, m.Status)
	}
	if a := m.Update("stop-or-archive"); a.Kind != ActionArchive {
		t.Fatalf("action=%+v", a)
	}
}

func TestUnsupportedActionExplainsWhy(t *testing.T) {
	m := NewModel()
	m.Rows = []session.Session{{Key: session.Key{Provider: session.ProviderCodex, ID: "external"}, Capabilities: session.Capabilities{Reason: "external writer"}}}
	if a := m.Update("open"); a.Kind != ActionNone || !strings.Contains(m.Status, "external writer") {
		t.Fatalf("action=%+v status=%q", a, m.Status)
	}
}

func TestReadKeyConsumesLegacyAndUnknownSequencesAtomically(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"\x1b[Z", "shift+tab"},
		{"\x0f", "open"},
		{"\x01", "folders"},
		{"\x18", "stop-or-archive"},
		{"\x1b[55;5uX", ""},
		{"\x1b[999~X", ""},
	}
	for _, tc := range cases {
		r := bufio.NewReader(strings.NewReader(tc.input))
		got, err := readKey(r)
		if err != nil || got != tc.want {
			t.Fatalf("input=%q got=%q err=%v", tc.input, got, err)
		}
		if strings.HasSuffix(tc.input, "X") {
			next, err := readKey(r)
			if err != nil || next != "X" {
				t.Fatalf("input=%q trailing key=%q err=%v", tc.input, next, err)
			}
		}
	}
}

func TestInputBytesDriveActionsWithoutLeakingProtocolText(t *testing.T) {
	m := NewModel()
	m.Rows = []session.Session{{
		Key:          session.Key{Provider: session.ProviderCodex, ID: "fixture"},
		Capabilities: session.Capabilities{Attach: true, Stop: true},
	}}
	for _, tc := range []struct {
		input string
		want  ActionKind
	}{
		{"\x0f", ActionAttach},
		{"\x18", ActionStop},
		{"\x0c", ActionRefresh},
	} {
		key, err := readKey(bufio.NewReader(strings.NewReader(tc.input)))
		if err != nil {
			t.Fatal(err)
		}
		if action := m.Update(key); action.Kind != tc.want {
			t.Fatalf("input=%q action=%+v", tc.input, action)
		}
	}

	for _, input := range []string{"\x16", "\x1b[55;5u", "\x1b[999~"} {
		key, err := readKey(bufio.NewReader(strings.NewReader(input)))
		if err != nil {
			t.Fatal(err)
		}
		before := m.Prompt
		if action := m.Update(key); action.Kind != ActionNone || m.Prompt != before || m.AllDirectories {
			t.Fatalf("input=%q key=%q action=%+v model=%+v", input, key, action, m)
		}
	}
}

func TestTerminalLifecycleUsesNoEnhancedKeyboardProtocol(t *testing.T) {
	var output bytes.Buffer
	beginTerminal(&output)
	endTerminal(&output)
	got := output.String()
	if strings.Contains(got, "[>1u") || strings.Contains(got, "[<u") {
		t.Fatalf("enhanced keyboard protocol present: %q", got)
	}
	for _, sequence := range []string{"\x1b[?1049h", "\x1b[?25l", "\x1b[?25h", "\x1b[?1049l"} {
		if strings.Count(got, sequence) != 1 {
			t.Fatalf("terminal sequence %q count=%d", sequence, strings.Count(got, sequence))
		}
	}
}

func TestRunRestoresTerminalOnErrorAndPanic(t *testing.T) {
	for _, tc := range []struct {
		name  string
		read  func(*bufio.Reader) (string, error)
		panic bool
	}{
		{name: "error", read: func(*bufio.Reader) (string, error) { return "", errors.New("fixture read failure") }},
		{name: "panic", panic: true, read: func(*bufio.Reader) (string, error) { panic("fixture panic") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			master, slave, err := pty.Open()
			if err != nil {
				t.Fatal(err)
			}
			defer master.Close()
			defer slave.Close()
			before, err := term.GetState(int(slave.Fd()))
			if err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			app := App{Input: slave, Output: &output, CWD: "/work", Model: NewModel(), ReadInput: tc.read}
			func() {
				if tc.panic {
					defer func() {
						if recover() == nil {
							t.Fatal("expected panic")
						}
					}()
				}
				_ = app.Run(context.Background())
			}()
			after, err := term.GetState(int(slave.Fd()))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("terminal state changed: before=%#v after=%#v", before, after)
			}
			if !strings.Contains(output.String(), "\x1b[?25h\x1b[?1049l") {
				t.Fatalf("terminal UI was not restored: %q", output.String())
			}
		})
	}
}

func TestViewKeepsFooterComposerSelectionAndErrorWithinNarrowViewport(t *testing.T) {
	for _, count := range []int{0, 1, 100} {
		m := NewModel()
		for i := 0; i < count; i++ {
			m.Rows = append(m.Rows, session.Session{Key: session.Key{Provider: session.ProviderCodex, ID: fmt.Sprint(i)}, Name: fmt.Sprintf("session-%03d", i), Activity: session.ActivityIdle})
		}
		if count > 0 {
			m.Selected = count - 1
		}
		m.Status = "fixture error"
		view := m.View(48, 10)
		if lines := strings.Count(view, "\n"); lines != 10 {
			t.Fatalf("count=%d lines=%d\n%s", count, lines, view)
		}
		for _, text := range []string{"claude >", "Ctrl+X", "fixture error"} {
			if !strings.Contains(view, text) {
				t.Fatalf("count=%d missing %q\n%s", count, text, view)
			}
		}
		if count > 0 && !strings.Contains(view, "> codex") {
			t.Fatalf("count=%d selected row not visible\n%s", count, view)
		}
	}
}
