package pty

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestRenameClaudeSendsExpectedInputAndDetachesCleanly fixes the mechanical
// contract of RenameClaude against a fake `claude attach <id>` client: the
// exact "/rename <name>\r" bytes must reach the child, and once the fake
// client consumes the same detach byte (0x1a) AttachClaude's real detach
// path relies on, RenameClaude must return with no error -- proving the
// attach client end (not a hang, not a forced kill) is what ends this.
func TestRenameClaudeSendsExpectedInputAndDetachesCleanly(t *testing.T) {
	dir := t.TempDir()
	outPath := filepath.Join(dir, "got")
	name := "Test Rename"
	command := "/rename " + name + "\r"
	script := writeFakeClaudeAttachScript(t, fmt.Sprintf(
		`stty raw -echo; dd bs=1 count=%d of="%s" 2>/dev/null; dd bs=1 count=1 of=/dev/null 2>/dev/null; exit 0`,
		len(command), outPath))

	done := make(chan error, 1)
	go func() { done <- RenameClaude(context.Background(), script, "id", name, time.Second) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RenameClaude err=%v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("RenameClaude did not return")
	}

	got, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != command {
		t.Fatalf("child received %q, want %q", got, command)
	}
}

// TestRenameClaudeRejectsControlCharacterWithoutStartingChild fixes the PTY
// command-injection defense at the transport boundary itself, not just
// Provider.Rename's own check: a name containing a raw control character
// (here CR, which -- reproduced against the installed CLI -- submits
// `/rename` early and turns the remainder into a brand-new prompt the live
// agent actually executes) must be rejected before RenameClaude ever starts
// the attach client, so no command separator can reach a real PTY at all.
// The fake client marks a file the instant it starts; that file must never
// appear.
func TestRenameClaudeRejectsControlCharacterWithoutStartingChild(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "started")
	script := writeFakeClaudeAttachScript(t, fmt.Sprintf(`: > "%s"; stty raw -echo; exit 0`, marker))

	err := RenameClaude(context.Background(), script, "id", "evil\rhi there", time.Second)
	if err == nil {
		t.Fatal("a name containing a raw CR was accepted")
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatal("rejected name still started the attach client -- a command separator could have reached a real PTY")
	}
}

// TestRenameClaudeReturnsErrorOnEarlyClientExit fixes that an attach client
// exiting before RenameClaude can send `/rename` at all is surfaced as an
// error, never silently treated as a successful rename.
func TestRenameClaudeReturnsErrorOnEarlyClientExit(t *testing.T) {
	script := writeFakeClaudeAttachScript(t, "exit 1")
	done := make(chan error, 1)
	go func() { done <- RenameClaude(context.Background(), script, "id", "Some Name", time.Second) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected an error when the attach client exits before rename can be sent")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("RenameClaude did not return after an early client exit")
	}
}

// TestValidateRenameNameRejectsControlCharactersAndBlank and
// TestValidateRenameNameAllowsUnicode exercise validateRenameName directly:
// it is the last gate before RenameClaude assembles the literal pty input,
// independent of whatever validation a caller already did.
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
