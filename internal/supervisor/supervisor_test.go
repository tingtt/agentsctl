//go:build darwin || linux

package supervisor

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	processinfo "github.com/tingtt/agentsctl/internal/process"
	"github.com/tingtt/agentsctl/internal/protocol"
	"github.com/tingtt/agentsctl/internal/state"
	"golang.org/x/term"
)

func TestDetachDoesNotStopManagedChild(t *testing.T) {
	dir := t.TempDir()
	st := state.New(filepath.Join(dir, "state.json"))
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
	d, _ := st.Load()
	if d.Runs["r"].State != "running" {
		t.Fatalf("state=%s", d.Runs["r"].State)
	}
	stop := callServer(t, srv, Request{Action: "stop", RunID: "r"})
	if !stop.OK {
		t.Fatal(stop.Error)
	}
	d, err = st.Load()
	if err != nil || d.Runs["r"].SessionID != "thread" {
		t.Fatalf("stopped run lost session binding: run=%+v err=%v", d.Runs["r"], err)
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
	store := state.New(filepath.Join(root, "state.json"))
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
	store := state.New(filepath.Join(dir, "state.json"))
	server := &Server{Store: store, runs: map[string]*process{}, ResolveExecutable: func(string) (string, error) {
		return filepath.Join(dir, "missing"), nil
	}}
	response := callServer(t, server, Request{Action: "start", RunID: "failed", Provider: "codex", CWD: dir})
	if response.OK || !strings.Contains(response.Error, "PTY process spawn") {
		t.Fatalf("response=%+v", response)
	}
	data, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := data.Runs["failed"]; exists {
		t.Fatal("failed executable remained as a started run")
	}
}

func TestHandshakeReportsProtocolBuildAndDaemonIdentity(t *testing.T) {
	server := &Server{Store: state.New(filepath.Join(t.TempDir(), "state.json"))}
	response := callServer(t, server, Request{Action: "ping"})
	if !response.OK || response.ProtocolVersion != ProtocolVersion || response.BuildVersion != BuildVersion || response.DaemonPID != os.Getpid() || response.DaemonStartTime == 0 {
		t.Fatalf("handshake=%+v", response)
	}
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
	store := state.New(statePath)
	if err := store.Update(func(data *state.Data) error {
		data.Runs["active"] = state.Run{ID: "active", State: "running"}
		return nil
	}); err != nil {
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
	st := state.New(filepath.Join(dir, "state.json"))
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
	sub := make(chan []byte, 64)
	p.mu.Lock()
	p.subscribers[sub] = struct{}{}
	p.mu.Unlock()
	defer func() { p.mu.Lock(); delete(p.subscribers, sub); p.mu.Unlock() }()

	// Establish a known starting size (mirrors the real attach flow, which
	// sends a Resize frame on connect) before exercising the same-size
	// reattach path below, and let the helper's signal.Notify land.
	syncPTYSize(p, protocol.TerminalSize{Rows: 24, Cols: 80})
	drainSubscriberFor(sub, 200*time.Millisecond) // let any WINCH from the initial size settle and discard it

	syncPTYSize(p, protocol.TerminalSize{Rows: 24, Cols: 80, Redraw: true})
	requireSubscriberContains(t, sub, "WINCH", 2*time.Second)
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
	store := state.New(filepath.Join(t.TempDir(), "state.json"))
	_ = store.Update(func(d *state.Data) error { d.Runs["r"] = state.Run{ID: "r", State: "running", PID: 999}; return nil })
	srv := Server{Store: store}
	if err := srv.markStale(); err != nil {
		t.Fatal(err)
	}
	d, _ := store.Load()
	if d.Runs["r"].State != "stale" || d.Runs["r"].Error == "" {
		t.Fatalf("run=%+v", d.Runs["r"])
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
