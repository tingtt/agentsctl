//go:build darwin || linux

package agentview

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/tingtt/agentsctl/internal/session"
	"github.com/tingtt/agentsctl/internal/sessionctl"
	"golang.org/x/term"
)

// handoffProvider is a provider-agnostic double: Open runs a hook (the
// "attached child"), Dispatch records prompts. Nothing here knows a real
// provider or supervisor.
type handoffProvider struct {
	*fakeProvider
	mu         sync.Mutex
	dispatched []string
	open       func(in *os.File, out io.Writer) error
}

func (p *handoffProvider) Dispatch(ctx context.Context, prompt, cwd string) (session.Session, error) {
	p.mu.Lock()
	p.dispatched = append(p.dispatched, prompt)
	p.mu.Unlock()
	return session.Session{Key: session.Key{Provider: p.id, ID: "new"}, CWD: cwd}, nil
}

func (p *handoffProvider) Open(_ context.Context, _ session.Session, in *os.File, out io.Writer) error {
	return p.open(in, out)
}

func (p *handoffProvider) dispatches() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.dispatched...)
}

// orderedLifecycle records suspend/resume into a shared event log, so a test
// sees them interleaved with the provider's Open.
type orderedLifecycle struct {
	events     *[]string
	suspendErr error
	resumeErr  error
}

func (l *orderedLifecycle) suspend() error {
	*l.events = append(*l.events, "suspend")
	return l.suspendErr
}
func (l *orderedLifecycle) resume() error {
	*l.events = append(*l.events, "resume")
	return l.resumeErr
}
func (l *orderedLifecycle) close() error { *l.events = append(*l.events, "close"); return nil }

func handoffRuntime(t *testing.T, lifecycle overviewLifecycle, open func(*os.File, io.Writer) error) (*Runtime, session.Session) {
	t.Helper()
	target := session.Session{Key: key("a"), CWD: "/work", Actions: session.Actions{session.ActionOpen: {Available: true}}}
	p := &handoffProvider{fakeProvider: &fakeProvider{id: session.ProviderClaude, rows: []session.Session{target}}, open: open}
	rt := &Runtime{
		Controller: sessionctl.Controller{Providers: []sessionctl.Source{p}, Pins: &fakePins{}},
		State:      NewState(),
		Output:     io.Discard,
		CWD:        "/work",
		terminal:   lifecycle,
	}
	rt.syncReload(context.Background())
	rt.State.selectIndex(0)
	return rt, target
}

func TestOpenSuspendsOverviewAroundProviderOpen(t *testing.T) {
	var events []string
	rt, target := handoffRuntime(t, &orderedLifecycle{events: &events}, func(*os.File, io.Writer) error {
		events = append(events, "open")
		return nil
	})
	if err := rt.act(context.Background(), Intent{Kind: IntentOpen, Key: target.Key}); err != nil {
		t.Fatal(err)
	}
	if want := []string{"suspend", "open", "resume"}; !reflect.DeepEqual(events, want) {
		t.Fatalf("events=%v, want %v", events, want)
	}
	if rt.State.LastAttachedKey != target.Key {
		t.Fatalf("LastAttachedKey=%v, want the opened session", rt.State.LastAttachedKey)
	}
}

func TestOpenFailureStillResumesOverview(t *testing.T) {
	var events []string
	openErr := errors.New("attach refused")
	rt, target := handoffRuntime(t, &orderedLifecycle{events: &events}, func(*os.File, io.Writer) error {
		events = append(events, "open")
		return openErr
	})
	err := rt.act(context.Background(), Intent{Kind: IntentOpen, Key: target.Key})
	if !errors.Is(err, openErr) || errors.Is(err, errOverviewTerminalOwnership) {
		t.Fatalf("err=%v, want the plain Open error", err)
	}
	if want := []string{"suspend", "open", "resume"}; !reflect.DeepEqual(events, want) {
		t.Fatalf("events=%v, want %v", events, want)
	}
	if rt.State.LastAttachedKey == target.Key {
		t.Fatal("a failed Open must not mark the session as attached")
	}
}

func TestOpenDoesNotRunWhenSuspendFails(t *testing.T) {
	var events []string
	suspendErr := errors.New("restore failed")
	rt, target := handoffRuntime(t, &orderedLifecycle{events: &events, suspendErr: suspendErr}, func(*os.File, io.Writer) error {
		events = append(events, "open")
		return nil
	})
	err := rt.act(context.Background(), Intent{Kind: IntentOpen, Key: target.Key})
	if !errors.Is(err, errOverviewTerminalOwnership) || !errors.Is(err, suspendErr) {
		t.Fatalf("err=%v, want terminal-ownership error wrapping the suspend failure", err)
	}
	if want := []string{"suspend"}; !reflect.DeepEqual(events, want) {
		t.Fatalf("events=%v, want Open never reached and no resume of a non-suspended overview", events)
	}
}

func TestOpenResumeFailureKeepsBothErrors(t *testing.T) {
	var events []string
	openErr, resumeErr := errors.New("attach failed"), errors.New("raw mode unavailable")
	rt, target := handoffRuntime(t, &orderedLifecycle{events: &events, resumeErr: resumeErr}, func(*os.File, io.Writer) error {
		return openErr
	})
	err := rt.act(context.Background(), Intent{Kind: IntentOpen, Key: target.Key})
	for _, want := range []error{errOverviewTerminalOwnership, resumeErr, openErr} {
		if !errors.Is(err, want) {
			t.Fatalf("err=%v does not include %v", err, want)
		}
	}
}

func TestEventLoopTreatsOpenResumeFailureAsFatal(t *testing.T) {
	var events []string
	rt, _ := handoffRuntime(t, &orderedLifecycle{events: &events, resumeErr: errors.New("raw mode unavailable")}, func(*os.File, io.Writer) error {
		return nil
	})
	input, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	rt.Input = input
	inputs := make(chan keyResult, 2)
	inputs <- keyResult{ev: keyInput(KeyEvent{Key: KeyEnter})} // empty composer + selection -> Open
	inputs <- keyResult{ev: keyInput(KeyEvent{Key: KeyRune, Rune: '!'})}
	var reads int
	err = rt.eventLoop(context.Background(), bufio.NewReader(input), func(*bufio.Reader) (InputEvent, error) {
		reads++
		r := <-inputs
		return r.ev, r.err
	})
	if !errors.Is(err, errOverviewTerminalOwnership) {
		t.Fatalf("eventLoop err=%v, want terminal-ownership failure", err)
	}
	if reads != 1 {
		t.Fatalf("reads=%d, want no read after a failed resume", reads)
	}
}

// ptyAgentView is a real overview terminal on a PTY driving eventLoop with the
// production input decoder. The "terminal emulator" is the PTY master.
type ptyAgentView struct {
	master, slave *os.File
	out           *syncBuffer
	overview      *overviewTerminal
	originalMode  string
	rt            *Runtime
	provider      *handoffProvider
	done          chan error
}

func startPTYAgentView(t *testing.T, open func(in *os.File, out io.Writer) error) *ptyAgentView {
	t.Helper()
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = master.Close(); _ = slave.Close() })
	if err := pty.Setsize(slave, &pty.Winsize{Rows: 24, Cols: 80}); err != nil {
		t.Fatal(err)
	}
	v := &ptyAgentView{master: master, slave: slave, out: &syncBuffer{}, done: make(chan error, 1)}
	v.originalMode = terminalMode(t, slave)
	v.overview = &overviewTerminal{input: slave, output: v.out}
	if err := v.overview.start(); err != nil {
		t.Fatal(err)
	}
	target := session.Session{Key: key("a"), CWD: "/work", Actions: session.Actions{session.ActionOpen: {Available: true}}}
	v.provider = &handoffProvider{fakeProvider: &fakeProvider{id: session.ProviderClaude, rows: []session.Session{target}}, open: open}
	v.rt = &Runtime{
		Controller: sessionctl.Controller{Providers: []sessionctl.Source{v.provider}, Pins: &fakePins{}},
		State:      NewState(),
		Input:      slave,
		Output:     v.out,
		CWD:        "/work",
		terminal:   v.overview,
	}
	v.rt.syncReload(context.Background())
	v.rt.State.selectIndex(0)
	go func() {
		v.done <- v.rt.eventLoop(context.Background(), bufio.NewReader(slave), func(r *bufio.Reader) (InputEvent, error) {
			return readTerminalInput(r, slave)
		})
	}()
	return v
}

// finish ends the loop by hanging up the terminal and returns its error.
func (v *ptyAgentView) finish(t *testing.T) error {
	t.Helper()
	_ = v.master.Close()
	select {
	case err := <-v.done:
		return err
	case <-time.After(3 * time.Second):
		t.Fatal("event loop did not end after terminal hangup")
		return nil
	}
}

func (v *ptyAgentView) send(t *testing.T, s string) {
	t.Helper()
	if _, err := v.master.WriteString(s); err != nil {
		t.Fatal(err)
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestPTYMultilinePasteInsertsWithoutDispatchThenEnterDispatchesOnce(t *testing.T) {
	v := startPTYAgentView(t, func(*os.File, io.Writer) error { return nil })
	if !strings.Contains(v.out.String(), "\x1b[?2004h") {
		t.Fatalf("overview did not enable bracketed paste: %q", v.out.String())
	}

	// A "terminal emulator" pasting CRLF, LF, and Japanese in one operation,
	// split across writes at an awkward place.
	v.send(t, "\x1b[20")
	v.send(t, "0~first\r\nsecond\n日本語\x1b[201~")
	eventually(t, "pasted text to render", func() bool { return strings.Contains(v.out.String(), "日本語") })
	if got := v.provider.dispatches(); len(got) != 0 {
		t.Fatalf("paste dispatched %q", got)
	}

	v.send(t, "\r")
	eventually(t, "dispatch", func() bool { return len(v.provider.dispatches()) > 0 })
	_ = v.finish(t)
	if got, want := v.provider.dispatches(), []string{"first\nsecond\n日本語"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("dispatches=%q, want exactly %q", got, want)
	}
}

var bracketedPasteToggle = regexp.MustCompile(`\x1b\[\?2004[hl]`)

// TestPTYAgentViewOpenRoundTripRestoresBracketedPaste models Agent View ->
// attached child -> Agent View: the child enables and disables bracketed
// paste on its own (as a real attach client does), and paste still works
// after the overview reacquires the terminal.
func TestPTYAgentViewOpenRoundTripRestoresBracketedPaste(t *testing.T) {
	var (
		v         *ptyAgentView
		childSaw  string
		childMode string
		atOpen    string
	)
	v = startPTYAgentView(t, func(in *os.File, out io.Writer) error {
		childMode = terminalMode(t, in)
		atOpen = v.out.String()
		// The child owns the terminal: it establishes its own paste mode, as
		// an attach client does, and releases it before returning.
		_, _ = io.WriteString(out, "\x1b[?2004h")
		restore, err := term.MakeRaw(int(in.Fd()))
		if err != nil {
			return err
		}
		defer func() { _ = term.Restore(int(in.Fd()), restore) }()
		buf := make([]byte, 5)
		n, _ := io.ReadFull(in, buf)
		childSaw = string(buf[:n])
		_, _ = io.WriteString(out, "\x1b[?2004l")
		return nil
	})
	toggleCount := func() int { return len(bracketedPasteToggle.FindAllString(v.out.String(), -1)) }

	// Enter with an empty composer opens the selected session.
	v.send(t, "\r")
	eventually(t, "child to establish its paste mode", func() bool { return toggleCount() >= 3 })
	v.send(t, "CHILD")
	eventually(t, "overview to resume", func() bool { return toggleCount() >= 5 })

	// Pasting after returning works.
	v.send(t, "\x1b[200~again\r\n日本語\x1b[201~")
	eventually(t, "second paste to render", func() bool { return strings.Contains(v.out.String(), "日本語") })
	_ = v.finish(t)

	if childSaw != "CHILD" {
		t.Fatalf("child received %q, want CHILD (the overview must not read while the child owns input)", childSaw)
	}
	if childMode != v.originalMode {
		t.Fatalf("child terminal mode=%q, want the pre-Agent-View mode %q", childMode, v.originalMode)
	}
	if !strings.HasSuffix(atOpen, "\x1b[?2004l\x1b[0m\x1b[?25h\x1b[?1049l") {
		t.Fatalf("terminal at Open=%q, want every overview mode released, bracketed paste first", atOpen)
	}
	if got, want := v.rt.State.Composer.Prompt, "again\n日本語"; got != want {
		t.Fatalf("Prompt=%q, want %q", got, want)
	}
	if got := v.provider.dispatches(); len(got) != 0 {
		t.Fatalf("paste after handoff dispatched %q", got)
	}
	// overview on, overview off (suspend), child on, child off, overview on (resume).
	toggles := strings.Join(bracketedPasteToggle.FindAllString(v.out.String(), -1), "")
	want := "\x1b[?2004h\x1b[?2004l\x1b[?2004h\x1b[?2004l\x1b[?2004h"
	if toggles != want {
		t.Fatalf("bracketed paste toggles=%q, want %q", toggles, want)
	}
}
