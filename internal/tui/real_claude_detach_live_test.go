//go:build darwin || linux

package tui

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
)

// TestRealAgentsctlBinaryClaudeCtrlBracketDetach drives the actual built
// agentsctl binary over a real PTY, exactly as an interactive user would,
// attached to a REAL background `claude` session (never the fake CLI used
// by TestBuiltBinaryRealPTYJourney). It exists to catch anything that only
// manifests in the full TUI loop -- outer raw-mode setup, the outer
// bufio.Reader, App.attach wiring -- which a pty-package-only test calling
// AttachClaude directly cannot see.
func TestRealAgentsctlBinaryClaudeCtrlBracketDetach(t *testing.T) {
	if os.Getenv("AGENTSCTL_REAL_CLAUDE_DETACH") != "1" {
		t.Skip("set AGENTSCTL_REAL_CLAUDE_DETACH=1 for the live installed-claude full-binary detach test")
	}
	claudePath, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("claude CLI not installed")
	}
	moduleRoot := moduleRoot(t)
	root, err := os.MkdirTemp("/tmp", "agentsctl-real-claude-e2e-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	probe, err := net.Listen("unix", filepath.Join(root, "probe.sock"))
	if err != nil {
		if errors.Is(err, syscall.EPERM) {
			t.Skipf("sandbox does not permit Unix sockets: %v", err)
		}
		t.Fatal(err)
	}
	_ = probe.Close()
	_ = os.Remove(filepath.Join(root, "probe.sock"))
	stateDir := filepath.Join(root, "state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stopTestDaemon(stateDir) })

	// The project directory the real session is dispatched under, and the
	// directory agentsctl itself runs from, must match: the catalog scopes
	// by current directory unless "all folders" is toggled.
	project := moduleRoot

	binary := filepath.Join(root, "agentsctl")
	build := exec.Command("go", "build", "-o", binary, "./cmd/agentsctl")
	build.Dir = moduleRoot
	build.Env = environmentForTest(os.Environ(), "GOCACHE", filepath.Join(root, "go-cache"))
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build agentsctl: %v\n%s", err, output)
	}

	marker := fmt.Sprintf("probe%d", time.Now().UnixNano()%1_000_000)
	dispatch := exec.Command(claudePath, "--bg", marker+": run `sleep 2` four separate times back to back using the Bash tool (four separate tool calls, not one combined command), with no other output before or after.")
	dispatch.Dir = project
	out, err := dispatch.CombinedOutput()
	if err != nil {
		t.Fatalf("claude --bg: %v: %s", err, out)
	}
	id := parseBackgroundedIDForTest(string(out))
	if id == "" {
		t.Fatalf("could not parse a session id from claude --bg output: %s", out)
	}
	t.Cleanup(func() { _ = exec.Command(claudePath, "rm", id).Run() })
	t.Logf("dispatched real background session id=%s marker=%s", id, marker)

	command := exec.Command(binary)
	command.Dir = project
	command.Env = append(os.Environ(), "AGENTSCTL_STATE_DIR="+stateDir)
	terminal, err := pty.StartWithSize(command, &pty.Winsize{Rows: 30, Cols: 100})
	if err != nil {
		t.Fatal(err)
	}
	defer terminal.Close()
	var output lockedBuffer
	readDone := make(chan struct{})
	go func() { _, _ = io.Copy(&output, terminal); close(readDone) }()

	if err := waitOutput(&output, marker, 10*time.Second); err != nil {
		t.Fatalf("session %s (marker %s) did not appear in the real catalog: %v", id, marker, err)
	}
	t.Logf("catalog screen:\n%s", latestFrame(output.String()))

	// The freshly dispatched session has the newest CreatedAt of every row
	// in the catalog (across both providers, commingled and sorted newest
	// first), so it sorts to the top and is selected by default -- but
	// verify that explicitly instead of assuming it, so a sort-order
	// regression fails here with a clear message instead of silently
	// attaching the wrong row.
	screen := emulateANSIScreen(latestFrame(output.String()), 100, 30)
	selected := false
	for _, line := range screen {
		if strings.Contains(line, marker) {
			selected = strings.HasPrefix(line, "> ")
			break
		}
	}
	if !selected {
		t.Fatalf("dispatched session %s (marker %s) was not the selected (topmost) row:\n%s", id, marker, strings.Join(screen, "\n"))
	}
	overviewMarker := "agentsctl · current folder"

	attachAndDetach := func(round string, settle time.Duration) {
		t.Helper()
		writePTY(t, terminal, "\r")
		if err := waitOutput(&output, "\x1b[?1049h", 8*time.Second); err != nil {
			t.Fatalf("[%s] attach did not open an alternate screen: %v\nscreen:\n%s", round, err, latestFrame(output.String()))
		}
		// Let the client run for a bit -- with a multi-step task this lands
		// while it is genuinely still working (mid-tool-call), not just
		// idling at a finished prompt, which is closer to how a user would
		// actually check in on a long-running session before detaching.
		time.Sleep(settle)
		t.Logf("[%s] attached output before detach:\n%s", round, output.String())

		beforeCount := strings.Count(output.String(), overviewMarker)
		writePTY(t, terminal, "\x1d") // Ctrl+]
		if err := waitOutputAfter(&output, overviewMarker, beforeCount+1, 8*time.Second); err != nil {
			t.Fatalf("[%s] Ctrl+] did not return to the agentsctl overview after a real claude attach: %v\nfull output:\n%s", round, err, output.String())
		}
		t.Logf("[%s] returned to overview after Ctrl+]", round)

		state, status, err := claudeSessionStateForTest(claudePath, id)
		if err != nil {
			t.Fatal(err)
		}
		if state == "stopped" || state == "cancelled" || state == "canceled" {
			t.Fatalf("[%s] background session did not survive detach: state=%q status=%q", round, state, status)
		}
		t.Logf("[%s] background session survived detach: state=%q status=%q", round, state, status)
	}

	// First cycle: detach while the multi-step task is very likely still
	// mid-execution (each of its four steps sleeps 2s).
	attachAndDetach("first (mid-task)", 1500*time.Millisecond)
	// Second cycle: re-attach to the same still-running session and detach
	// again, to catch anything that only breaks on a repeat attach/detach
	// (stale reader state, a second raw-mode transition, etc).
	attachAndDetach("second (repeat attach)", 1500*time.Millisecond)

	writePTY(t, terminal, "\x1b")
	if err := command.Wait(); err != nil {
		t.Fatalf("agentsctl exit: %v\n%s", err, output.String())
	}
	<-readDone
}

var ansiEscapeForTest = ansiEscapeFor(`\x1b\[[0-9;]*m`)

func ansiEscapeFor(pattern string) func(string) string {
	// Minimal local ANSI-stripper (kept independent from any package-level
	// regexp import churn); matches the same "\x1b[...m" SGR shape used
	// elsewhere in this codebase's ANSI handling.
	return func(s string) string {
		var b strings.Builder
		for i := 0; i < len(s); {
			if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
				j := i + 2
				for j < len(s) && !(s[j] >= 0x40 && s[j] <= 0x7e) {
					j++
				}
				if j < len(s) {
					i = j + 1
					continue
				}
			}
			b.WriteByte(s[i])
			i++
		}
		return b.String()
	}
}

func parseBackgroundedIDForTest(output string) string {
	for _, line := range strings.Split(output, "\n") {
		plain := ansiEscapeForTest(line)
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

func claudeSessionStateForTest(claudePath, id string) (state, status string, err error) {
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
	return "", "", fmt.Errorf("session %s not found in claude agents --json --all output", id)
}
