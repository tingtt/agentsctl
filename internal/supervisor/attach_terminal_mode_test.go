//go:build darwin || linux

// These tests pin the outer-terminal mode ownership of Client.Attach. The
// managed process enables bracketed paste once, at its own startup, long
// before any attach subscriber exists, and that output is never replayed.
// An attach therefore establishes the mode itself when it takes the
// terminal and releases it before returning it, on every way it can end,
// while everything the process emits during the attach is forwarded
// untouched (issue #29).
package supervisor

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	creackpty "github.com/creack/pty"
	"github.com/tingtt/agentsctl/internal/localstate"
	"github.com/tingtt/agentsctl/internal/supervisor/protocol"
	"github.com/tingtt/agentsctl/internal/terminal"
)

// terminalModel is the outer terminal reduced to the one behavior these
// tests need: it records what it is written, remembers whether bracketed
// paste is on (the last 2004 set/reset it received), and, as a real
// terminal does, marks a paste with bracketed-paste markers only while on.
type terminalModel struct {
	mu     sync.Mutex
	output bytes.Buffer
	on     bool
	failOn string // when set, a Write of exactly this string fails
}

func (m *terminalModel) Write(b []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failOn != "" && string(b) == m.failOn {
		return 0, errors.New("terminal write failed")
	}
	m.output.Write(b)
	on := bytes.LastIndex(b, []byte(bracketedPasteEnable))
	off := bytes.LastIndex(b, []byte(bracketedPasteDisable))
	if on >= 0 || off >= 0 {
		m.on = on > off
	}
	return len(b), nil
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

func TestClientAttachEstablishesBracketedPasteBeforeAnyOutputAndReleasesItAfterLast(t *testing.T) {
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
			if want := bracketedPasteEnable + "hello" + bracketedPasteDisable; term.written() != want {
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
	if want := bracketedPasteEnable + bracketedPasteDisable; term.written() != want {
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
	if want := bracketedPasteEnable + bracketedPasteDisable; term.written() != want {
		t.Fatalf("terminal output=%q, want %q", term.written(), want)
	}
}

// TestClientAttachTakesNoTerminalModeWhenItCannotEstablishOne pins that a
// failed enable fails the attach, and no session output is forwarded.
func TestClientAttachTakesNoTerminalModeWhenItCannotEstablishOne(t *testing.T) {
	_, slave := newTerminalPair(t)
	term := &terminalModel{failOn: bracketedPasteEnable}
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
		t.Fatalf("session output reached a terminal that never got its mode: %q", term.written())
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
	want := strings.Repeat(bracketedPasteEnable+bracketedPasteDisable, 2)
	if term.written() != want {
		t.Fatalf("terminal output=%q, want %q", term.written(), want)
	}
}

// TestClientAttachForwardsTheProcessOwnTerminalModeChanges pins that the
// process stays the owner of its own mode changes during an attach (Codex
// turns bracketed paste off around an external editor and back on): they
// are neither filtered nor reordered relative to other output.
func TestClientAttachForwardsTheProcessOwnTerminalModeChanges(t *testing.T) {
	_, slave := newTerminalPair(t)
	term := &terminalModel{}
	sock := fakeSupervisorSocket(t, func(conn net.Conn) {
		for _, out := range []string{"a", bracketedPasteDisable, "\x1b[?1049h", "b", "\x1b[?1049l", bracketedPasteEnable, "c"} {
			_ = protocol.Write(conn, protocol.Output, []byte(out))
		}
		_ = protocol.Write(conn, protocol.Exit, nil)
	})
	done := make(chan error, 1)
	go func() { done <- (Client{Socket: sock}).Attach(context.Background(), "run1", slave, term) }()
	if err := waitAttach(t, done); err != nil {
		t.Fatalf("Attach err=%v", err)
	}
	want := bracketedPasteEnable + "a" + bracketedPasteDisable + "\x1b[?1049h" + "b" + "\x1b[?1049l" + bracketedPasteEnable + "c" + bracketedPasteDisable
	if term.written() != want {
		t.Fatalf("terminal output=%q, want %q", term.written(), want)
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
	if term.bracketedPasteOn() || !strings.HasSuffix(term.written(), bracketedPasteDisable) {
		t.Fatalf("terminal not released: %q", term.written())
	}
}
