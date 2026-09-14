package chatgpt

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

type fakeBrowserExecutor struct {
	foregroundPath string
	foregroundArgs []string
}

func (f *fakeBrowserExecutor) StartBackground(context.Context, string, []string) (processHandle, error) {
	return &fakeProcessHandle{done: make(chan struct{})}, nil
}
func (f *fakeBrowserExecutor) RunForeground(_ context.Context, path string, args []string, _ *os.File, _ io.Writer) error {
	f.foregroundPath = path
	f.foregroundArgs = append([]string(nil), args...)
	return nil
}

type fakeProcessHandle struct {
	done    chan struct{}
	stopped bool
}

func (f *fakeProcessHandle) Done() <-chan struct{} { return f.done }
func (f *fakeProcessHandle) Err() error            { return nil }
func (f *fakeProcessHandle) Stop() error           { f.stopped = true; return nil }

func TestRuntimeOpenUsesCanonicalAppModeInPersistentPartition(t *testing.T) {
	executor := &fakeBrowserExecutor{}
	helper := &fakeProcessHandle{done: make(chan struct{})}
	runtime := &runtime{
		path:        "terminal-browser-test",
		partition:   "agentsctl-chatgpt",
		executor:    executor,
		helper:      helper,
		bridge:      &fakeDiscoveryBridge{},
		preloadPath: "/owned/preload.js",
	}
	if err := runtime.Open(context.Background(), conversationA, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	wantURL := "https://chatgpt.com/c/" + conversationA
	for _, arg := range []string{wantURL, "--app-mode", "--no-merge", "--partition=agentsctl-chatgpt", "--preload=/owned/preload.js"} {
		if !slices.Contains(executor.foregroundArgs, arg) {
			t.Fatalf("foreground args=%v, missing %q", executor.foregroundArgs, arg)
		}
	}
	if executor.foregroundPath != "terminal-browser-test" {
		t.Fatalf("foreground path=%q", executor.foregroundPath)
	}
	if !strings.Contains(preloadScriptTemplate, "isCloseShortcut(event)") ||
		!strings.Contains(preloadScriptTemplate, "globalThis.terminalBrowser.quit()") {
		t.Fatal("embedded preload does not preserve Ctrl+] close-view behavior")
	}
}

// TestListResetsDiscoveryOnBrokenTransport fixes the self-healing
// guarantee behind "chatgpt unavailable: receive browser bridge
// scrollRegion: EOF" never becoming permanent: once the bridge socket
// connection itself has broken (not merely a cancelled/timed-out request
// against an otherwise-healthy connection), List must tear down the
// current helper/bridge so the *next* List call re-materializes a fresh
// one via ensureDiscoveryLocked, rather than reusing -- and immediately
// failing against -- the same dead connection forever (see
// resetDiscoveryIfBrokenLocked's doc comment). ensureDiscoveryLocked's own
// helper.Done() check alone never catches this: the helper OS process here
// is still running the whole time.
func TestListResetsDiscoveryOnBrokenTransport(t *testing.T) {
	helper := &fakeProcessHandle{done: make(chan struct{})} // never closes: the process is still alive
	bridge := &fakeDiscoveryBridge{beginListErr: errors.New("receive browser bridge scrollRegion: EOF")}
	rt := &runtime{helper: helper, bridge: bridge}

	if _, err := rt.List(context.Background(), "g-p-test"); err == nil {
		t.Fatal("expected the broken-transport error to propagate")
	}
	if rt.bridge != nil || rt.helper != nil {
		t.Fatalf("broken transport must reset both bridge and helper, got bridge=%v helper=%v", rt.bridge, rt.helper)
	}
	if !bridge.closed {
		t.Fatal("the broken bridge connection must be closed on reset")
	}
	if !helper.stopped {
		t.Fatal("the helper process must be stopped on reset")
	}
}

// TestListDoesNotResetDiscoveryOnContextCancellation fixes the other half
// of resetDiscoveryIfBrokenLocked's guarantee: a request that failed only
// because its own ctx was cancelled or timed out (e.g. a superseded
// catalog reload -- see agentview.Runtime.requestReload) must NOT tear
// down an otherwise-healthy connection; the caller is expected to retry
// against the same bridge next time.
func TestListDoesNotResetDiscoveryOnContextCancellation(t *testing.T) {
	helper := &fakeProcessHandle{done: make(chan struct{})}
	bridge := &fakeDiscoveryBridge{beginListErr: context.Canceled}
	rt := &runtime{helper: helper, bridge: bridge}

	if _, err := rt.List(context.Background(), "g-p-test"); err == nil {
		t.Fatal("expected the cancellation error to propagate")
	}
	if rt.bridge == nil || rt.helper == nil {
		t.Fatal("a context-cancellation failure must not reset an otherwise-healthy bridge/helper")
	}
	if bridge.closed || helper.stopped {
		t.Fatal("a context-cancellation failure must not close the bridge or stop the helper")
	}
}

func TestGracefulCommandUsesBoundedCancellation(t *testing.T) {
	cmd := gracefulCommand(context.Background(), "terminal-browser", "open")
	if cmd.Cancel == nil {
		t.Fatal("command cancellation must send a graceful termination signal")
	}
	if cmd.WaitDelay != 5*time.Second {
		t.Fatalf("WaitDelay=%s, want 5s", cmd.WaitDelay)
	}
}

func TestRuntimeMaterializesOwnedScriptsAndCleansThemUp(t *testing.T) {
	runtime := &runtime{}
	runtime.mu.Lock()
	if err := runtime.materializeAssetsLocked(); err != nil {
		runtime.mu.Unlock()
		t.Fatal(err)
	}
	dir, mainPath, socketPath := runtime.assetDir, runtime.mainPath, runtime.socketPath
	runtime.mu.Unlock()
	mainContents, err := os.ReadFile(mainPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(mainContents), "__AGENTSCTL_CHATGPT_SOCKET_PATH__") || !strings.Contains(string(mainContents), socketPath) {
		t.Fatal("materialized main script did not receive its private socket path")
	}
	preloadContents, err := os.ReadFile(runtime.preloadPath)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.ownerToken == "" || strings.Contains(string(mainContents), "__AGENTSCTL_CHATGPT_OWNER_TOKEN__") ||
		strings.Contains(string(preloadContents), "__AGENTSCTL_CHATGPT_OWNER_TOKEN__") ||
		!strings.Contains(string(mainContents), runtime.ownerToken) || !strings.Contains(string(preloadContents), runtime.ownerToken) {
		t.Fatal("materialized bridge did not receive its private ownership token")
	}
	if strings.Contains(string(preloadContents), navigationPlaceholder) ||
		strings.Contains(string(preloadContents), rendererNavigationPlaceholder) ||
		!strings.Contains(string(preloadContents), "class NumberJumpState") ||
		!strings.Contains(string(preloadContents), "class ChatGPTNavigation") {
		t.Fatal("materialized preload does not contain the renderer navigation modules")
	}
	if filepath.Dir(socketPath) != dir {
		t.Fatalf("socket=%q is outside owned runtime dir %q", socketPath, dir)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("owned runtime directory still exists: %v", err)
	}
}

func TestEmbeddedBridgeContainsNoCredentialTransport(t *testing.T) {
	combined := strings.ToLower(mainScriptTemplate + preloadScriptTemplate + string(navigationScript) +
		string(rendererNavigationScript) + string(ownershipScript) + string(captureScript))
	for _, forbidden := range []string{"authorization", "access_token", "refresh_token", "document.cookie"} {
		if strings.Contains(combined, forbidden) {
			t.Fatalf("embedded bridge contains forbidden credential transport %q", forbidden)
		}
	}
}
