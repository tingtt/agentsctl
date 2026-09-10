//go:build darwin || linux

package supervisor

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/tingtt/agentsctl/internal/localstate"
	processinfo "github.com/tingtt/agentsctl/internal/process"
	"github.com/tingtt/agentsctl/internal/supervisor/protocol"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

func TestDetachDoesNotStopManagedChild(t *testing.T) {
	dir := t.TempDir()
	st := localstate.New(filepath.Join(dir, "state.json"))
	srv := &Server{Store: st, runs: map[string]*process{}}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	srv.ResolveExecutable = func(string) (string, error) { return exe, nil }
	res := callServer(t, srv, Request{Action: "start", RunID: "r", SessionID: "thread", Args: []string{"-test.run=TestSupervisorPTYHelper"}, Provider: "codex", CWD: dir})
	if !res.OK && strings.Contains(res.Error, "operation not permitted") {
		t.Skip("sandbox does not permit PTY process spawn")
	}
	if !res.OK {
		t.Fatal(res.Error)
	}
	if res.Run == nil || res.Run.PID == 0 {
		t.Fatal("child not started")
	}
	client, server := net.Pipe()
	go srv.handle(server)
	b, _ := json.Marshal(Request{Action: "attach", RunID: "r"})
	_ = protocol.Write(client, protocol.Request, b)
	_, _, _ = protocol.Read(client)
	_ = protocol.Write(client, protocol.Detach, nil)
	_ = client.Close()
	time.Sleep(20 * time.Millisecond)
	runs, _ := st.Runs()
	if runs["r"].State != "running" {
		t.Fatalf("state=%s", runs["r"].State)
	}
	stop := callServer(t, srv, Request{Action: "stop", RunID: "r"})
	if !stop.OK {
		t.Fatal(stop.Error)
	}
	runs, err = st.Runs()
	if err != nil || runs["r"].SessionID != "thread" {
		t.Fatalf("stopped run lost session binding: run=%+v err=%v", runs["r"], err)
	}
}

func TestClientDaemonLauncherResolvesExecutableFromDaemonPATH(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "agentsctl-launcher-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "fake-codex")
	if err := os.WriteFile(target, []byte("#!/bin/sh\nprintf 'fake-codex 1.0\\n'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(bin, "codex")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	socket := filepath.Join(root, "supervisor.sock")
	store := localstate.New(filepath.Join(root, "state.json"))
	server := &Server{Socket: socket, Store: store}
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	waitForSocket(t, socket, done)
	client := Client{Socket: socket}
	preflight, err := client.Preflight(context.Background(), "codex", "--version")
	if err != nil {
		t.Fatal(err)
	}
	if preflight.ResolvedPath != link || strings.TrimSpace(preflight.Output) != "fake-codex 1.0" {
		t.Fatalf("preflight=%+v", preflight)
	}
	started, err := client.Call(context.Background(), Request{Action: "start", RunID: "version", Provider: "codex", Args: []string{"--version"}, CWD: root})
	if err != nil || started.Run == nil || started.Run.PID == 0 {
		t.Fatalf("start=%+v err=%v", started, err)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server did not stop")
	}
}

func TestSpawnFailureDoesNotLeaveStartedRun(t *testing.T) {
	dir := t.TempDir()
	store := localstate.New(filepath.Join(dir, "state.json"))
	server := &Server{Store: store, runs: map[string]*process{}, ResolveExecutable: func(string) (string, error) {
		return filepath.Join(dir, "missing"), nil
	}}
	response := callServer(t, server, Request{Action: "start", RunID: "failed", Provider: "codex", CWD: dir})
	if response.OK || !strings.Contains(response.Error, "PTY process spawn") {
		t.Fatalf("response=%+v", response)
	}
	runs, err := store.Runs()
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := runs["failed"]; exists {
		t.Fatal("failed executable remained as a started run")
	}
}

func TestHandshakeReportsProtocolBuildAndDaemonIdentity(t *testing.T) {
	server := &Server{Store: localstate.New(filepath.Join(t.TempDir(), "state.json"))}
	response := callServer(t, server, Request{Action: "ping"})
	if !response.OK || response.ProtocolVersion != ProtocolVersion || response.BuildVersion != BuildVersion || response.DaemonPID != os.Getpid() || response.DaemonStartTime == 0 {
		t.Fatalf("handshake=%+v", response)
	}
}

func TestCompatibilityRejectsSupervisorWithoutEnvironmentOverrides(t *testing.T) {
	if compatible(Response{ProtocolVersion: 2, BuildVersion: "session-lifecycle-2026-09-03"}) {
		t.Fatal("protocol 2 supervisor was treated as compatible with environment overrides")
	}
	if !compatible(Response{ProtocolVersion: ProtocolVersion, BuildVersion: BuildVersion}) {
		t.Fatal("current supervisor protocol/build was treated as incompatible")
	}
}

func TestManagedChildEnvironmentMergesRequestOverrides(t *testing.T) {
	t.Setenv("CODEX_EDITOR", "nvim")
	t.Setenv("PATH", "/test/bin:/usr/bin")
	t.Setenv("HOME", "/test/home")

	cases := []struct {
		name            string
		provider        string
		overrides       map[string]string
		inheritedEditor string
		unsetEditor     bool
		wantEditor      string
	}{
		{name: "Codex override without inherited editor", provider: "codex", overrides: map[string]string{"EDITOR": "nvim"}, unsetEditor: true, wantEditor: "nvim"},
		{name: "Codex override replaces inherited editor", provider: "codex", overrides: map[string]string{"EDITOR": "nvim"}, inheritedEditor: "vim", wantEditor: "nvim"},
		{name: "Codex inheritance", provider: "codex", inheritedEditor: "vim", wantEditor: "vim"},
		{name: "Claude does not interpret CODEX_EDITOR", provider: "claude", inheritedEditor: "vim", wantEditor: "vim"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("EDITOR", tc.inheritedEditor)
			if tc.unsetEditor {
				if err := os.Unsetenv("EDITOR"); err != nil {
					t.Fatal(err)
				}
			}
			dir := t.TempDir()
			output := filepath.Join(dir, "environment.json")
			statePath := filepath.Join(dir, "state.json")
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			server := &Server{
				Store: localstate.New(statePath),
				runs:  map[string]*process{},
				ResolveExecutable: func(string) (string, error) {
					return executable, nil
				},
			}
			overrides := make(map[string]string, len(tc.overrides)+1)
			for key, value := range tc.overrides {
				overrides[key] = value
			}
			overrides["AGENTSCTL_CHILD_ENV_FILE"] = output
			response := callServer(t, server, Request{
				Action:      "start",
				RunID:       "environment",
				Provider:    tc.provider,
				Args:        []string{"-test.run=^TestManagedChildEnvironmentHelper$"},
				CWD:         dir,
				Environment: overrides,
			})
			if !response.OK && strings.Contains(response.Error, "operation not permitted") {
				t.Skip("sandbox does not permit PTY process spawn")
			}
			if !response.OK {
				t.Fatal(response.Error)
			}

			environment := readChildEnvironment(t, output)
			if got := environment["EDITOR"]; len(got) != 1 || got[0] != tc.wantEditor {
				t.Fatalf("child EDITOR entries=%v, want exactly [%q]", got, tc.wantEditor)
			}
			if got := environment["PATH"]; len(got) != 1 || got[0] != "/test/bin:/usr/bin" {
				t.Fatalf("child PATH entries=%v, want inherited PATH", got)
			}
			if got := environment["HOME"]; len(got) != 1 || got[0] != "/test/home" {
				t.Fatalf("child HOME entries=%v, want inherited HOME", got)
			}
			if got := environment["CODEX_TUI_DISABLE_KEYBOARD_ENHANCEMENT"]; len(got) != 1 || got[0] != "1" {
				t.Fatalf("child Codex adaptation entries=%v, want [1]", got)
			}
			waitLivePreflightStopped(t, statePath, "environment")
		})
	}
}

func TestMergeEnvironmentReplacesEveryDuplicateOverride(t *testing.T) {
	merged := mergeEnvironment(
		[]string{"EDITOR=vim", "PATH=/test/bin", "EDITOR=emacs", "HOME=/test/home"},
		map[string]string{"EDITOR": "nvim"},
	)
	environment := make(map[string][]string, len(merged))
	for _, entry := range merged {
		key, value, found := strings.Cut(entry, "=")
		if found {
			environment[key] = append(environment[key], value)
		}
	}
	if got := environment["EDITOR"]; len(got) != 1 || got[0] != "nvim" {
		t.Fatalf("EDITOR entries=%v, want exactly [nvim]", got)
	}
	if got := environment["PATH"]; len(got) != 1 || got[0] != "/test/bin" {
		t.Fatalf("PATH entries=%v, want preserved PATH", got)
	}
	if got := environment["HOME"]; len(got) != 1 || got[0] != "/test/home" {
		t.Fatalf("HOME entries=%v, want preserved HOME", got)
	}
}

func TestManagedChildEnvironmentHelper(t *testing.T) {
	path := os.Getenv("AGENTSCTL_CHILD_ENV_FILE")
	if path == "" {
		return
	}
	b, err := json.Marshal(os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func readChildEnvironment(t *testing.T, path string) map[string][]string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		var entries []string
		if err := json.Unmarshal(b, &entries); err != nil {
			t.Fatal(err)
		}
		environment := make(map[string][]string, len(entries))
		for _, entry := range entries {
			key, value, found := strings.Cut(entry, "=")
			if found {
				environment[key] = append(environment[key], value)
			}
		}
		return environment
	}
	t.Fatalf("managed child did not write environment to %s", path)
	return nil
}

func TestPeerIdentityComesFromUnixSocketCredentials(t *testing.T) {
	dir := shortTempDir(t)
	socket := filepath.Join(dir, "peer.sock")
	listener := listenUnixOrSkip(t, socket)
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr == nil {
			accepted <- conn
		}
	}()
	conn, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	serverConn := <-accepted
	defer serverConn.Close()
	identity, err := peerIdentity(conn)
	if err != nil {
		t.Fatal(err)
	}
	expected, err := processinfo.Observe(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if identity != expected {
		t.Fatalf("peer identity=%+v, want %+v", identity, expected)
	}
}

func TestLegacyRestartRefusesActiveManagedRun(t *testing.T) {
	dir := shortTempDir(t)
	socket := filepath.Join(dir, "supervisor.sock")
	listener := listenUnixOrSkip(t, socket)
	defer listener.Close()
	if err := os.Chmod(socket, 0o600); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(dir, "state.json")
	store := localstate.New(statePath)
	if err := store.StartRun(localstate.Run{ID: "active", State: "running"}); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	identity, err := processinfo.Observe(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	client := Client{Socket: socket, StatePath: statePath, DaemonPath: executable}
	err = client.restartLegacyOwned(context.Background(), identity)
	if err == nil || !strings.Contains(err.Error(), "active run") {
		t.Fatalf("restart error=%v, want active-run refusal", err)
	}
}

func TestLegacyRestartStopsVerifiedIdleDaemon(t *testing.T) {
	dir := shortTempDir(t)
	socket := filepath.Join(dir, "supervisor.sock")
	probe := listenUnixOrSkip(t, socket)
	_ = probe.Close()
	_ = os.Remove(socket)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(executable, "-test.run=^TestLegacyDaemonHelper$")
	command.Env = append(os.Environ(), "AGENTSCTL_LEGACY_HELPER=1", "AGENTSCTL_LEGACY_SOCKET="+socket)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() { waited <- command.Wait() }()
	t.Cleanup(func() {
		_ = command.Process.Kill()
		select {
		case <-waited:
		default:
		}
	})
	waitForSocket(t, socket, waited)
	conn, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := peerIdentity(conn)
	_ = conn.Close()
	if err != nil {
		t.Fatal(err)
	}
	client := Client{Socket: socket, StatePath: filepath.Join(dir, "state.json"), DaemonPath: executable}
	if err := client.restartLegacyOwned(context.Background(), identity); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-waited:
		if err != nil {
			t.Fatalf("legacy helper exit: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("legacy helper did not exit")
	}
}

func TestLegacyDaemonHelper(t *testing.T) {
	if os.Getenv("AGENTSCTL_LEGACY_HELPER") != "1" {
		return
	}
	socket := os.Getenv("AGENTSCTL_LEGACY_SOCKET")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(socket, 0o600); err != nil {
		t.Fatal(err)
	}
	terminated := make(chan os.Signal, 1)
	signal.Notify(terminated, syscall.SIGTERM)
	<-terminated
	_ = listener.Close()
}

func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "agentsctl-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func listenUnixOrSkip(t *testing.T, socket string) net.Listener {
	t.Helper()
	listener, err := net.Listen("unix", socket)
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "operation not permitted") {
		t.Skipf("sandbox does not permit Unix sockets: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	return listener
}

func waitForSocket(t *testing.T, socket string, done <-chan error) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-done:
			if err != nil && strings.Contains(strings.ToLower(err.Error()), "operation not permitted") {
				t.Skipf("sandbox does not permit Unix sockets: %v", err)
			}
			t.Fatalf("supervisor stopped before socket was ready: %v", err)
		default:
		}
		if conn, err := net.Dial("unix", socket); err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("supervisor socket did not become ready")
}

func TestSupervisorPTYHelper(t *testing.T) {
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
	}
}

// TestSupervisorPTYSignalHelper is a child process fixture (never run as a
// test on its own) modeling the installed Codex CLI's verified resize
// behavior: on SIGWINCH it re-reads the PTY size and only repaints
// ("WINCH\n" to stdout here) when that size actually differs from what it
// last saw, exactly as observed by attaching a live Codex session, changing
// nothing, and finding it does not repaint. A signal raised without a real
// size change (the naive fix this replaces) is therefore invisible to this
// helper, same as it was to real Codex -- only a genuine TIOCSWINSZ change
// makes it print.
func TestSupervisorPTYSignalHelper(t *testing.T) {
	winch := make(chan os.Signal, 8)
	signal.Notify(winch, syscall.SIGWINCH)
	lastCols, lastRows, _ := term.GetSize(int(os.Stdin.Fd()))
	go func() {
		for range winch {
			cols, rows, err := term.GetSize(int(os.Stdin.Fd()))
			if err == nil && (cols != lastCols || rows != lastRows) {
				lastCols, lastRows = cols, rows
				os.Stdout.WriteString("WINCH\n")
			}
		}
	}()
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
	}
}

func TestSupervisorPTYEditorLifecycleHelper(t *testing.T) {
	if os.Getenv("AGENTSCTL_TEST_EDITOR_LIFECYCLE") != "1" {
		return
	}
	previous, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	defer term.Restore(int(os.Stdin.Fd()), previous)

	winch := make(chan os.Signal, 8)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	lastCols, lastRows, _ := term.GetSize(int(os.Stdin.Fd()))
	go func() {
		for range winch {
			cols, rows, sizeErr := term.GetSize(int(os.Stdin.Fd()))
			if sizeErr == nil && (cols != lastCols || rows != lastRows) {
				lastCols, lastRows = cols, rows
				fmt.Fprintf(os.Stdout, "REDRAW %dx%d\n", rows, cols)
			}
		}
	}()
	input := make([]byte, 1)
	if _, err := os.Stdin.Read(input); err != nil || input[0] != 's' {
		return
	}
	foreground, _ := unix.IoctlGetInt(int(os.Stdin.Fd()), unix.TIOCGPGRP)
	fmt.Fprintf(os.Stdout, "TUI_READY %d %d\n", syscall.Getpgrp(), foreground)

	for {
		if _, err := os.Stdin.Read(input); err != nil {
			return
		}
		switch input[0] {
		case 'e':
			_, _ = os.Stdout.WriteString("\x1b[?2004l\x1b[?1004l")
			executable, executableErr := os.Executable()
			if executableErr != nil {
				t.Fatal(executableErr)
			}
			editor := exec.Command(executable, "-test.run=^TestSupervisorPTYEditorChildHelper$")
			editor.Stdin, editor.Stdout, editor.Stderr = os.Stdin, os.Stdout, os.Stderr
			if err := editor.Run(); err != nil {
				t.Fatal(err)
			}
			_, _ = os.Stdout.WriteString("\x1b[?2004h\x1b[?1004hTUI_RESUMED\n")
		case 'x':
			return
		}
	}
}

func TestSupervisorPTYEditorChildHelper(t *testing.T) {
	if os.Getenv("AGENTSCTL_TEST_EDITOR_LIFECYCLE") != "1" {
		return
	}
	foreground, _ := unix.IoctlGetInt(int(os.Stdin.Fd()), unix.TIOCGPGRP)
	_, _ = os.Stdout.WriteString("\x1b[?1049h\x1b[?2004h\x1b[?1004h")
	fmt.Fprintf(os.Stdout, "EDITOR_READY %d %d %d\n", syscall.Getpgrp(), foreground, os.Getppid())
	input := make([]byte, 1)
	_, _ = os.Stdin.Read(input)
	_, _ = os.Stdout.WriteString("\x1b[?2004l\x1b[?1004l\x1b[?1049lEDITOR_EXIT\n")
}

func TestSlowSubscriberIsDisconnectedBeforeOutputCanBeDropped(t *testing.T) {
	disconnected := make(chan struct{})
	sub := &subscriber{
		output:     make(chan []byte, subscriberBuffer),
		disconnect: func() { close(disconnected) },
	}
	p := &process{
		subscribers: map[*subscriber]struct{}{sub: {}},
		done:        make(chan struct{}),
	}
	for i := range subscriberBuffer {
		p.broadcast([]byte{byte(i)})
	}
	select {
	case <-disconnected:
		t.Fatal("subscriber disconnected before its bounded queue filled")
	default:
	}
	p.broadcast([]byte("overflow"))
	select {
	case <-disconnected:
	default:
		t.Fatal("full subscriber remained attached after output could not be queued")
	}
	p.mu.Lock()
	remaining := len(p.subscribers)
	p.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("subscribers=%d, want 0 after overflow", remaining)
	}
	for i := range subscriberBuffer {
		chunk, ok := <-sub.output
		if !ok {
			t.Fatalf("subscriber queue closed after %d chunks, want %d", i, subscriberBuffer)
		}
		if len(chunk) != 1 || chunk[0] != byte(i) {
			t.Fatalf("chunk %d=%v, want ordered byte %d", i, chunk, i)
		}
	}
	if _, ok := <-sub.output; ok {
		t.Fatal("subscriber queue remained open after disconnect")
	}
}

func TestManagedProcessExitEndsAttachAfterQueuedOutput(t *testing.T) {
	p := &process{
		run:         localstate.Run{ID: "r"},
		subscribers: map[*subscriber]struct{}{},
		done:        make(chan struct{}),
	}
	srv := &Server{runs: map[string]*process{"r": p}}
	client, server := net.Pipe()
	defer client.Close()
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		defer server.Close()
		srv.attach(server, "r")
	}()
	kind, _, err := protocol.Read(client)
	if err != nil || kind != protocol.Response {
		t.Fatalf("attach response kind=%q err=%v", kind, err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		p.mu.Lock()
		attached := len(p.subscribers) == 1
		p.mu.Unlock()
		if attached {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("server did not register attach subscriber")
		}
		time.Sleep(time.Millisecond)
	}
	p.broadcast([]byte("last output"))
	p.finishSubscribers()
	kind, data, err := protocol.Read(client)
	if err != nil || kind != protocol.Output || string(data) != "last output" {
		t.Fatalf("last output kind=%q data=%q err=%v", kind, data, err)
	}
	kind, _, err = protocol.Read(client)
	if err != nil || kind != protocol.Exit {
		t.Fatalf("exit kind=%q err=%v", kind, err)
	}
	_ = client.Close()
	select {
	case <-serverDone:
	case <-time.After(3 * time.Second):
		t.Fatal("server attach did not return after process exit")
	}
}

func TestAttachRaceWithProcessExitReturnsExit(t *testing.T) {
	done := make(chan struct{})
	close(done)
	p := &process{
		run:         localstate.Run{ID: "r"},
		subscribers: map[*subscriber]struct{}{},
		done:        done,
	}
	srv := &Server{runs: map[string]*process{"r": p}}
	client, server := net.Pipe()
	defer client.Close()
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		defer server.Close()
		srv.attach(server, "r")
	}()
	kind, _, err := protocol.Read(client)
	if err != nil || kind != protocol.Response {
		t.Fatalf("attach response kind=%q err=%v", kind, err)
	}
	kind, _, err = protocol.Read(client)
	if err != nil || kind != protocol.Exit {
		t.Fatalf("exit kind=%q err=%v", kind, err)
	}
	_ = client.Close()
	select {
	case <-serverDone:
	case <-time.After(3 * time.Second):
		t.Fatal("server attach did not return for an already-ended process")
	}
}

// TestReattachForcesRedrawEvenWhenSizeIsUnchanged fixes the root cause of the
// Codex reattach redraw bug: a manually raised SIGWINCH with no underlying
// PTY size change was verified against the installed Codex CLI to not
// reliably cause a repaint (Codex re-reads the size on the signal and skips
// redrawing when it sees no difference). syncPTYSize must instead bounce the
// PTY to a harmless alternate size and back, producing a genuine
// kernel-delivered SIGWINCH the child cannot distinguish from a real resize.
// This drives a real child through that exact path (same size, Redraw
// requested, as happens on reattach) and asserts the child actually
// receives a SIGWINCH.
func TestReattachForcesRedrawEvenWhenSizeIsUnchanged(t *testing.T) {
	dir := t.TempDir()
	st := localstate.New(filepath.Join(dir, "state.json"))
	srv := &Server{Store: st, runs: map[string]*process{}}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	srv.ResolveExecutable = func(string) (string, error) { return exe, nil }
	res := callServer(t, srv, Request{Action: "start", RunID: "r", SessionID: "thread", Args: []string{"-test.run=TestSupervisorPTYSignalHelper"}, Provider: "codex", CWD: dir})
	if !res.OK && strings.Contains(res.Error, "operation not permitted") {
		t.Skip("sandbox does not permit PTY process spawn")
	}
	if !res.OK {
		t.Fatal(res.Error)
	}

	p := srv.runs["r"]
	if p == nil {
		t.Fatal("run not tracked")
	}
	// The production drain() goroutine is the only reader of p.ptmx (a second
	// direct reader would race it for bytes), so observe output the same way
	// a real attach does: through a subscriber channel fed by that broadcast.
	sub := &subscriber{output: make(chan []byte, subscriberBuffer), disconnect: func() {}}
	if !p.addSubscriber(sub) {
		t.Fatal("live process rejected subscriber")
	}
	defer p.removeSubscriber(sub)

	// Establish a known starting size (mirrors the real attach flow, which
	// sends a Resize frame on connect) before exercising the same-size
	// reattach path below, and let the helper's signal.Notify land.
	syncPTYSize(p, protocol.TerminalSize{Rows: 24, Cols: 80})
	drainSubscriberFor(sub.output, 200*time.Millisecond) // let any WINCH from the initial size settle and discard it

	syncPTYSize(p, protocol.TerminalSize{Rows: 24, Cols: 80, Redraw: true})
	requireSubscriberContains(t, sub.output, "WINCH", 2*time.Second)
}

func TestEditorReturnForcesRedrawWithoutClientResize(t *testing.T) {
	dir := t.TempDir()
	store := localstate.New(filepath.Join(dir, "state.json"))
	server := &Server{Store: store, runs: map[string]*process{}}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	server.ResolveExecutable = func(string) (string, error) { return executable, nil }
	response := callServer(t, server, Request{
		Action: "start", RunID: "editor", SessionID: "thread", Provider: "codex", CWD: dir,
		Args:        []string{"-test.run=^TestSupervisorPTYEditorLifecycleHelper$"},
		Environment: map[string]string{"AGENTSCTL_TEST_EDITOR_LIFECYCLE": "1"},
	})
	if !response.OK && strings.Contains(response.Error, "operation not permitted") {
		t.Skip("sandbox does not permit PTY process spawn")
	}
	if !response.OK {
		t.Fatal(response.Error)
	}

	p := server.runs["editor"]
	if p == nil {
		t.Fatal("run not tracked")
	}
	sub := &subscriber{output: make(chan []byte, subscriberBuffer), disconnect: func() {}}
	if !p.addSubscriber(sub) {
		t.Fatal("live process rejected subscriber")
	}
	defer p.removeSubscriber(sub)
	var output strings.Builder
	if _, err := p.ptmx.Write([]byte{'s'}); err != nil {
		t.Fatal(err)
	}
	waitSubscriberText(t, sub.output, &output, "TUI_READY", 2*time.Second)

	syncPTYSize(p, protocol.TerminalSize{Rows: 24, Cols: 80})
	waitSubscriberText(t, sub.output, &output, "REDRAW 24x80", 2*time.Second)
	baselineRedraws := strings.Count(output.String(), "REDRAW ")

	for cycle := 1; cycle <= 2; cycle++ {
		if _, err := p.ptmx.Write([]byte{'e'}); err != nil {
			t.Fatal(err)
		}
		waitSubscriberCount(t, sub.output, &output, "EDITOR_READY", cycle, 2*time.Second)
		var editorPGRP, editorForeground, editorParent int
		if _, err := fmt.Sscanf(textAfterLast(output.String(), "EDITOR_READY "), "%d %d %d", &editorPGRP, &editorForeground, &editorParent); err != nil {
			t.Fatalf("parse editor process groups from %q: %v", output.String(), err)
		}
		managedPGRP, err := syscall.Getpgid(p.cmd.Process.Pid)
		if err != nil {
			t.Fatal(err)
		}
		if editorPGRP != managedPGRP || editorForeground != editorPGRP {
			t.Fatalf("editor pgrp=%d foreground=%d, want managed Codex pgrp", editorPGRP, editorForeground)
		}
		children := directChildren(p.cmd.Process.Pid)
		if editorParent != p.cmd.Process.Pid || len(children) != 1 {
			t.Fatalf("editor parent=%d, managed PID=%d, direct children=%v", editorParent, p.cmd.Process.Pid, children)
		}
		assertNoSubscriberText(t, sub.output, &output, "REDRAW ", baselineRedraws, 150*time.Millisecond)

		if _, err := p.ptmx.Write([]byte{'q'}); err != nil {
			t.Fatal(err)
		}
		waitSubscriberCount(t, sub.output, &output, "REDRAW ", baselineRedraws+2, 2*time.Second)
		waitSubscriberText(t, sub.output, &output, "REDRAW 24x80", 2*time.Second)
		current, err := pty.GetsizeFull(p.ptmx)
		if err != nil {
			t.Fatal(err)
		}
		if current.Rows != 24 || current.Cols != 80 {
			t.Fatalf("cycle %d final PTY size=%dx%d, want 24x80", cycle, current.Rows, current.Cols)
		}
		assertNoSubscriberText(t, sub.output, &output, "REDRAW ", baselineRedraws+2, 150*time.Millisecond)
		baselineRedraws += 2
	}

	if _, err := p.ptmx.Write([]byte{'x'}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.done:
	case <-time.After(2 * time.Second):
		t.Fatal("managed process did not exit")
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		server.mu.RLock()
		_, live := server.runs["editor"]
		server.mu.RUnlock()
		if !live {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("managed process was not cleaned up")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestExternalEditorRedrawDetector(t *testing.T) {
	childActive := false
	editor := processinfo.Identity{PID: 456, StartTime: 789, UID: 10}
	persistentChild := processinfo.Identity{PID: 123, StartTime: 456, UID: 10}
	detector := &externalEditorRedrawDetector{
		listDirectChildren: func() []processinfo.Identity {
			children := []processinfo.Identity{persistentChild}
			if childActive {
				children = append(children, editor)
			}
			return children
		},
	}
	detector.refreshKnownChildren()
	observeBytes := func(sequence string) bool {
		t.Helper()
		redraw := false
		for i := range len(sequence) {
			if detector.Observe([]byte{sequence[i]}) {
				redraw = true
			}
		}
		return redraw
	}

	// A failed launch restores Codex's screen without a direct child. It must
	// neither redraw nor leave stale state that can fire during the next editor.
	if observeBytes("\x1b[?2004l\x1b[?1049l\x1b[?1049h\x1b[?2004h") {
		t.Fatal("editor launch failure triggered redraw")
	}

	for cycle := 1; cycle <= 2; cycle++ {
		if observeBytes("\x1b[?2004l") {
			t.Fatalf("cycle %d Codex suspend triggered redraw", cycle)
		}
		childActive = true
		if observeBytes("\x1b[?1049h\x1b[?2004h") {
			t.Fatalf("cycle %d editor start triggered redraw", cycle)
		}
		if observeBytes("\x1b[?2004l\x1b[?1049l") {
			t.Fatalf("cycle %d editor exit modes triggered redraw before Codex resumed", cycle)
		}
		childActive = false
		if !observeBytes("\x1b[?1049h\x1b[?2004h") {
			t.Fatalf("cycle %d Codex resume did not trigger redraw", cycle)
		}
		if observeBytes("\x1b[?2004h") {
			t.Fatalf("cycle %d repeated Codex mode triggered redraw", cycle)
		}
	}
}

func TestExternalEditorRedrawDetectorIsCodexSpecific(t *testing.T) {
	if detector := newExternalEditorRedrawDetector("claude", 123); detector != nil {
		t.Fatal("Claude process received Codex editor redraw detector")
	}
	if detector := newExternalEditorRedrawDetector("codex", 123); detector == nil {
		t.Fatal("Codex process did not receive editor redraw detector")
	}
}

func waitSubscriberText(t *testing.T, sub <-chan []byte, output *strings.Builder, want string, timeout time.Duration) {
	t.Helper()
	waitSubscriber(t, sub, output, timeout, func(text string) bool { return strings.Contains(text, want) })
}

func waitSubscriberCount(t *testing.T, sub <-chan []byte, output *strings.Builder, want string, count int, timeout time.Duration) {
	t.Helper()
	waitSubscriber(t, sub, output, timeout, func(text string) bool { return strings.Count(text, want) >= count })
}

func waitSubscriber(t *testing.T, sub <-chan []byte, output *strings.Builder, timeout time.Duration, done func(string) bool) {
	t.Helper()
	if done(output.String()) {
		return
	}
	deadline := time.After(timeout)
	for {
		select {
		case chunk, ok := <-sub:
			if !ok {
				t.Fatalf("subscriber closed with output %q", output.String())
			}
			output.Write(chunk)
			if done(output.String()) {
				return
			}
		case <-deadline:
			t.Fatalf("timed out with output %q", output.String())
		}
	}
}

func assertNoSubscriberText(t *testing.T, sub <-chan []byte, output *strings.Builder, want string, count int, duration time.Duration) {
	t.Helper()
	deadline := time.After(duration)
	for {
		select {
		case chunk, ok := <-sub:
			if !ok {
				t.Fatalf("subscriber closed with output %q", output.String())
			}
			output.Write(chunk)
			if got := strings.Count(output.String(), want); got != count {
				t.Fatalf("%q count=%d during editor activity, want %d; output=%q", want, got, count, output.String())
			}
		case <-deadline:
			return
		}
	}
}

func textAfterLast(text, marker string) string {
	index := strings.LastIndex(text, marker)
	if index < 0 {
		return ""
	}
	return text[index+len(marker):]
}

func drainSubscriberFor(sub <-chan []byte, d time.Duration) {
	deadline := time.After(d)
	for {
		select {
		case <-sub:
		case <-deadline:
			return
		}
	}
}

func requireSubscriberContains(t *testing.T, sub <-chan []byte, want string, timeout time.Duration) {
	t.Helper()
	var collected []byte
	deadline := time.After(timeout)
	for {
		select {
		case chunk, ok := <-sub:
			if !ok {
				t.Fatalf("subscriber channel closed before seeing %q, got %q", want, collected)
			}
			collected = append(collected, chunk...)
			if strings.Contains(string(collected), want) {
				return
			}
		case <-deadline:
			t.Fatalf("expected output to contain %q, got %q", want, collected)
		}
	}
}

func TestSupervisorRestartMarksUnrecoverableRecordStale(t *testing.T) {
	store := localstate.New(filepath.Join(t.TempDir(), "state.json"))
	_ = store.StartRun(localstate.Run{ID: "r", State: "running", PID: 999})
	srv := Server{Store: store}
	if err := srv.markStale(); err != nil {
		t.Fatal(err)
	}
	runs, _ := store.Runs()
	if runs["r"].State != "stale" || runs["r"].Error == "" {
		t.Fatalf("run=%+v", runs["r"])
	}
}

func callServer(t *testing.T, srv *Server, req Request) Response {
	t.Helper()
	client, server := net.Pipe()
	go srv.handle(server)
	b, _ := json.Marshal(req)
	if err := protocol.Write(client, protocol.Request, b); err != nil {
		t.Fatal(err)
	}
	kind, b, err := protocol.Read(client)
	if err != nil || kind != protocol.Response {
		t.Fatalf("response: kind=%c err=%v", kind, err)
	}
	_ = client.Close()
	var res Response
	if err := json.Unmarshal(b, &res); err != nil {
		t.Fatal(err)
	}
	return res
}
