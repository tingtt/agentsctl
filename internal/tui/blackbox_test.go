//go:build darwin || linux

package tui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/tingtt/agentsctl/internal/state"
	"github.com/tingtt/agentsctl/internal/supervisor"
	"golang.org/x/sys/unix"
)

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

func TestBuiltBinaryRealPTYJourney(t *testing.T) {
	moduleRoot := moduleRoot(t)
	root, err := os.MkdirTemp("/tmp", "agentsctl-e2e-")
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
	bin := filepath.Join(root, "bin")
	project := filepath.Join(root, "project")
	fakeDir := filepath.Join(root, "fake")
	stateDir := filepath.Join(root, "state")
	for _, dir := range []string{bin, project, fakeDir, stateDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	project, err = filepath.EvalSymlinks(project)
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(root, "agentsctl")
	build := exec.Command("go", "build", "-o", binary, "./cmd/agentsctl")
	build.Dir = moduleRoot
	build.Env = environmentForTest(os.Environ(), "GOCACHE", filepath.Join(root, "go-cache"))
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build agentsctl: %v\n%s", err, output)
	}
	for _, name := range []string{"codex", "claude"} {
		if err := os.Symlink(filepath.Join(moduleRoot, "internal", "testkit", "fakecli", name), filepath.Join(bin, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeCatalogFixture(fakeDir, project, 100); err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(root, "run-agentsctl")
	wrapperSource := "#!/bin/sh\nstty -g > \"$AGENTSCTL_FAKE_DIR/before.stty\"\n\"$AGENTSCTL_TEST_BINARY\"\nresult=$?\nstty -g > \"$AGENTSCTL_FAKE_DIR/after.stty\"\nexit $result\n"
	if err := os.WriteFile(wrapper, []byte(wrapperSource), 0o700); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(wrapper)
	command.Dir = project
	command.Env = append(os.Environ(),
		"AGENTSCTL_FAKE_DIR="+fakeDir,
		"AGENTSCTL_STATE_DIR="+stateDir,
		"AGENTSCTL_TEST_BINARY="+binary,
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
	t.Cleanup(func() { stopTestDaemon(stateDir) })
	terminal, err := pty.StartWithSize(command, &pty.Winsize{Rows: 24, Cols: 80})
	if err != nil {
		t.Fatal(err)
	}
	defer terminal.Close()
	var output lockedBuffer
	readDone := make(chan struct{})
	go func() { _, _ = io.Copy(&output, terminal); close(readDone) }()

	if err := waitOutput(&output, "session-099", 8*time.Second); err != nil {
		if strings.Contains(strings.ToLower(output.String()), "operation not permitted") {
			t.Skipf("sandbox does not permit daemon/PTY boundary: %s", output.String())
		}
		t.Fatal(err)
	}
	assertScreen(t, output.String(), 80, 24, "session-099", "Ctrl+X", "claude >")
	writePTY(t, terminal, "\x01")
	if err := waitOutput(&output, "MUST-NOT-SHOW-SIBLING", 3*time.Second); err != nil {
		t.Fatal("Ctrl+A did not expose all directories")
	}
	writePTY(t, terminal, "\x01")
	if err := waitLatestFrame(&output, 3*time.Second, func(frame string) bool {
		return strings.Contains(frame, "agentsctl · current folder") && !strings.Contains(frame, "MUST-NOT-SHOW-SIBLING")
	}); err != nil {
		t.Fatalf("Ctrl+A did not restore current-directory scope:\n%s", latestFrame(output.String()))
	}

	writePTY(t, terminal, "\x1b[Z")
	if err := waitOutput(&output, "codex >", 3*time.Second); err != nil {
		t.Fatal(err)
	}
	frameCount := strings.Count(output.String(), "\x1b[2J\x1b[H")
	writePTY(t, terminal, "\x16\x1b[55;5u")
	if err := waitLatestFrame(&output, 3*time.Second, func(string) bool {
		return strings.Count(output.String(), "\x1b[2J\x1b[H") >= frameCount+2
	}); err != nil {
		t.Fatal(err)
	}
	screen := latestFrame(output.String())
	if strings.Contains(screen, "archived") || strings.Contains(screen, "55;5u") || strings.Contains(screen, "7;5u") {
		t.Fatalf("unsupported input leaked into screen:\n%s", screen)
	}

	if err := pty.Setsize(terminal, &pty.Winsize{Rows: 10, Cols: 48}); err != nil {
		t.Fatal(err)
	}
	writePTY(t, terminal, "\x0c")
	if err := waitLatestFrame(&output, 3*time.Second, func(frame string) bool {
		return len(frameLines(frame)) == 10
	}); err != nil {
		t.Fatal(err)
	}
	assertScreen(t, output.String(), 48, 10, "session-099", "Ctrl+X", "codex >")
	if err := pty.Setsize(terminal, &pty.Winsize{Rows: 24, Cols: 80}); err != nil {
		t.Fatal(err)
	}
	writePTY(t, terminal, "\x0c")

	writePTY(t, terminal, "\r")
	if err := waitOutput(&output, "FAKE CODEX ATTACHED", 5*time.Second); err != nil {
		t.Fatal(err)
	}
	run := waitRunningRun(t, filepath.Join(stateDir, "state.json"))
	overviewCount := strings.Count(output.String(), "agentsctl · current folder")
	writePTY(t, terminal, "\x1d")
	if err := waitOutputAfter(&output, "agentsctl · current folder", overviewCount+1, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(run.PID, 0); err != nil {
		t.Fatalf("child did not survive detach: %v", err)
	}

	writePTY(t, terminal, "\x18")
	waitRunStopped(t, filepath.Join(stateDir, "state.json"), run.ID)
	if err := syscall.Kill(run.PID, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("child remained alive after Stop: %v", err)
	}
	writePTY(t, terminal, "\x18")
	if err := waitOutput(&output, "Press Ctrl+X again to archive", 3*time.Second); err != nil {
		t.Fatal(err)
	}
	writePTY(t, terminal, "\x18")
	waitArchived(t, filepath.Join(fakeDir, "codex.json"), "session-099", &output)
	if err := os.WriteFile(filepath.Join(fakeDir, "codex.json"), []byte("[]"), 0o600); err != nil {
		t.Fatal(err)
	}
	claudeFixture, _ := json.Marshal([]map[string]any{{
		"id": "claude-live", "name": "fake-claude-live", "summary": "Working", "cwd": project,
		"updatedAt": time.Now().Unix(), "status": "working",
	}})
	if err := os.WriteFile(filepath.Join(fakeDir, "claude.json"), claudeFixture, 0o600); err != nil {
		t.Fatal(err)
	}
	writePTY(t, terminal, "\x0c")
	if err := waitOutput(&output, "fake-claude-live", 3*time.Second); err != nil {
		t.Fatal(err)
	}
	writePTY(t, terminal, "\r")
	if err := waitOutput(&output, "FAKE CLAUDE ATTACHED", 5*time.Second); err != nil {
		t.Fatal(err)
	}
	overviewCount = strings.Count(output.String(), "agentsctl · current folder")
	writePTY(t, terminal, "\x1d")
	if err := waitOutputAfter(&output, "agentsctl · current folder", overviewCount+1, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	claudeData, err := os.ReadFile(filepath.Join(fakeDir, "claude.json"))
	if err != nil || !strings.Contains(string(claudeData), "working") {
		t.Fatalf("Claude session did not survive detach: data=%s err=%v", claudeData, err)
	}
	if err := os.WriteFile(filepath.Join(fakeDir, "codex.json"), []byte("not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	writePTY(t, terminal, "\x0c")
	if err := waitOutput(&output, "unavailable:", 3*time.Second); err != nil {
		t.Fatal(err)
	}
	assertScreen(t, output.String(), 80, 24, "fake-claude-live", "unavailable:", "Ctrl+X")

	writePTY(t, terminal, "\x1b")
	if err := command.Wait(); err != nil {
		t.Fatalf("agentsctl exit: %v\n%s", err, output.String())
	}
	<-readDone
	before, err := os.ReadFile(filepath.Join(fakeDir, "before.stty"))
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join(fakeDir, "after.stty"))
	if err != nil {
		t.Fatal(err)
	}
	if normalizeStty(string(before)) != normalizeStty(string(after)) {
		t.Fatalf("terminal mode was not restored: before=%q after=%q", before, after)
	}
	stopTestDaemon(stateDir)
}

func normalizeStty(value string) string {
	fields := strings.Split(strings.TrimSpace(value), ":")
	for i, field := range fields {
		if !strings.HasPrefix(field, "lflag=") {
			continue
		}
		flags, err := strconv.ParseUint(strings.TrimPrefix(field, "lflag="), 16, 64)
		if err == nil {
			fields[i] = fmt.Sprintf("lflag=%x", flags&^uint64(unix.PENDIN))
		}
	}
	return strings.Join(fields, ":")
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test source")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func writeCatalogFixture(root, current string, count int) error {
	rows := make([]map[string]any, 0, count+2)
	for i := 0; i < count; i++ {
		rows = append(rows, map[string]any{
			"id": fmt.Sprintf("session-%03d", i), "name": fmt.Sprintf("session-%03d", i),
			"preview": "Ready", "cwd": current, "createdAt": i + 1, "updatedAt": i + 1,
			"status": map[string]any{"type": "idle"}, "archived": false,
		})
	}
	rows = append(rows,
		map[string]any{"id": "sibling", "name": "MUST-NOT-SHOW-SIBLING", "cwd": filepath.Join(filepath.Dir(current), "sibling"), "updatedAt": count + 2, "status": map[string]any{"type": "idle"}, "archived": false},
		map[string]any{"id": "archived", "name": "MUST-NOT-SHOW-ARCHIVED", "cwd": current, "updatedAt": count + 3, "status": map[string]any{"type": "idle"}, "archived": true},
	)
	b, err := json.Marshal(rows)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(root, "codex.json"), b, 0o600); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(root, "claude.json"), []byte("[]"), 0o600)
}

func environmentForTest(environment []string, key, value string) []string {
	prefix := key + "="
	result := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		if !strings.HasPrefix(entry, prefix) {
			result = append(result, entry)
		}
	}
	return append(result, prefix+value)
}

func writePTY(t *testing.T, terminal *os.File, value string) {
	t.Helper()
	if _, err := terminal.WriteString(value); err != nil {
		t.Fatal(err)
	}
}

func waitOutput(output *lockedBuffer, text string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if strings.Contains(output.String(), text) {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for %q\n%s", text, output.String())
}

func waitOutputAfter(output *lockedBuffer, text string, minimumCount int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if strings.Count(output.String(), text) >= minimumCount {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for occurrence %d of %q\n%s", minimumCount, text, output.String())
}

func latestFrame(output string) string {
	const clear = "\x1b[2J\x1b[H"
	if index := strings.LastIndex(output, clear); index >= 0 {
		output = output[index+len(clear):]
	}
	return output
}

func waitLatestFrame(output *lockedBuffer, timeout time.Duration, predicate func(string) bool) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if predicate(latestFrame(output.String())) {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return errors.New("timed out waiting for final screen state")
}

func frameLines(frame string) []string {
	lines := strings.Split(frame, "\r\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func assertScreen(t *testing.T, output string, width, height int, required ...string) {
	t.Helper()
	frame := latestFrame(output)
	if strings.Contains(strings.ReplaceAll(frame, "\r\n", ""), "\n") {
		t.Fatalf("screen contains bare LF:\n%q", frame)
	}
	lines := frameLines(frame)
	if len(lines) != height {
		t.Fatalf("screen lines=%d want=%d\n%s", len(lines), height, frame)
	}
	for _, line := range lines {
		if len([]rune(line)) > width {
			t.Fatalf("screen row exceeds width %d: %q", width, line)
		}
		if strings.Contains(line, "codex  session-") && strings.Index(line, "codex") != 2 {
			t.Fatalf("session row did not start at the shared column: %q", line)
		}
	}
	for _, text := range required {
		if !strings.Contains(frame, text) {
			t.Fatalf("screen missing %q:\n%s", text, frame)
		}
	}
	for _, forbidden := range []string{"MUST-NOT-SHOW-SIBLING", "MUST-NOT-SHOW-ARCHIVED"} {
		if strings.Contains(frame, forbidden) {
			t.Fatalf("screen contains %q:\n%s", forbidden, frame)
		}
	}
}

func waitRunningRun(t *testing.T, path string) state.Run {
	t.Helper()
	store := state.New(path)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, _ := store.Load()
		for _, run := range data.Runs {
			if run.State == "running" && run.PID > 0 {
				return run
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("managed child did not reach running state")
	return state.Run{}
}

func waitRunStopped(t *testing.T, path, id string) {
	t.Helper()
	store := state.New(path)
	deadline := time.Now().Add(7 * time.Second)
	for time.Now().Before(deadline) {
		data, _ := store.Load()
		if run, ok := data.Runs[id]; ok && run.State == "stopped" && run.PID == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("managed child did not stop")
}

func waitArchived(t *testing.T, path, id string, output *lockedBuffer) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		b, _ := os.ReadFile(path)
		var rows []map[string]any
		_ = json.Unmarshal(b, &rows)
		for _, row := range rows {
			if row["id"] == id && row["archived"] == true {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	b, _ := os.ReadFile(path)
	t.Fatalf("session was not archived; catalog=%s\nscreen=%s", b, latestFrame(output.String()))
}

func stopTestDaemon(stateDir string) {
	client := supervisor.Client{Socket: filepath.Join(stateDir, "supervisor.sock")}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	response, err := client.Call(ctx, supervisor.Request{Action: "ping"})
	if err == nil && response.DaemonPID > 0 {
		_ = syscall.Kill(response.DaemonPID, syscall.SIGTERM)
	}
}
