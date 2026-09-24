package codex

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func dialFake(t *testing.T, d *fakeDaemon, onNotify func(string, json.RawMessage)) *rpcConn {
	t.Helper()
	if onNotify == nil {
		onNotify = func(string, json.RawMessage) {}
	}
	conn, err := dialRPC(context.Background(), d.socket, onNotify)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(conn.Close)
	return conn
}

func TestRPCConnMatchesConcurrentResponsesAndRoutesNotifications(t *testing.T) {
	d := newFakeDaemon(t)
	d.setThreads(catalogThread("a", 1), catalogThread("b", 2))
	var mu sync.Mutex
	var notes []string
	conn := dialFake(t, d, func(method string, _ json.RawMessage) {
		mu.Lock()
		notes = append(notes, method)
		mu.Unlock()
	})
	if err := initializeRPC(context.Background(), conn.call, conn.notify, nil); err != nil {
		t.Fatal(err)
	}
	c := d.waitReady(t)

	var wg sync.WaitGroup
	for _, id := range []string{"a", "b", "a", "b", "a", "b"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := readThread(context.Background(), conn, id)
			if err != nil || got.ID != id {
				t.Errorf("read %s = %+v, %v", id, got, err)
			}
		}()
	}
	wg.Wait()

	// Malformed frames and a server request are skipped; the notification
	// behind them still arrives and the connection stays usable.
	_ = c.ws.Write(context.Background(), websocket.MessageText, []byte("not json"))
	c.send(map[string]any{"jsonrpc": "2.0", "id": 99, "method": "item/commandExecution/requestApproval", "params": map[string]any{}})
	c.notify("future/notification", map[string]any{"x": 1})
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(notes)
		mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("notifications=%v", notes)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := readThread(context.Background(), conn, "a"); err != nil {
		t.Fatal(err)
	}
	if d.callCount("item/commandExecution/requestApproval") != 0 {
		t.Fatal("a server request must never be answered")
	}
}

func TestRPCConnCallCancellationKeepsConnection(t *testing.T) {
	d := newFakeDaemon(t)
	d.setThreads(catalogThread("a", 1))
	release := make(chan struct{})
	d.beforeRead = func(_ *fakeConn, id string) *ThreadStatus {
		if id == "slow" {
			<-release
		}
		return nil
	}
	defer close(release)
	conn := dialFake(t, d, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := readThread(ctx, conn, "slow"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v", err)
	}
	select {
	case <-conn.Done():
		t.Fatalf("a cancelled call must not end the connection: %v", conn.Err())
	default:
	}
}

func TestRPCConnCloseReleasesPendingCalls(t *testing.T) {
	d := newFakeDaemon(t)
	release := make(chan struct{})
	defer close(release)
	d.beforeRead = func(*fakeConn, string) *ThreadStatus { <-release; return nil }
	conn := dialFake(t, d, nil)

	errc := make(chan error, 1)
	go func() {
		_, err := readThread(context.Background(), conn, "a")
		errc <- err
	}()
	time.Sleep(20 * time.Millisecond)
	conn.Close()
	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("pending call must fail once the connection is closed")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pending call was not released")
	}
}

func TestRPCConnReportsServerDisconnect(t *testing.T) {
	d := newFakeDaemon(t)
	conn := dialFake(t, d, nil)
	d.dropAll()
	select {
	case <-conn.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("disconnect not detected")
	}
	if conn.Err() == nil {
		t.Fatal("a closed connection must report why")
	}
	if _, err := readThread(context.Background(), conn, "a"); err == nil {
		t.Fatal("call on a closed connection must fail")
	}
}

func TestResolveCodexHome(t *testing.T) {
	userHome := func() (string, error) { return "/home/u", nil }
	env := func(v string) func(string) string {
		return func(key string) string {
			if key == "CODEX_HOME" {
				return v
			}
			return ""
		}
	}

	got, err := resolveCodexHome(env(""), userHome)
	if err != nil || got != "/home/u/.codex" {
		t.Fatalf("default = %q, %v", got, err)
	}
	if controlSocketPath(got) != "/home/u/.codex/app-server-control/app-server-control.sock" {
		t.Fatalf("socket=%s", controlSocketPath(got))
	}

	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.EvalSymlinks(real)
	if got, err := resolveCodexHome(env(link), userHome); err != nil || got != want {
		t.Fatalf("CODEX_HOME symlink = %q, %v; want canonical %q", got, err, want)
	}

	if _, err := resolveCodexHome(env(filepath.Join(dir, "missing")), userHome); err == nil {
		t.Fatal("a missing CODEX_HOME must be an error, not a fallback")
	}
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveCodexHome(env(file), userHome); err == nil {
		t.Fatal("a CODEX_HOME that is not a directory must be an error")
	}
}
