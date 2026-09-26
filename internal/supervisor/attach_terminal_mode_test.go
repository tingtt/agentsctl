//go:build darwin || linux

// These tests pin the outer-terminal mode ownership of Client.Attach. The
// managed process establishes terminal modes at startup, long before an
// attach subscriber may exist, and that output is never replayed. An attach
// therefore establishes its own alternate screen and bracketed-paste mode,
// and normalizes only a child alternate-screen leave at the physical-terminal
// boundary so the user's main screen remains untouched.
package supervisor

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	creackpty "github.com/creack/pty"
	"github.com/tingtt/agentsctl/internal/localstate"
	"github.com/tingtt/agentsctl/internal/supervisor/protocol"
	"github.com/tingtt/agentsctl/internal/terminal"
	"golang.org/x/term"
)

// terminalModel is the outer terminal reduced to the behaviors these tests
// need: bracketed paste and main/alternate screen selection. It deliberately
// recognizes only the DECSET/DECRST sequences Attach owns; it is not a general
// terminal emulator.
type terminalModel struct {
	mu                     sync.Mutex
	output                 bytes.Buffer
	main                   bytes.Buffer
	alternate              bytes.Buffer
	screenPending          []byte
	alternateActive        bool
	on                     bool
	failOn                 string // when set, a Write of exactly this string fails
	beforeWrite            func([]byte)
	forwardingStopped      *atomic.Bool
	cleanupBeforePumpStops bool
}

func (m *terminalModel) Write(b []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failOn != "" && string(b) == m.failOn {
		return 0, errors.New("terminal write failed")
	}
	if m.beforeWrite != nil {
		m.beforeWrite(b)
	}
	if m.forwardingStopped != nil && (string(b) == bracketedPasteDisable || string(b) == alternateScreenDisable) && !m.forwardingStopped.Load() {
		m.cleanupBeforePumpStops = true
	}
	m.output.Write(b)
	on := bytes.LastIndex(b, []byte(bracketedPasteEnable))
	off := bytes.LastIndex(b, []byte(bracketedPasteDisable))
	if on >= 0 || off >= 0 {
		m.on = on > off
	}
	m.applyScreen(b)
	return len(b), nil
}

func (m *terminalModel) applyScreen(chunk []byte) {
	data := append(m.screenPending, chunk...)
	m.screenPending = m.screenPending[:0]
	for len(data) > 0 {
		index, enter, length := nextScreenMode(data)
		if index >= 0 {
			m.writeScreenText(data[:index])
			if enter {
				m.alternate.Reset()
				m.alternateActive = true
			} else {
				m.alternateActive = false
			}
			data = data[index+length:]
			continue
		}
		keep := longestScreenModePrefix(data)
		m.writeScreenText(data[:len(data)-keep])
		m.screenPending = append(m.screenPending, data[len(data)-keep:]...)
		return
	}
}

func nextScreenMode(data []byte) (index int, enter bool, length int) {
	index = -1
	for _, candidate := range []struct {
		sequence string
		enter    bool
	}{{alternateScreenEnable, true}, {alternateScreenDisable, false}} {
		found := bytes.Index(data, []byte(candidate.sequence))
		if found >= 0 && (index < 0 || found < index) {
			index, enter, length = found, candidate.enter, len(candidate.sequence)
		}
	}
	return index, enter, length
}

func longestScreenModePrefix(data []byte) int {
	longest := 0
	for _, sequence := range []string{alternateScreenEnable, alternateScreenDisable} {
		for n := 1; n < len(sequence) && n <= len(data); n++ {
			if n > longest && bytes.Equal(data[len(data)-n:], []byte(sequence[:n])) {
				longest = n
			}
		}
	}
	return longest
}

func (m *terminalModel) writeScreenText(b []byte) {
	if m.alternateActive {
		m.alternate.Write(b)
		return
	}
	m.main.Write(b)
}

func (m *terminalModel) written() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.output.String()
}

func (m *terminalModel) bracketedPasteOn() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.on
}

func (m *terminalModel) screenState() (main, alternate string, alternateActive bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.main.String(), m.alternate.String(), m.alternateActive
}

// pasteBytes is what the terminal sends for a paste of text right now.
func (m *terminalModel) pasteBytes(text string) []byte {
	if m.bracketedPasteOn() {
		return []byte("\x1b[200~" + text + "\x1b[201~")
	}
	return []byte(text)
}

func newTerminalPair(t *testing.T) (master, slave *os.File) {
	t.Helper()
	master, slave, err := creackpty.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = master.Close(); _ = slave.Close() })
	return master, slave
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func waitAttach(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("Attach did not return")
		return nil
	}
}

func TestClientAttachOwnsTerminalModesBeforeAnyOutputAndReleasesThemAfterLast(t *testing.T) {
	cases := map[string]struct {
		server  func(conn net.Conn)
		wantErr string // substring; empty means a clean return
	}{
		"remote exit": {server: func(conn net.Conn) {
			_ = protocol.Write(conn, protocol.Output, []byte("hello"))
			_ = protocol.Write(conn, protocol.Exit, nil)
		}},
		"failure frame": {server: func(conn net.Conn) {
			_ = protocol.Write(conn, protocol.Output, []byte("hello"))
			_ = protocol.Write(conn, protocol.Failure, []byte("consumer was not keeping up"))
		}, wantErr: "consumer was not keeping up"},
		"socket closed": {server: func(conn net.Conn) {
			_ = protocol.Write(conn, protocol.Output, []byte("hello"))
		}, wantErr: "attach connection closed unexpectedly"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, slave := newTerminalPair(t)
			term := &terminalModel{}
			done := make(chan error, 1)
			go func() {
				done <- (Client{Socket: fakeSupervisorSocket(t, tc.server)}).Attach(context.Background(), "run1", slave, term)
			}()
			err := waitAttach(t, done)
			if tc.wantErr == "" && err != nil || tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("err=%v, want %q", err, tc.wantErr)
			}
			if want := alternateScreenEnable + bracketedPasteEnable + "hello" + bracketedPasteDisable + alternateScreenDisable; term.written() != want {
				t.Fatalf("terminal output=%q, want %q", term.written(), want)
			}
			if term.bracketedPasteOn() {
				t.Fatal("bracketed paste left on after Attach returned the terminal")
			}
		})
	}
}

func TestClientAttachReleasesBracketedPasteOnDetach(t *testing.T) {
	master, slave := newTerminalPair(t)
	term := &terminalModel{}
	sock := fakeSupervisorSocket(t, func(conn net.Conn) {
		for {
			if kind, _, err := protocol.Read(conn); err != nil || kind == protocol.Detach {
				return
			}
		}
	})
	done := make(chan error, 1)
	go func() { done <- (Client{Socket: sock}).Attach(context.Background(), "run1", slave, term) }()
	waitUntil(t, "bracketed paste to be established", term.bracketedPasteOn)
	if _, err := master.Write([]byte{terminal.DetachKey}); err != nil {
		t.Fatal(err)
	}
	if err := waitAttach(t, done); err != nil {
		t.Fatalf("Attach err=%v", err)
	}
	if want := alternateScreenEnable + bracketedPasteEnable + bracketedPasteDisable + alternateScreenDisable; term.written() != want {
		t.Fatalf("terminal output=%q, want %q", term.written(), want)
	}
}

func TestClientAttachReleasesBracketedPasteOnContextCancel(t *testing.T) {
	_, slave := newTerminalPair(t)
	term := &terminalModel{}
	sock := fakeSupervisorSocket(t, func(conn net.Conn) {
		for { // hold the connection open until the client closes it
			if _, _, err := protocol.Read(conn); err != nil {
				return
			}
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- (Client{Socket: sock}).Attach(ctx, "run1", slave, term) }()
	waitUntil(t, "bracketed paste to be established", term.bracketedPasteOn)
	cancel()
	// Which error a cancellation surfaces as is not this test's concern.
	if err := waitAttach(t, done); err == nil {
		t.Fatal("Attach returned no error after its context was canceled")
	}
	if want := alternateScreenEnable + bracketedPasteEnable + bracketedPasteDisable + alternateScreenDisable; term.written() != want {
		t.Fatalf("terminal output=%q, want %q", term.written(), want)
	}
}

// TestClientAttachDoesNotForwardOutputWhenTerminalModesCannotBeEstablished
// pins that a failed enable fails the attach before session output starts.
func TestClientAttachDoesNotForwardOutputWhenTerminalModesCannotBeEstablished(t *testing.T) {
	for _, failOn := range []string{alternateScreenEnable, bracketedPasteEnable} {
		t.Run(strings.TrimSuffix(strings.TrimPrefix(failOn, "\x1b[?"), "h"), func(t *testing.T) {
			_, slave := newTerminalPair(t)
			term := &terminalModel{failOn: failOn}
			sock := fakeSupervisorSocket(t, func(conn net.Conn) {
				_ = protocol.Write(conn, protocol.Output, []byte("hello"))
				_ = protocol.Write(conn, protocol.Exit, nil)
			})
			done := make(chan error, 1)
			go func() { done <- (Client{Socket: sock}).Attach(context.Background(), "run1", slave, term) }()
			if err := waitAttach(t, done); err == nil || !strings.Contains(err.Error(), "terminal write failed") {
				t.Fatalf("Attach err=%v, want the terminal write failure", err)
			}
			if strings.Contains(term.written(), "hello") {
				t.Fatalf("session output reached a terminal that never got its modes: %q", term.written())
			}
		})
	}
}

func TestClientAttachPropagatesFilteredOutputWriteErrorAndStillReleasesModes(t *testing.T) {
	_, slave := newTerminalPair(t)
	term := &terminalModel{failOn: "hello"}
	sock := fakeSupervisorSocket(t, func(conn net.Conn) {
		_ = protocol.Write(conn, protocol.Output, []byte("hello"))
	})
	err := (Client{Socket: sock}).Attach(context.Background(), "run1", slave, term)
	if err == nil || !strings.Contains(err.Error(), "terminal write failed") {
		t.Fatalf("Attach err=%v, want the terminal write failure", err)
	}
	if want := alternateScreenEnable + bracketedPasteEnable + bracketedPasteDisable + alternateScreenDisable; term.written() != want {
		t.Fatalf("terminal output=%q, want cleanup after failed output %q", term.written(), want)
	}
}

// TestClientAttachReestablishesBracketedPasteOnEveryAttach is the reattach
// case: the process is long running and emits nothing about paste mode the
// second time, yet each attach must establish and release the mode itself.
func TestClientAttachReestablishesBracketedPasteOnEveryAttach(t *testing.T) {
	term := &terminalModel{}
	for attach := 1; attach <= 2; attach++ {
		master, slave := newTerminalPair(t)
		sock := fakeSupervisorSocket(t, func(conn net.Conn) {
			for {
				if kind, _, err := protocol.Read(conn); err != nil || kind == protocol.Detach {
					return
				}
			}
		})
		done := make(chan error, 1)
		go func() { done <- (Client{Socket: sock}).Attach(context.Background(), "run1", slave, term) }()
		waitUntil(t, "bracketed paste to be established", term.bracketedPasteOn)
		if _, err := master.Write([]byte{terminal.DetachKey}); err != nil {
			t.Fatal(err)
		}
		if err := waitAttach(t, done); err != nil {
			t.Fatalf("attach %d: err=%v", attach, err)
		}
		if term.bracketedPasteOn() {
			t.Fatalf("attach %d: mode left on after detach", attach)
		}
	}
	want := strings.Repeat(alternateScreenEnable+bracketedPasteEnable+bracketedPasteDisable+alternateScreenDisable, 2)
	if term.written() != want {
		t.Fatalf("terminal output=%q, want %q", term.written(), want)
	}
}

// TestClientAttachKeepsScreenOwnershipAcrossExternalEditorLifecycle models
// the child/editor mode lifecycle while proving the real main screen remains
// selected only outside Attach. Bracketed-paste changes and child screen entry
// remain ordered; the child screen leave is the sole normalized sequence.
func TestClientAttachKeepsScreenOwnershipAcrossExternalEditorLifecycle(t *testing.T) {
	_, slave := newTerminalPair(t)
	term := &terminalModel{}
	_, _ = term.Write([]byte("shell"))
	sock := fakeSupervisorSocket(t, func(conn net.Conn) {
		for _, out := range []string{"Codex output", bracketedPasteDisable, alternateScreenEnable, "editor output", alternateScreenDisable, bracketedPasteEnable, "Codex redraw/output"} {
			_ = protocol.Write(conn, protocol.Output, []byte(out))
		}
		_ = protocol.Write(conn, protocol.Exit, nil)
	})
	done := make(chan error, 1)
	go func() { done <- (Client{Socket: sock}).Attach(context.Background(), "run1", slave, term) }()
	if err := waitAttach(t, done); err != nil {
		t.Fatalf("Attach err=%v", err)
	}
	want := "shell" + alternateScreenEnable + bracketedPasteEnable + "Codex output" + bracketedPasteDisable + alternateScreenEnable + "editor output" + bracketedPasteEnable + "Codex redraw/output" + bracketedPasteDisable + alternateScreenDisable
	if term.written() != want {
		t.Fatalf("terminal output=%q, want %q", term.written(), want)
	}
	main, alternate, active := term.screenState()
	if active || strings.Contains(main, "Codex") || strings.Contains(main, "editor") || !strings.Contains(main, "shell") {
		t.Fatalf("screen state after Attach: main=%q alternate=%q active=%v", main, alternate, active)
	}
	if !strings.Contains(alternate, "editor output") || !strings.Contains(alternate, "Codex redraw/output") {
		t.Fatalf("attach-owned alternate screen did not retain child lifecycle output: %q", alternate)
	}
}

func TestClientAttachStopsForwardingBeforeTerminalModeCleanup(t *testing.T) {
	_, slave := newTerminalPair(t)
	var stopped atomic.Bool
	term := &terminalModel{forwardingStopped: &stopped}
	pump := func(ctx context.Context, _ *os.File, _ *lockedFrames) inputOutcome {
		<-ctx.Done()
		stopped.Store(true)
		return inputOutcome{err: ctx.Err()}
	}
	sock := fakeSupervisorSocket(t, func(conn net.Conn) {
		_ = protocol.Write(conn, protocol.Exit, nil)
	})
	if err := (Client{Socket: sock}).attach(context.Background(), "run1", slave, term, pump); err != nil {
		t.Fatal(err)
	}
	if term.cleanupBeforePumpStops {
		t.Fatal("terminal modes were released before the input pump stopped")
	}
}

func TestClientAttachLeavesAlternateScreenBeforeRestoringTerminalMode(t *testing.T) {
	_, slave := newTerminalPair(t)
	original, err := term.GetState(int(slave.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	var leaveObserved, leaveWhileRaw atomic.Bool
	terminalOutput := &terminalModel{beforeWrite: func(b []byte) {
		if string(b) != alternateScreenDisable {
			return
		}
		leaveObserved.Store(true)
		current, stateErr := term.GetState(int(slave.Fd()))
		leaveWhileRaw.Store(stateErr == nil && !reflect.DeepEqual(current, original))
	}}
	sock := fakeSupervisorSocket(t, func(conn net.Conn) {
		_ = protocol.Write(conn, protocol.Exit, nil)
	})
	if err := (Client{Socket: sock}).Attach(context.Background(), "run1", slave, terminalOutput); err != nil {
		t.Fatal(err)
	}
	if !leaveObserved.Load() || !leaveWhileRaw.Load() {
		t.Fatal("alternate screen was not released while Attach still owned raw mode")
	}
	restored, err := term.GetState(int(slave.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(restored, original) {
		t.Fatal("terminal mode was not restored after alternate-screen cleanup")
	}
}

func TestClientAttachNeverDepositsAFrameOnTheMainScreenAcrossHandoffs(t *testing.T) {
	master, slave := newTerminalPair(t)
	term := &terminalModel{}
	_, _ = term.Write([]byte("shell"))

	sock := fakeSupervisorSocket(t, func(conn net.Conn) {
		_ = protocol.Write(conn, protocol.Output, []byte("Codex A frame"))
		for {
			if kind, _, err := protocol.Read(conn); err != nil || kind == protocol.Detach {
				return
			}
		}
	})
	done := make(chan error, 1)
	go func() { done <- (Client{Socket: sock}).Attach(context.Background(), "run1", slave, term) }()
	waitUntil(t, "Codex A redraw", func() bool {
		_, alternate, active := term.screenState()
		return active && strings.Contains(alternate, "Codex A frame")
	})
	if _, err := master.Write([]byte{terminal.DetachKey}); err != nil {
		t.Fatal(err)
	}
	if err := waitAttach(t, done); err != nil {
		t.Fatal(err)
	}

	// Agent View owns its alternate screen, then releases it for the next
	// foreground provider. That release must expose the original shell, never
	// a frame from the previous Codex attach.
	_, _ = term.Write([]byte(alternateScreenEnable + "Agent View" + alternateScreenDisable))
	main, _, active := term.screenState()
	if active || strings.Contains(main, "Codex A frame") || !strings.Contains(main, "shell") {
		t.Fatalf("next foreground handoff exposed stale content: main=%q active=%v", main, active)
	}
}

// TestClientAttachBracketsTerminalPasteOfAnAlreadyRunningProcess connects
// the two contracts: the attach establishes the mode (the process, already
// running, never emits it), the terminal therefore marks a long multiline
// paste, and the transport delivers marker, payload and marker to the PTY
// unchanged, after which Ctrl+] detaches and the mode is released. Without
// the establish step the terminal never turns bracketing on and the wait
// below times out.
func TestClientAttachBracketsTerminalPasteOfAnAlreadyRunningProcess(t *testing.T) {
	master, slave := newTerminalPair(t)
	term := &terminalModel{}
	w := &recordingWriter{maxWrites: []int{17, 3, 4096}}
	p := &process{run: localstate.Run{ID: "r"}, input: w, subscribers: map[*subscriber]struct{}{}, done: make(chan struct{})}
	sock := serveAttach(t, &Server{runs: map[string]*process{"r": p}}, "r")

	done := make(chan error, 1)
	go func() { done <- (Client{Socket: sock}).Attach(context.Background(), "r", slave, term) }()
	waitUntil(t, "the terminal to have bracketed paste on", term.bracketedPasteOn)

	body := string(pasteStream(20 << 10))
	body = strings.TrimSuffix(strings.TrimPrefix(body, "\x1b[200~"), "\x1b[201~")
	paste := term.pasteBytes(body)
	if !bytes.HasPrefix(paste, []byte("\x1b[200~")) {
		t.Fatal("terminal did not bracket the paste")
	}
	go func() {
		for rest := paste; len(rest) > 0; {
			n := min(len(rest), 1531)
			if _, err := master.Write(rest[:n]); err != nil {
				return
			}
			rest = rest[n:]
		}
		_, _ = master.Write([]byte{terminal.DetachKey})
	}()
	if err := waitAttach(t, done); err != nil {
		t.Fatalf("Attach err=%v", err)
	}
	if !bytes.Equal(w.bytes(), paste) {
		t.Fatalf("PTY received %d bytes, want the %d-byte bracketed paste unchanged", len(w.bytes()), len(paste))
	}
	if term.bracketedPasteOn() || !strings.HasSuffix(term.written(), bracketedPasteDisable+alternateScreenDisable) {
		t.Fatalf("terminal not released: %q", term.written())
	}
}
