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
