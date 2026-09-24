//go:build darwin || linux

package claude

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// readyNow is a sendClaudeRename readiness func for a session whose worker
// is already live.
func readyNow(context.Context) error { return nil }

// TestSendClaudeRenameSendsExpectedInputWithoutFixedDelay fixes the
// mechanical contract of sendClaudeRename against a fake `claude attach
// <id>` client: once ready has passed (exactly once), the command must
// reach the child as a bracketed paste followed by a CR, and Send must
// return as soon as that write succeeds -- not after some fixed settle
// window (an earlier design waited a fixed 1.5s before writing and 1.2s
// after, which was never load-bearing for correctness).
//
// The framing is the issue #75 regression: Claude 2.1.281 handles an
// unbracketed burst of 64 or more characters as a paste, so a bare
// "/rename <name>\r" with a name like the one below left the command
// unsubmitted in the composer with the CR inserted as a newline. The name
// is deliberately over that threshold and non-ASCII. Whether the real CLI
// actually submits these bytes is covered by the opt-in live test.
func TestSendClaudeRenameSendsExpectedInputWithoutFixedDelay(t *testing.T) {
	dir := t.TempDir()
	outPath := filepath.Join(dir, "got")
	name := "#75 fix(claude): submit /rename correctly for 既存 sessions"
	command := "\x1b[200~/rename " + name + "\x1b[201~\r"
	script := writeFakeClaudeAttachScript(t, fmt.Sprintf(
		`stty raw -echo; dd bs=1 count=%d of="%s" 2>/dev/null; dd bs=1 count=1 of=/dev/null 2>/dev/null; exit 0`,
		len(command), outPath))

	readyCalls := 0
	ready := func(context.Context) error { readyCalls++; return nil }
	start := time.Now()
	cleanup, err := sendClaudeRename(context.Background(), script, "id", name, ready)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("sendClaudeRename err=%v", err)
	}
	if cleanup == nil {
		t.Fatal("sendClaudeRename returned a nil cleanup on success")
	}
	if readyCalls != 1 {
		t.Fatalf("ready was called %d times, want 1", readyCalls)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("sendClaudeRename took %v to return -- a fixed settle delay appears to have been reintroduced", elapsed)
	}

	done := make(chan error, 1)
	go func() { done <- cleanup(context.Background(), time.Second) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("cleanup err=%v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cleanup did not return")
	}

	got, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != command {
		t.Fatalf("child received %q, want %q", got, command)
	}
}

// TestSendClaudeRenameWritesNothingUntilSessionIsReady is the transport
// half of the issue #75 regression: `claude attach` on a stopped session
// respawns its worker, and a "/rename <name>\r" that reaches the worker
// before its REPL is mounted is buffered as composer text with the CR
// turned into a newline -- never submitted. The command must therefore
// not be written until ready passes. When ready fails, Send must report
// the error, write nothing, and still hand back a cleanup for the client
// it already started: the first byte the fake client ever receives must
// be cleanup's detach byte (Ctrl+Z), not any part of the command.
func TestSendClaudeRenameWritesNothingUntilSessionIsReady(t *testing.T) {
	dir := t.TempDir()
	outPath := filepath.Join(dir, "got")
	script := writeFakeClaudeAttachScript(t, fmt.Sprintf(
		`stty raw -echo; dd bs=1 count=1 of="%s" 2>/dev/null; exit 0`, outPath))
	notReady := errors.New("session worker is not live")

	cleanup, err := sendClaudeRename(context.Background(), script, "id", "Some Name", func(context.Context) error { return notReady })
	if !errors.Is(err, notReady) {
		t.Fatalf("sendClaudeRename err=%v, want the readiness error", err)
	}
	if cleanup == nil {
		t.Fatal("a readiness failure after the client started must still return its cleanup")
	}
	done := make(chan error, 1)
	go func() { done <- cleanup(context.Background(), time.Second) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("cleanup err=%v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cleanup did not return")
	}

	got, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "\x1a" {
		t.Fatalf("child's first input byte was %q, want only the detach byte -- the command was written before the session was ready", got)
	}
}

// TestSendClaudeRenameRejectsControlCharacterWithoutStartingChild fixes the
// PTY command-injection defense at the transport boundary itself, not just
// Provider.Rename's own check: a name containing a raw control character
// (here CR, which -- reproduced against the installed CLI -- submits
// `/rename` early and turns the remainder into a brand-new prompt the live
// agent actually executes) must be rejected before sendClaudeRename ever
// starts the attach client, so no command separator can reach a real PTY
// at all. The fake client marks a file the instant it starts; that file
// must never appear, and cleanup must be nil since nothing needs cleaning up.
func TestSendClaudeRenameRejectsControlCharacterWithoutStartingChild(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "started")
	script := writeFakeClaudeAttachScript(t, fmt.Sprintf(`: > "%s"; stty raw -echo; exit 0`, marker))

	ready := func(context.Context) error {
		t.Error("ready was consulted for a rejected name")
		return nil
	}
	cleanup, err := sendClaudeRename(context.Background(), script, "id", "evil\rhi there", ready)
	if err == nil {
		t.Fatal("a name containing a raw CR was accepted")
	}
	if cleanup != nil {
		t.Fatal("a rejected name must not return a cleanup func")
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatal("rejected name still started the attach client -- a command separator could have reached a real PTY")
	}
}

// TestSendClaudeRenameDoesNotHangOrPanicOnEarlyClientExit covers a client
// that exits before (or immediately after) the command can be sent. Send
// deliberately does not try to detect this itself (see its doc comment: a
// bounded wait for it would either be too short to help or add a fixed
// delay to every successful call) -- the guarantee this fixes is narrower:
// Send must return promptly either way (error or a usable cleanup), never
// hang or panic, and if it does return a cleanup, that cleanup must itself
// return promptly rather than getting stuck waiting on a client that's
// already gone. The actual failure surfaces one layer up, in
// Provider.confirmRenamed's native-catalog check.
func TestSendClaudeRenameDoesNotHangOrPanicOnEarlyClientExit(t *testing.T) {
	script := writeFakeClaudeAttachScript(t, "exit 1")
	type result struct {
		cleanup func(context.Context, time.Duration) error
		err     error
	}
	done := make(chan result, 1)
	go func() {
		cleanup, err := sendClaudeRename(context.Background(), script, "id", "Some Name", readyNow)
		done <- result{cleanup, err}
	}()
	var r result
	select {
	case r = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("sendClaudeRename did not return after an early client exit")
	}
	if r.err != nil {
		if r.cleanup != nil {
			t.Fatal("an errored Send must not return a cleanup func")
		}
		return
	}
	if r.cleanup == nil {
		t.Fatal("a non-error Send must return a usable cleanup func")
	}
	cleanupDone := make(chan error, 1)
	go func() { cleanupDone <- r.cleanup(context.Background(), time.Second) }()
	select {
	case <-cleanupDone:
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup did not return for an already-exited client")
	}
}

// TestValidateRenameNameRejectsControlCharactersAndBlank and
// TestValidateRenameNameAllowsUnicode exercise validateRenameName directly:
// it is the last gate before sendClaudeRename assembles the literal pty
// input, independent of whatever validation a caller already did.
func TestValidateRenameNameRejectsControlCharactersAndBlank(t *testing.T) {
	cases := []string{"", "   ", "evil\r", "evil\n", "evil\x1b", "evil\x03", "evil\x7f", "evil\x1a", "evil\x1d"}
	for _, name := range cases {
		if err := validateRenameName(name); err == nil {
			t.Fatalf("name %q was accepted", name)
		}
	}
}
func TestValidateRenameNameAllowsUnicode(t *testing.T) {
	for _, name := range []string{"simple-name", "My Session Name", "日本語 セッション"} {
		if err := validateRenameName(name); err != nil {
			t.Fatalf("name %q was rejected: %v", name, err)
		}
	}
}
