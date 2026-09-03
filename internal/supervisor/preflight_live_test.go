//go:build darwin || linux

package supervisor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/tingtt/agentsctl/internal/state"
)

func TestRealCodexDaemonPreflight(t *testing.T) {
	if os.Getenv("AGENTSCTL_REAL_CODEX_PREFLIGHT") != "1" {
		t.Skip("set AGENTSCTL_REAL_CODEX_PREFLIGHT=1 for the read-only installed-provider preflight")
	}
	binary := os.Getenv("AGENTSCTL_TEST_BINARY")
	if binary == "" {
		t.Fatal("AGENTSCTL_TEST_BINARY is required")
	}
	wantCodex, err := exec.LookPath("codex")
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp("/tmp", "agentsctl-preflight-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	client := Client{
		Socket: filepath.Join(root, "supervisor.sock"), DaemonPath: binary,
		StatePath: filepath.Join(root, "state.json"),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := client.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	handshake, err := client.Call(ctx, Request{Action: "ping"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if handshake.DaemonPID > 0 {
			_ = syscall.Kill(handshake.DaemonPID, syscall.SIGTERM)
		}
	})
	if handshake.ProtocolVersion != ProtocolVersion || handshake.BuildVersion != BuildVersion || handshake.DaemonUID != uint32(os.Getuid()) || handshake.DaemonParentPID <= 0 || handshake.DaemonCWD == "" || handshake.DaemonPATH == "" {
		t.Fatalf("incomplete daemon handshake: %+v", handshake)
	}
	preflight, err := client.Preflight(ctx, "codex", "--version")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Clean(preflight.ResolvedPath) != filepath.Clean(wantCodex) {
		t.Fatalf("daemon codex=%q shell codex=%q", preflight.ResolvedPath, wantCodex)
	}
	if !strings.Contains(preflight.Output, "codex-cli") {
		t.Fatalf("unexpected codex --version output: %q", preflight.Output)
	}
	started, err := client.Call(ctx, Request{Action: "start", RunID: "codex-version", Provider: "codex", Args: []string{"--version"}, CWD: handshake.DaemonCWD})
	if err != nil || started.Run == nil || started.Run.PID <= 0 {
		t.Fatalf("daemon PTY codex --version: response=%+v err=%v", started, err)
	}
	waitLivePreflightStopped(t, client.StatePath, "codex-version")
	t.Logf("shell/daemon codex=%s output=%q daemonPid=%d parentPid=%d cwd=%s build=%s", wantCodex, strings.TrimSpace(preflight.Output), handshake.DaemonPID, handshake.DaemonParentPID, handshake.DaemonCWD, handshake.BuildVersion)
}

func waitLivePreflightStopped(t *testing.T, path, id string) {
	t.Helper()
	store := state.New(path)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := store.Load()
		if err == nil {
			if run, ok := data.Runs[id]; ok && run.State == "stopped" && run.PID == 0 {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("PTY codex --version did not exit cleanly")
}
