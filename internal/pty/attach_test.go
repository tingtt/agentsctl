package pty

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	creackpty "github.com/creack/pty"
	"github.com/tingtt/agentsctl/internal/protocol"
	"github.com/tingtt/agentsctl/internal/supervisor"
)

func TestDetachIsConsumedAndNeverForwarded(t *testing.T) {
	input := bytes.NewReader([]byte{'a', DetachKey, 'b'})
	var sent bytes.Buffer
	detached := false
	err := forwardInput(input, func(b []byte) error { _, e := sent.Write(b); return e }, func() error { detached = true; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if sent.String() != "a" || !detached {
		t.Fatalf("sent=%q detached=%v", sent.String(), detached)
	}
	if bytes.Contains(sent.Bytes(), []byte{DetachKey}) {
		t.Fatal("detach key reached child")
	}
}
func TestInputEOFIsReported(t *testing.T) {
	err := forwardInput(bytes.NewReader(nil), func([]byte) error { return nil }, func() error { return nil })
	if err != io.EOF {
		t.Fatalf("err=%v", err)
	}
}

// TestClaudeDetachUsesControlZWhenClientConsumesIt models the installed
// `claude attach` client: a well-behaved client puts its own PTY into raw
// mode (disabling ISIG so a literal 0x1a byte reaches its read loop instead
// of becoming a kernel-generated SIGTSTP) and exits on Ctrl+Z, mirroring the
// verified real CLI. detachClaudeClient must succeed via that byte alone,
// never reaching for a signal.
func TestClaudeDetachUsesControlZWhenClientConsumesIt(t *testing.T) {
	cmd := exec.Command("sh", "-c", "stty raw -echo; dd bs=1 count=1 of=/dev/null 2>/dev/null; exit 0")
	child, err := creackpty.Start(cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer child.Close()
	go io.Copy(io.Discard, child) // production drains child output the same way; an unread PTY buffer can wedge a client mid-exit
	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()
	time.Sleep(200 * time.Millisecond) // let `stty raw` land before the byte is sent
	if err := detachClaudeClient(context.Background(), cmd, child, wait, time.Second); err != nil {
		t.Fatal(err)
	}
}

// TestClaudeDetachFallsBackToSignalsWhenClientIgnoresControlZ covers a client
// that never consumes the byte (e.g. stuck in a submode that owns input
// itself). detachClaudeClient must still end only the owned client process,
// via SIGHUP/SIGTERM, without hanging.
func TestClaudeDetachFallsBackToSignalsWhenClientIgnoresControlZ(t *testing.T) {
	// stty -isig keeps the kernel from turning the Ctrl+Z byte into a real
	// SIGTSTP job-control stop, so the fallback signal path is what's tested.
	cmd := exec.Command("sh", "-c", "stty -isig; trap 'exit 0' HUP TERM; while :; do sleep 1; done")
	child, err := creackpty.Start(cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer child.Close()
	go io.Copy(io.Discard, child) // production drains child output the same way; an unread PTY buffer can wedge a client mid-exit
	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()
	time.Sleep(200 * time.Millisecond) // let `stty -isig` land before the byte is sent
	if err := detachClaudeClient(context.Background(), cmd, child, wait, time.Second); err != nil {
		t.Fatal(err)
	}
}

// TestStartClaudeAttachRawDeliversByteSentDuringChildStartupWindow proves
// the fix for a real race found against the installed `claude` CLI:
// sending the detach byte (literal Ctrl+Z, 0x1a) immediately after attach
// starts -- before the child has gotten around to putting its own tty
// into raw mode -- used to be intercepted by the kernel's cooked-mode
// line discipline as the VSUSP special character (visibly echoed back as
// "^Z") and turned into SIGTSTP instead of ever reaching the app as
// input, silently swallowing the detach. startClaudeAttachRaw closes that
// window by putting the pty's slave side into raw mode before the child
// process starts. This models that exact window with a child that
// deliberately delays before touching its own terminal at all.
func TestStartClaudeAttachRawDeliversByteSentDuringChildStartupWindow(t *testing.T) {
	dir := t.TempDir()
	outPath := filepath.Join(dir, "got")
	cmd := exec.Command("sh", "-c", "sleep 0.3; dd bs=1 count=1 of='"+outPath+"' 2>/dev/null")
	master, err := startClaudeAttachRaw(cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	// Sent well before the child's 0.3s sleep elapses: squarely inside
	// the window a not-yet-raw pty would still be in cooked mode.
	if _, err := master.Write([]byte{0x1a}); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != 0x1a {
		t.Fatalf("byte sent during the child's startup window was not delivered as literal data: %v", got)
	}
}

func TestCtrlSIsForwardedToAttachedChild(t *testing.T) {
	input := bytes.NewReader([]byte{'a', 0x13, 'b', DetachKey})
	var sent bytes.Buffer
	err := forwardInput(input, func(b []byte) error { _, e := sent.Write(b); return e }, func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(sent.Bytes(), []byte{'a', 0x13, 'b'}) {
		t.Fatalf("sent=%v", sent.Bytes())
	}
}

// fakeSupervisorSocket starts a minimal fake supervisor: it accepts one
// connection, reads the attach Request frame, always answers with an OK
// Response, then hands the connection to handle to drive the rest of the
// exchange (e.g. sending an Exit frame to model a session ending on its
// own, or just blocking to let the caller drive an explicit detach).
func fakeSupervisorSocket(t *testing.T, handle func(conn net.Conn)) string {
	t.Helper()
	// A short-path temp dir, not t.TempDir() (which nests under a long
	// per-test-name directory): unix socket paths are limited to ~104
	// bytes (sockaddr_un) on macOS, and a path built from the full test
	// name here routinely exceeds that.
	dir, err := os.MkdirTemp("/tmp", "pty-sock-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "supervisor.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		kind, _, err := protocol.Read(conn)
		if err != nil || kind != protocol.Request {
			return
		}
		res, _ := json.Marshal(supervisor.Response{OK: true})
		if protocol.Write(conn, protocol.Response, res) != nil {
			return
		}
		handle(conn)
	}()
	return sock
}

// TestAttachCodexExplicitDetachReturnsCleanly models the DetachKey
// (Ctrl+]) path end to end at the AttachCodex level: local input carries
// the detach byte, AttachCodex must intercept it (never forward it as
// session input), send protocol.Detach, and return with no error. Which
// session the overview marks as "last attached" no longer depends on
// this path specifically — see app_unix.go's act()/Model.MarkAttached,
// which fires on any successful attach return — but the underlying
// detach mechanism itself is still exercised here as a regression check.
func TestAttachCodexExplicitDetachReturnsCleanly(t *testing.T) {
	detachSeen := make(chan struct{}, 1)
	sock := fakeSupervisorSocket(t, func(conn net.Conn) {
		for {
			kind, _, err := protocol.Read(conn)
			if err != nil {
				return
			}
			if kind == protocol.Detach {
				detachSeen <- struct{}{}
				return
			}
		}
	})
	master, slave, err := creackpty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	defer slave.Close()

	done := make(chan error, 1)
	go func() {
		done <- AttachCodex(context.Background(), sock, "run1", slave, io.Discard)
	}()
	if _, err := master.Write([]byte{DetachKey}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-detachSeen:
	case <-time.After(3 * time.Second):
		t.Fatal("server never observed a protocol.Detach frame")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("AttachCodex err=%v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("AttachCodex did not return after sending Detach")
	}
}

// TestAttachCodexNaturalExitReturnsCleanly models the session ending on
// its own (the remote Codex run exits, sending protocol.Exit) with no
// local Ctrl+] ever pressed -- this must also return with no error, since
// the App layer treats any error-free attach return as "this session was
// last attached", regardless of how it ended.
func TestAttachCodexNaturalExitReturnsCleanly(t *testing.T) {
	sock := fakeSupervisorSocket(t, func(conn net.Conn) {
		_ = protocol.Write(conn, protocol.Exit, nil)
	})
	master, slave, err := creackpty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	defer slave.Close()

	if err := AttachCodex(context.Background(), sock, "run1", slave, io.Discard); err != nil {
		t.Fatalf("AttachCodex err=%v", err)
	}
}

// TestAttachCodexFailureReturnsError covers an attach-time/runtime error
// (protocol.Failure): the App layer must not mark a session last-attached
// when attach itself failed.
func TestAttachCodexFailureReturnsError(t *testing.T) {
	sock := fakeSupervisorSocket(t, func(conn net.Conn) {
		_ = protocol.Write(conn, protocol.Failure, []byte("boom"))
	})
	master, slave, err := creackpty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	defer slave.Close()

	if err := AttachCodex(context.Background(), sock, "run1", slave, io.Discard); err == nil {
		t.Fatal("expected an error from a protocol.Failure frame")
	}
}

// TestAttachClaudeExplicitDetachReturnsCleanly and
// TestAttachClaudeNaturalExitReturnsCleanly exercise AttachClaude end to
// end against a fake `claude attach` client (a tiny shell script, not the
// installed CLI -- see attach_live_test.go for that live coverage), the
// same way TestClaudeDetachUsesControlZWhenClientConsumesIt exercises
// detachClaudeClient alone.
func TestAttachClaudeExplicitDetachReturnsCleanly(t *testing.T) {
	script := writeFakeClaudeAttachScript(t, "stty raw -echo; dd bs=1 count=1 of=/dev/null 2>/dev/null; exit 0")
	master, slave, err := creackpty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	defer slave.Close()

	done := make(chan error, 1)
	go func() {
		done <- AttachClaude(context.Background(), script, "id", slave, io.Discard, time.Second)
	}()
	time.Sleep(200 * time.Millisecond) // let the fake client's own `stty raw` land
	if _, err := master.Write([]byte{DetachKey}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("AttachClaude err=%v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("AttachClaude did not return after Ctrl+]")
	}
}

func TestAttachClaudeNaturalExitReturnsCleanly(t *testing.T) {
	script := writeFakeClaudeAttachScript(t, "stty raw -echo; exit 0")
	master, slave, err := creackpty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	defer slave.Close()

	if err := AttachClaude(context.Background(), script, "id", slave, io.Discard, time.Second); err != nil {
		t.Fatalf("AttachClaude err=%v", err)
	}
}

// writeFakeClaudeAttachScript writes an executable shell script standing
// in for `claude attach <id>` and returns its path, for tests that need
// to drive AttachClaude end to end without the installed CLI.
func writeFakeClaudeAttachScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-claude")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestDetachScannerRecognizesCSIuCtrlBracket covers issue #4: iTerm2 (and
// any other terminal that honors the CSI-u / "Kitty keyboard protocol"
// extension a client like `claude` negotiates for its own key handling,
// which gets proxied through to the real terminal by AttachClaude's output
// copy) sends Ctrl+] as an escape sequence instead of the classic literal
// byte 0x1d. Terminal.app doesn't support the extension and keeps sending
// the literal byte, which is why the same build detached cleanly there but
// silently did nothing in iTerm2. A scanner that only recognized the
// literal byte would forward these sequences straight into the child as
// ordinary, unbound, silently-ignored input.
func TestDetachScannerRecognizesCSIuCtrlBracket(t *testing.T) {
	cases := []struct {
		name   string
		chunks [][]byte
		detach bool
	}{
		{"literal byte, one chunk", [][]byte{{'a', DetachKey, 'b'}}, true},
		{"CSI-u plain modifier form, one chunk", [][]byte{[]byte("hi\x1b[93;5u")}, true},
		{"CSI-u event-typed modifier form", [][]byte{[]byte("\x1b[93;5:1u")}, true},
		{"CSI-u with alternate-key-codes prefix", [][]byte{[]byte("\x1b[93:125;5u")}, true},
		{"CSI-u split across two reads", [][]byte{[]byte("hi\x1b[93;"), []byte("5u")}, true},
		{"CSI-u split mid keycode", [][]byte{[]byte("\x1b[9"), []byte("3;5u")}, true},
		{"CSI-u without Ctrl held (shift only) is not a match", [][]byte{[]byte("\x1b[93;2u")}, false},
		{"CSI-u for a different key (']' vs 'a'=97) is not a match", [][]byte{[]byte("\x1b[97;5u")}, false},
		{"CSI-u with no modifier field is not a match", [][]byte{[]byte("\x1b[93u")}, false},
		{"unrelated CSI sequence (e.g. an arrow key) passes through", [][]byte{[]byte("\x1b[A")}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var scanner detachScanner
			var delivered []byte
			detach := false
			for _, chunk := range tc.chunks {
				before, found := scanner.feed(chunk)
				delivered = append(delivered, before...)
				if found {
					detach = true
				}
			}
			if detach != tc.detach {
				t.Fatalf("detach=%v, want %v (delivered=%q, pending=%q)", detach, tc.detach, delivered, scanner.pending)
			}
			if tc.detach && bytes.Contains(delivered, []byte{DetachKey}) {
				t.Fatal("detach key reached the delivered/forwarded bytes")
			}
		})
	}
}

// TestAttachClaudeDetachesOnCSIuCtrlBracket drives AttachClaude end to end
// (fake client, real PTY) with the outer input sending Ctrl+] in the CSI-u
// form iTerm2 uses instead of the classic literal byte, proving the fix
// for issue #4 at the same level TestAttachClaudeExplicitDetachReturnsCleanly
// already covers for the classic byte.
func TestAttachClaudeDetachesOnCSIuCtrlBracket(t *testing.T) {
	script := writeFakeClaudeAttachScript(t, "stty raw -echo; dd bs=1 count=1 of=/dev/null 2>/dev/null; exit 0")
	master, slave, err := creackpty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	defer slave.Close()

	done := make(chan error, 1)
	go func() {
		done <- AttachClaude(context.Background(), script, "id", slave, io.Discard, time.Second)
	}()
	time.Sleep(200 * time.Millisecond) // let the fake client's own `stty raw` land
	if _, err := master.Write([]byte("\x1b[93;5u")); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("AttachClaude err=%v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("AttachClaude did not return after a CSI-u encoded Ctrl+]")
	}
}

func TestInitialTerminalSizeIsARedrawResizeFrame(t *testing.T) {
	master, slave, err := creackpty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	defer slave.Close()
	if err := creackpty.Setsize(slave, &creackpty.Winsize{Rows: 24, Cols: 80}); err != nil {
		t.Fatal(err)
	}
	var frames bytes.Buffer
	stop := resizeTerminal(slave, &lockedFrames{w: &frames})
	stop()
	kind, payload, err := protocol.Read(&frames)
	if err != nil {
		t.Fatal(err)
	}
	var size protocol.TerminalSize
	if err := json.Unmarshal(payload, &size); err != nil {
		t.Fatal(err)
	}
	if kind != protocol.Resize || !size.Redraw || size.Rows != 24 || size.Cols != 80 {
		t.Fatalf("kind=%q size=%+v", kind, size)
	}
	if kind == protocol.Input {
		t.Fatal("terminal size sync was encoded as child input")
	}
}
