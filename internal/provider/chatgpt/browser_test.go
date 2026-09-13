package chatgpt

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
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
	if !strings.Contains(string(preloadScript), `event.ctrlKey && event.key === "]"`) ||
		!strings.Contains(string(preloadScript), "globalThis.terminalBrowser.quit()") {
		t.Fatal("embedded preload does not preserve Ctrl+] close-view behavior")
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
	combined := strings.ToLower(mainScriptTemplate + string(preloadScript))
	for _, forbidden := range []string{"authorization", "access_token", "refresh_token", "document.cookie"} {
		if strings.Contains(combined, forbidden) {
			t.Fatalf("embedded bridge contains forbidden credential transport %q", forbidden)
		}
	}
}
