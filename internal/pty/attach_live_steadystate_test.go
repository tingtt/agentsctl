//go:build darwin || linux

package pty

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	creackpty "github.com/creack/pty"
)

// TestRealClaudeCtrlBracketDetachSteadyState complements
// TestRealClaudeCtrlBracketDetachSurvivesEarlyRace: that test sends the
// detach byte as early as possible (racing the client's own raw-mode
// setup); this one waits for the client to fully start and settle first,
// exercising the ordinary steady-state path against the installed `claude`
// CLI.
func TestRealClaudeCtrlBracketDetachSteadyState(t *testing.T) {
	if os.Getenv("AGENTSCTL_REAL_CLAUDE_DETACH") != "1" {
		t.Skip("set AGENTSCTL_REAL_CLAUDE_DETACH=1 for the live installed-claude detach steady-state test")
	}
	claudePath, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("claude CLI not installed")
	}

	dispatch := exec.Command(claudePath, "--bg", "Count slowly from 1 to 100, one number per message, waiting between each.")
	out, err := dispatch.CombinedOutput()
	if err != nil {
		t.Fatalf("claude --bg: %v: %s", err, out)
	}
	id := parseBackgroundedID(string(out))
	if id == "" {
		t.Fatalf("could not parse a session id from claude --bg output: %s", out)
	}
	t.Cleanup(func() { _ = exec.Command(claudePath, "rm", id).Run() })

	master, slave, err := creackpty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	defer slave.Close()
	_ = creackpty.Setsize(master, &creackpty.Winsize{Rows: 40, Cols: 120})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var output bytes.Buffer
	attachErr := make(chan error, 1)
	go func() { attachErr <- AttachClaude(ctx, claudePath, id, slave, &output, 3*time.Second) }()

	// Let the client fully start up and settle -- well past any startup race
	// -- before sending the detach key, to test the ordinary steady-state
	// path rather than the earliest-possible-byte race.
	time.Sleep(3 * time.Second)

	if _, err := master.Write([]byte{DetachKey}); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-attachErr:
		if err != nil {
			t.Fatalf("AttachClaude did not detach cleanly: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("AttachClaude did not return within 10s after a steady-state Ctrl+]")
	}

	if strings.Contains(output.String(), "^Z") {
		t.Fatalf("cooked-mode line discipline echoed the detach byte as ^Z instead of delivering it as data:\n%s", output.String())
	}
	// The alternate screen must be closed and the cursor restored: the
	// client exited cleanly rather than being killed out from under its
	// own terminal-mode cleanup.
	if !strings.Contains(output.String(), "\x1b[?1049l") {
		t.Fatalf("client output never left the alternate screen (?1049l) -- did not exit cleanly:\n%s", output.String())
	}

	state, status, err := claudeSessionState(claudePath, id)
	if err != nil {
		t.Fatal(err)
	}
	if state == "stopped" || state == "cancelled" || state == "canceled" {
		t.Fatalf("background session did not survive steady-state detach: state=%q status=%q", state, status)
	}
	t.Logf("background session survived steady-state detach: id=%s state=%q status=%q", id, state, status)
}
