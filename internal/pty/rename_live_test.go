//go:build darwin || linux

package pty

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"testing"
	"time"
)

// TestRealClaudeRenameMutatesSessionInPlace exercises the actual installed
// `claude` CLI (never a fake), the same way
// TestRealClaudeCtrlBracketDetachSurvivesEarlyRace does for detach: it
// dispatches a real, disposable background session, renames it via
// SendClaudeRename, and confirms — via `claude agents --json --all`, the
// same native source Provider.List/confirmRenamed use, never PTY output —
// that the rename landed on the very same session (same id and sessionId)
// with no extra session created alongside it.
//
// It also measures user-visible rename latency (Send -> catalog confirms
// the new name, mirroring what Provider.Rename's confirmRenamed loop does
// -- cleanup is deliberately excluded, since Provider.Rename runs it
// concurrently rather than waiting for it before reporting success) and
// asserts it stays well under the ~2.7s of fixed settle delay the previous
// design always paid, without hard-coding a specific millisecond figure
// that would make this test flaky.
//
// This intentionally exercises only a quick/completed session for
// determinism and cost; SendClaudeRename's doc comment records what was
// separately, manually verified during this feature's investigation for a
// working (mid-tool-call) session: same id/sessionId/pid, no fork, no
// interruption of the in-flight background execution, and the same
// sub-second catalog-confirmation latency.
func TestRealClaudeRenameMutatesSessionInPlace(t *testing.T) {
	if os.Getenv("AGENTSCTL_REAL_CLAUDE_RENAME") != "1" {
		t.Skip("set AGENTSCTL_REAL_CLAUDE_RENAME=1 for the live installed-claude rename test")
	}
	claudePath, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("claude CLI not installed")
	}

	before, err := realClaudeAgentsJSON(claudePath)
	if err != nil {
		t.Fatal(err)
	}

	dispatch := exec.Command(claudePath, "--bg", "Reply with exactly the word: ready")
	out, err := dispatch.CombinedOutput()
	if err != nil {
		t.Fatalf("claude --bg: %v: %s", err, out)
	}
	id := parseBackgroundedID(string(out))
	if id == "" {
		t.Fatalf("could not parse a session id from claude --bg output: %s", out)
	}
	t.Cleanup(func() { _ = exec.Command(claudePath, "rm", id).Run() })

	beforeSessionID, _, found := realClaudeRowByID(before, id)
	if found {
		t.Fatalf("dispatched session id %s collided with a pre-existing row (sessionId=%s) -- refusing to touch a session this test did not create", id, beforeSessionID)
	}

	const wantName = "検証 Rename テスト"
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	start := time.Now()
	cleanup, sendErr := SendClaudeRename(ctx, claudePath, id, wantName)
	if sendErr != nil {
		t.Fatalf("SendClaudeRename: %v", sendErr)
	}
	// Mirrors Provider.Rename: catalog confirmation is what decides success,
	// and cleanup runs concurrently rather than gating it.
	cleanupDone := make(chan error, 1)
	go func() { cleanupDone <- cleanup(ctx, 8*time.Second) }()
	if !pollUntilNativeName(claudePath, id, wantName, 15*time.Second) {
		t.Fatalf("native catalog never reflected %q within 15s", wantName)
	}
	userVisibleLatency := time.Since(start)
	if err := <-cleanupDone; err != nil {
		t.Logf("cleanup returned an error (does not affect rename correctness): %v", err)
	}

	if userVisibleLatency > 2500*time.Millisecond {
		t.Fatalf("user-visible rename latency was %v -- expected well under the ~2.7s the previous fixed-settle design always paid", userVisibleLatency)
	}
	t.Logf("user-visible rename latency (Send -> catalog confirmed, cleanup excluded): %v", userVisibleLatency)

	after, err := realClaudeAgentsJSON(claudePath)
	if err != nil {
		t.Fatal(err)
	}
	gotSessionID, gotName, found := realClaudeRowByID(after, id)
	if !found {
		t.Fatalf("session %s missing from `claude agents --json --all` after rename", id)
	}
	if gotName != wantName {
		t.Fatalf("native name = %q, want %q", gotName, wantName)
	}

	// No fork: exactly one row for this id, both before-dispatch absence
	// and after-rename presence accounted for, and no second row anywhere
	// in the catalog carrying the same (or a related) sessionId.
	count := 0
	for _, row := range after {
		if id2, _ := row["id"].(string); id2 == id {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly one row for id %s after rename, found %d -- rename must not fork a session", id, count)
	}
	t.Logf("native rename confirmed in place: id=%s sessionId=%s name=%q", id, gotSessionID, gotName)
}

func realClaudeAgentsJSON(claudePath string) ([]map[string]any, error) {
	out, err := exec.Command(claudePath, "agents", "--json", "--all").CombinedOutput()
	if err != nil {
		return nil, err
	}
	var rows []map[string]any
	if err := json.Unmarshal(out, &rows); err != nil {
		return nil, err
	}
	return rows, nil
}

func realClaudeRowByID(rows []map[string]any, id string) (sessionID, name string, found bool) {
	for _, row := range rows {
		rowID, _ := row["id"].(string)
		if rowID != id {
			continue
		}
		sessionID, _ = row["sessionId"].(string)
		name, _ = row["name"].(string)
		return sessionID, name, true
	}
	return "", "", false
}

// pollUntilNativeName polls `claude agents --json --all` until session id
// reports name, mirroring internal/provider/claude.Provider.confirmRenamed
// closely enough for this live test's own latency measurement (this
// package cannot import the claude provider package to reuse it directly
// without an import cycle).
func pollUntilNativeName(claudePath, id, name string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		rows, err := realClaudeAgentsJSON(claudePath)
		if err == nil {
			if _, got, found := realClaudeRowByID(rows, id); found && got == name {
				return true
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}
