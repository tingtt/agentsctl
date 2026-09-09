//go:build darwin || linux

package agentview

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/creack/pty"
	"github.com/tingtt/agentsctl/internal/session"
	"github.com/tingtt/agentsctl/internal/sessionctl"
)

type fakeOverviewLifecycle struct {
	events     []string
	suspendErr error
	resumeErr  error
}

func (f *fakeOverviewLifecycle) suspend() error {
	f.events = append(f.events, "suspend")
	return f.suspendErr
}

func (f *fakeOverviewLifecycle) resume() error {
	f.events = append(f.events, "resume")
	return f.resumeErr
}

func (f *fakeOverviewLifecycle) close() error {
	f.events = append(f.events, "close")
	return nil
}

func promptEditorRuntime(t *testing.T, prompt string) (*Runtime, *fakeOverviewLifecycle) {
	t.Helper()
	input, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = input.Close() })
	lifecycle := &fakeOverviewLifecycle{}
	s := NewState()
	s.Composer = Composer{Prompt: prompt, Cursor: len([]rune(prompt)), Stash: "stashed"}
	return &Runtime{State: s, Input: input, Output: io.Discard, terminal: lifecycle}, lifecycle
}

func TestPromptEditorRoundTripsPromptContent(t *testing.T) {
	cases := []struct {
		name     string
		original string
		saved    string
		want     string
	}{
		{name: "empty", original: "", saved: "\n", want: ""},
		{name: "single line", original: "Review this", saved: "Revised\n", want: "Revised"},
		{name: "multiline Japanese", original: "確認する\n小さく直す", saved: "確認済み\nテストする\n", want: "確認済み\nテストする"},
		{name: "one meaningful trailing newline", original: "foo\n", saved: "foo\n\n", want: "foo\n"},
		{name: "two meaningful trailing newlines", original: "foo\n\n", saved: "foo\n\n\n", want: "foo\n\n"},
		{name: "editor CRLF", original: "old", saved: "first\r\nsecond\r\n", want: "first\nsecond"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt, lifecycle := promptEditorRuntime(t, tc.original)
			var path string
			rt.runPromptEditor = func(_ context.Context, candidate string, _ *os.File, _ io.Writer) error {
				path = candidate
				info, err := os.Stat(candidate)
				if err != nil {
					return err
				}
				if got := info.Mode().Perm(); got != 0o600 {
					t.Fatalf("temporary prompt file permissions=%o, want 600", got)
				}
				content, err := os.ReadFile(candidate)
				if err != nil {
					return err
				}
				if got, want := string(content), encodePromptFile(tc.original); got != want {
					t.Fatalf("initial editor file=%q, want prompt-only content %q", got, want)
				}
				return os.WriteFile(candidate, []byte(tc.saved), 0o600)
			}
			if err := rt.editPrompt(context.Background()); err != nil {
				t.Fatal(err)
			}
			if rt.State.Composer.Prompt != tc.want {
				t.Fatalf("Prompt=%q, want %q", rt.State.Composer.Prompt, tc.want)
			}
			if rt.State.Composer.Cursor != len([]rune(tc.want)) {
				t.Fatalf("Cursor=%d, want %d", rt.State.Composer.Cursor, len([]rune(tc.want)))
			}
			if rt.State.Composer.Stash != "stashed" {
				t.Fatalf("Stash=%q, want unchanged", rt.State.Composer.Stash)
			}
			if !reflect.DeepEqual(lifecycle.events, []string{"suspend", "resume"}) {
				t.Fatalf("terminal events=%v", lifecycle.events)
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("temporary prompt file still exists: %v", err)
			}
		})
	}
}

func TestPromptEditorCancelPreservesComposerState(t *testing.T) {
	rt, _ := promptEditorRuntime(t, "original\n日本語")
	rt.State.Composer.Cursor = 3
	rt.State.Composer.preferredColumn = 2
	rt.State.Composer.hasPreferredColumn = true
	rt.runPromptEditor = func(context.Context, string, *os.File, io.Writer) error { return nil }
	before := rt.State.Composer
	if err := rt.editPrompt(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rt.State.Composer, before) {
		t.Fatalf("Composer=%+v, want unchanged %+v", rt.State.Composer, before)
	}
}

func TestPromptEditorFailuresPreserveOriginalPrompt(t *testing.T) {
	cases := []struct {
		name string
		run  promptEditorRunner
	}{
		{
			name: "vim launch or exit failure",
			run: func(context.Context, string, *os.File, io.Writer) error {
				return errors.New("vim failed")
			},
		},
		{
			name: "saved file read failure",
			run: func(_ context.Context, path string, _ *os.File, _ io.Writer) error {
				return os.Remove(path)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt, _ := promptEditorRuntime(t, "keep\n日本語")
			rt.State.Composer.Cursor = 2
			before := rt.State.Composer
			rt.runPromptEditor = tc.run
			if err := rt.editPrompt(context.Background()); err == nil {
				t.Fatal("editPrompt returned nil error")
			}
			if !reflect.DeepEqual(rt.State.Composer, before) {
				t.Fatalf("Composer=%+v, want unchanged %+v", rt.State.Composer, before)
			}
		})
	}
}

func TestPromptEditorFailureUsesAgentViewErrorPath(t *testing.T) {
	rt, _ := promptEditorRuntime(t, "keep me")
	rt.runPromptEditor = func(context.Context, string, *os.File, io.Writer) error {
		return errors.New("cannot start")
	}
	inputs := make(chan keyResult, 2)
	inputs <- keyResult{ev: KeyEvent{Key: KeyCtrlG}}
	inputs <- keyResult{err: io.EOF}
	err := rt.eventLoop(context.Background(), bufio.NewReader(rt.Input), func(*bufio.Reader) (KeyEvent, error) {
		result := <-inputs
		return result.ev, result.err
	})
	if !errors.Is(err, io.EOF) {
		t.Fatalf("eventLoop error=%v, want EOF after displaying the editor error", err)
	}
	if !strings.Contains(rt.State.Error, "vim: cannot start") {
		t.Fatalf("State.Error=%q, want Vim launch failure", rt.State.Error)
	}
	if rt.State.Composer.Prompt != "keep me" {
		t.Fatalf("Prompt=%q, want original after launch failure", rt.State.Composer.Prompt)
	}
}

func TestCtrlGUpdatesComposerWithoutDispatch(t *testing.T) {
	p := &fakeProvider{id: session.ProviderClaude, rows: []session.Session{{Key: key("existing"), CWD: "/work"}}}
	rt, _ := promptEditorRuntime(t, "before")
	rt.Controller = sessionctl.Controller{Providers: []sessionctl.Source{p}, Pins: &fakePins{}}
	rt.runPromptEditor = func(_ context.Context, path string, _ *os.File, _ io.Writer) error {
		return os.WriteFile(path, []byte("after\n"), 0o600)
	}
	intent := rt.State.Handle(KeyEvent{Key: KeyCtrlG})
	if intent.Kind != IntentOpenPromptEditor {
		t.Fatalf("intent=%+v", intent)
	}
	if err := rt.act(context.Background(), intent); err != nil {
		t.Fatal(err)
	}
	if rt.State.Composer.Prompt != "after" {
		t.Fatalf("Prompt=%q, want saved editor content", rt.State.Composer.Prompt)
	}
	if len(p.rows) != 1 {
		t.Fatalf("provider rows=%d, want no dispatch-created session", len(p.rows))
	}
}

func TestRunVimUsesFixedVimCommand(t *testing.T) {
	dir := t.TempDir()
	vim := filepath.Join(dir, "vim")
	if err := os.WriteFile(vim, []byte("#!/bin/sh\nprintf 'saved\\n' > \"$1\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("EDITOR", "must-not-be-used")
	t.Setenv("VISUAL", "must-not-be-used")
	prompt := filepath.Join(t.TempDir(), "prompt.txt")
	if err := os.WriteFile(prompt, []byte("before\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	input, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	if err := runVim(context.Background(), prompt, input, io.Discard); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(prompt)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "saved\n" {
		t.Fatalf("prompt file=%q, want fake vim output", content)
	}
}

func TestEventLoopHandsTerminalToPromptEditorAndAcceptsLaterInput(t *testing.T) {
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	defer slave.Close()
	originalMode := terminalMode(t, slave)
	out := &syncBuffer{}
	lifecycle := &overviewTerminal{input: slave, output: out}
	if err := lifecycle.start(); err != nil {
		t.Fatal(err)
	}

	p := &fakeProvider{id: session.ProviderClaude, rows: []session.Session{{Key: key("existing"), CWD: "/work"}}}
	s := NewState()
	s.Composer.ReplacePrompt("元の文\nsecond")
	rt := &Runtime{
		Controller: sessionctl.Controller{Providers: []sessionctl.Source{p}, Pins: &fakePins{}},
		State:      s,
		Input:      slave,
		Output:     out,
		terminal:   lifecycle,
	}
	var reads atomic.Int32
	inputs := make(chan keyResult, 3)
	readKey := func(*bufio.Reader) (KeyEvent, error) {
		reads.Add(1)
		result := <-inputs
		return result.ev, result.err
	}
	rt.runPromptEditor = func(_ context.Context, path string, input *os.File, output io.Writer) error {
		if got := reads.Load(); got != 1 {
			t.Fatalf("key reads while editor owns terminal=%d, want 1 consumed Ctrl+G read", got)
		}
		if input != slave || output != out {
			t.Fatal("editor did not inherit Agent View terminal streams")
		}
		if got := terminalMode(t, slave); got != originalMode {
			t.Fatalf("editor terminal mode=%q, want pre-Agent-View mode %q", got, originalMode)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if string(content) != "元の文\nsecond\n" {
			t.Fatalf("editor received %q", content)
		}
		return os.WriteFile(path, []byte("保存済み\nnext\n"), 0o600)
	}
	inputs <- keyResult{ev: KeyEvent{Key: KeyCtrlG}}
	inputs <- keyResult{ev: KeyEvent{Key: KeyRune, Rune: '!'}}
	inputs <- keyResult{err: io.EOF}
	if err := rt.eventLoop(context.Background(), bufio.NewReader(slave), readKey); !errors.Is(err, io.EOF) {
		t.Fatalf("eventLoop error=%v, want EOF after verification input", err)
	}
	if rt.State.Composer.Prompt != "保存済み\nnext!" {
		t.Fatalf("Prompt=%q, want saved text plus subsequent Agent View input", rt.State.Composer.Prompt)
	}
	if len(p.rows) != 1 {
		t.Fatalf("provider rows=%d, want no editor-triggered dispatch", len(p.rows))
	}
	if got := strings.Count(out.String(), "\x1b[2J\x1b[H"); got < 2 {
		t.Fatalf("full redraws=%d, want redraw after editor and subsequent input", got)
	}
	if err := lifecycle.close(); err != nil {
		t.Fatal(err)
	}
	if got := terminalMode(t, slave); got != originalMode {
		t.Fatalf("final terminal mode=%q, want original %q", got, originalMode)
	}

	sequence := out.String()
	controls := []string{
		"\x1b[?1049h\x1b[?25l",
		"\x1b[0m\x1b[?25h\x1b[?1049l",
		"\x1b[?1049h\x1b[?25l",
		"\x1b[0m\x1b[?25h\x1b[?1049l",
	}
	for _, control := range controls {
		index := strings.Index(sequence, control)
		if index < 0 {
			t.Fatalf("terminal output missing ordered control sequence %q", control)
		}
		sequence = sequence[index+len(control):]
	}
}

func TestEventLoopStopsReadingWhenTerminalResumeFails(t *testing.T) {
	rt, lifecycle := promptEditorRuntime(t, "original")
	lifecycle.resumeErr = errors.New("raw mode unavailable")
	rt.runPromptEditor = func(_ context.Context, path string, _ *os.File, _ io.Writer) error {
		return os.WriteFile(path, []byte("saved\n"), 0o600)
	}
	inputs := make(chan keyResult, 2)
	inputs <- keyResult{ev: KeyEvent{Key: KeyCtrlG}}
	inputs <- keyResult{ev: KeyEvent{Key: KeyRune, Rune: '!'}}
	var reads atomic.Int32
	err := rt.eventLoop(context.Background(), bufio.NewReader(rt.Input), func(*bufio.Reader) (KeyEvent, error) {
		reads.Add(1)
		result := <-inputs
		return result.ev, result.err
	})
	if !errors.Is(err, errOverviewTerminalOwnership) {
		t.Fatalf("eventLoop error=%v, want terminal-ownership failure", err)
	}
	if got := reads.Load(); got != 1 {
		t.Fatalf("key reads=%d, want no read after failed resume", got)
	}
	if rt.State.Composer.Prompt != "original" {
		t.Fatalf("Prompt=%q, want original after failed resume", rt.State.Composer.Prompt)
	}
}
