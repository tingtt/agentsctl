//go:build darwin || linux

package claude

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"testing"
	"time"

	base "github.com/tingtt/agentsctl/internal/provider"
)

// TestRealClaudeRenameMutatesSessionInPlace exercises the actual installed
// `claude` CLI (never a fake): it dispatches a real, disposable background
// session, renames it via sendClaudeRename gated by the same readiness
// check Provider.Rename uses (waitSessionLive), and confirms -- via `claude
// agents --json --all`, the same native source List/confirmRenamed use,
// never PTY output -- that the command was actually submitted: the native
// name changed on the very same session (same id and sessionId) with no
// extra session created alongside it.
//
// Both names are long enough that "/rename <name>\r" is 64 or more
// characters: the issue #75 regression, where Claude handled such an
// unbracketed burst as a paste and left the command unsubmitted in the
// composer. One is plain ASCII and one mixes Japanese and spaces, since
// the threshold counts characters, not bytes. It covers two session
// states:
//
//   - live: the session's worker is running (its turn has finished). The
//     user-visible rename latency (Send -> catalog confirms the new name,
//     mirroring Provider.Rename -- cleanup is excluded, since Rename runs
//     it concurrently) is asserted to stay well under the ~2.7s of fixed
//     settle delay a previous design always paid, without hard-coding a
//     specific millisecond figure that would make this test flaky.
//   - stopped: `claude attach` must respawn the worker first, and input
//     written before its REPL is mounted is never submitted; this covers
//     the readiness wait (waitSessionLive).
//
// sendClaudeRename's doc comment records what was separately, manually
// verified for a working (mid-tool-call) session: same id/sessionId/pid,
// no fork, no interruption of the in-flight background execution.
func TestRealClaudeRenameMutatesSessionInPlace(t *testing.T) {
	if os.Getenv("AGENTSCTL_REAL_CLAUDE_RENAME") != "1" {
		t.Skip("set AGENTSCTL_REAL_CLAUDE_RENAME=1 for the live installed-claude rename test")
	}
	claudePath, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("claude CLI not installed")
	}
	for _, tc := range []struct {
		state string
		stop  bool
		name  string
	}{
		{state: "live", stop: false, name: "#75 live: submit /rename correctly for existing Claude sessions"},
		{state: "stopped", stop: true, name: "#75 stopped: 既存の Claude セッションで /rename を正しく submit できることを確認する長い名前です"},
	} {
		t.Run(tc.state, func(t *testing.T) {
			id := dispatchRealClaudeSession(t, claudePath)
			if tc.stop {
				if out, err := exec.Command(claudePath, "stop", id).CombinedOutput(); err != nil {
					t.Fatalf("claude stop: %v: %s", err, out)
				}
			}
			before, err := realClaudeAgentsJSON(claudePath)
			if err != nil {
				t.Fatal(err)
			}
			beforeSessionID, _, found := realClaudeRowByID(before, id)
			if !found {
				t.Fatalf("dispatched session %s missing from the catalog before rename", id)
			}

			p := Provider{Path: claudePath, Runner: base.ExecRunner{}}
			wantName := tc.name
			if n := len([]rune("/rename " + wantName + "\r")); n < 64 {
				t.Fatalf("test name gives a %d-character command; it must be at least 64 to cover issue #75", n)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()

			start := time.Now()
			cleanup, sendErr := sendClaudeRename(ctx, claudePath, id, wantName, func(ctx context.Context) error {
				return p.waitSessionLive(ctx, id)
			})
			if sendErr != nil {
				t.Fatalf("sendClaudeRename: %v", sendErr)
			}
			cleanupDone := make(chan error, 1)
			go func() { cleanupDone <- cleanup(ctx, 8*time.Second) }()
			if !pollUntilNativeName(claudePath, id, wantName, 15*time.Second) {
				t.Fatalf("native catalog never reflected %q within 15s -- /rename was not submitted", wantName)
			}
			userVisibleLatency := time.Since(start)
			if err := <-cleanupDone; err != nil {
				t.Logf("cleanup returned an error (does not affect rename correctness): %v", err)
			}
			t.Logf("user-visible rename latency (Send -> catalog confirmed, cleanup excluded): %v", userVisibleLatency)
			if !tc.stop && userVisibleLatency > 2500*time.Millisecond {
				t.Fatalf("user-visible rename latency was %v -- expected well under the ~2.7s a previous fixed-settle design always paid", userVisibleLatency)
			}

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
			if gotSessionID != beforeSessionID {
				t.Fatalf("sessionId changed from %s to %s -- rename must not replace the session", beforeSessionID, gotSessionID)
			}
			// No fork: exactly one row for this id after rename.
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
		})
	}
}

// dispatchRealClaudeSession starts a disposable background session,
// registers its removal, and waits for its single turn to finish so the
// caller starts from a settled session.
func dispatchRealClaudeSession(t *testing.T, claudePath string) string {
	t.Helper()
	before, err := realClaudeAgentsJSON(claudePath)
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(claudePath, "--bg", "Reply with exactly the word: ready").CombinedOutput()
	if err != nil {
		t.Fatalf("claude --bg: %v: %s", err, out)
	}
	id := parseBackgroundedID(string(out))
	if id == "" {
		t.Fatalf("could not parse a session id from claude --bg output: %s", out)
	}
	t.Cleanup(func() { _ = exec.Command(claudePath, "rm", id).Run() })
	if sessionID, _, found := realClaudeRowByID(before, id); found {
		t.Fatalf("dispatched session id %s collided with a pre-existing row (sessionId=%s) -- refusing to touch a session this test did not create", id, sessionID)
	}
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if rows, err := realClaudeAgentsJSON(claudePath); err == nil {
			for _, row := range rows {
				if row["id"] == id && row["state"] == "done" {
					return id
				}
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("session %s did not finish its turn within 60s", id)
	return ""
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
// reports name, mirroring Provider.confirmRenamed closely enough for this
// live test's own latency measurement.
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
