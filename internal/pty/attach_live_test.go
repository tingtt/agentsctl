//go:build darwin || linux

package pty

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	creackpty "github.com/creack/pty"
)

// TestRealClaudeCtrlBracketDetachSurvivesEarlyRace exercises the actual
// installed `claude` CLI (never a fake) end to end: it dispatches a real
// background session, attaches to it via AttachClaude, and sends the
// detach key (Ctrl+]) as early as possible -- racing the client's own
// terminal setup, which is exactly the window in which a real, reproduced
// bug used to swallow the detach (see startClaudeAttachRaw's doc comment
// and TestStartClaudeAttachRawDeliversByteSentDuringChildStartupWindow
// for the mechanism). It asserts AttachClaude returns promptly (the
// client actually exited) and that the background session is still alive
// afterward, i.e. detaching never cancels or kills it.
func TestRealClaudeCtrlBracketDetachSurvivesEarlyRace(t *testing.T) {
	if os.Getenv("AGENTSCTL_REAL_CLAUDE_DETACH") != "1" {
		t.Skip("set AGENTSCTL_REAL_CLAUDE_DETACH=1 for the live installed-claude detach race test")
	}
	claudePath, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("claude CLI not installed")
	}

	dispatch := exec.Command(claudePath, "--bg", "Reply with just the word PONG, nothing else.")
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

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var output bytes.Buffer
	attachErr := make(chan error, 1)
	go func() { attachErr <- AttachClaude(ctx, claudePath, id, slave, &output, 2*time.Second) }()

	// Send the detach key as early as possible: this is the race.
	if _, err := master.Write([]byte{DetachKey}); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-attachErr:
		if err != nil {
			t.Fatalf("AttachClaude did not detach cleanly: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("AttachClaude did not return after an early Ctrl+] -- the race reproduced")
	}

	if strings.Contains(output.String(), "^Z") {
		t.Fatalf("cooked-mode line discipline echoed the detach byte as ^Z instead of delivering it as data:\n%s", output.String())
	}

	state, status, err := claudeSessionState(claudePath, id)
	if err != nil {
		t.Fatal(err)
	}
	if state == "stopped" || state == "cancelled" || state == "canceled" {
		t.Fatalf("background session did not survive detach: state=%q status=%q", state, status)
	}
	t.Logf("background session survived detach: id=%s state=%q status=%q", id, state, status)
}

var ansiEscape = regexp.MustCompile("\x1b\\[[0-9;]*m")

// parseBackgroundedID extracts the session id from `claude --bg`'s
// "backgrounded · <id>" first line (which still carries ANSI color codes
// even without a TTY attached).
func parseBackgroundedID(output string) string {
	for _, line := range strings.Split(output, "\n") {
		plain := ansiEscape.ReplaceAllString(line, "")
		if !strings.Contains(plain, "backgrounded") {
			continue
		}
		fields := strings.Fields(plain)
		if len(fields) == 0 {
			continue
		}
		return fields[len(fields)-1]
	}
	return ""
}

// claudeSessionState looks up one session's native state/status via
// `claude agents --json --all`, the same source of truth
// internal/provider/claude.Provider.List uses.
func claudeSessionState(claudePath, id string) (state, status string, err error) {
	out, err := exec.Command(claudePath, "agents", "--json", "--all").CombinedOutput()
	if err != nil {
		return "", "", err
	}
	var rows []map[string]any
	if err := json.Unmarshal(out, &rows); err != nil {
		return "", "", err
	}
	for _, row := range rows {
		rowID, _ := row["id"].(string)
		if rowID != id {
			continue
		}
		s, _ := row["state"].(string)
		st, _ := row["status"].(string)
		return s, st, nil
	}
	return "", "", errors.New("session not found in claude agents --json --all output")
}
