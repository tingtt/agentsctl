//go:build darwin || linux

package supervisor

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/tingtt/agentsctl/internal/localstate"
	"github.com/tingtt/agentsctl/internal/supervisor/protocol"
)

// recordingSupervisor answers a single start request with a run and hands
// the decoded request back to the test.
func recordingSupervisor(t *testing.T) (Client, <-chan Request) {
	t.Helper()
	socket := filepath.Join(shortTempDir(t), "s.sock")
	listener := listenUnixOrSkip(t, socket)
	t.Cleanup(func() { _ = listener.Close() })
	requests := make(chan Request, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, b, err := protocol.Read(conn)
		if err != nil {
			return
		}
		var req Request
		if err := json.Unmarshal(b, &req); err != nil {
			return
		}
		requests <- req
		run := localstate.Run{ID: req.RunID, Provider: req.Provider, SessionID: req.SessionID, CWD: req.CWD}
		res, _ := json.Marshal(Response{OK: true, Run: &run})
		_ = protocol.Write(conn, protocol.Response, res)
	}()
	return Client{Socket: socket}, requests
}

func TestDispatchStartsCodexWithoutDaemon(t *testing.T) {
	client, requests := recordingSupervisor(t)
	baseline := []string{"thread-a"}
	environment := map[string]string{"CODEX_HOME": "/codex"}
	run, err := Dispatcher{Client: client}.Dispatch(context.Background(), "hello", "/work", baseline, environment)
	if err != nil {
		t.Fatal(err)
	}
	req := <-requests
	if want := []string{"--no-daemon", "hello"}; !reflect.DeepEqual(req.Args, want) {
		t.Fatalf("Args = %q, want %q", req.Args, want)
	}
	if req.Action != "start" || req.Provider != "codex" || req.CWD != "/work" || req.SessionID != "" {
		t.Fatalf("request = %+v", req)
	}
	if !reflect.DeepEqual(req.Baseline, baseline) || !reflect.DeepEqual(req.Environment, environment) {
		t.Fatalf("baseline/environment = %q/%q", req.Baseline, req.Environment)
	}
	if !strings.HasPrefix(req.RunID, "run-") || run.ID != req.RunID {
		t.Fatalf("RunID = %q, returned run %q", req.RunID, run.ID)
	}
}

func TestResumeStartsCodexWithoutDaemon(t *testing.T) {
	environment := map[string]string{"CODEX_HOME": "/codex"}
	check := func(t *testing.T, req Request) {
		t.Helper()
		if want := []string{"--no-daemon", "resume", "thread-1"}; !reflect.DeepEqual(req.Args, want) {
			t.Fatalf("Args = %q, want %q", req.Args, want)
		}
		if req.Action != "start" || req.Provider != "codex" || req.SessionID != "thread-1" || req.CWD != "/work" {
			t.Fatalf("request = %+v", req)
		}
		if !reflect.DeepEqual(req.Environment, environment) {
			t.Fatalf("Environment = %q", req.Environment)
		}
	}

	t.Run("supplied run ID", func(t *testing.T) {
		client, requests := recordingSupervisor(t)
		if _, err := (Dispatcher{Client: client}).Resume(context.Background(), "run-given", "thread-1", "/work", environment); err != nil {
			t.Fatal(err)
		}
		req := <-requests
		check(t, req)
		if req.RunID != "run-given" {
			t.Fatalf("RunID = %q", req.RunID)
		}
	})
	t.Run("generated run ID", func(t *testing.T) {
		client, requests := recordingSupervisor(t)
		if _, err := (Dispatcher{Client: client}).ResumeExisting(context.Background(), "thread-1", "/work", environment); err != nil {
			t.Fatal(err)
		}
		req := <-requests
		check(t, req)
		if !strings.HasPrefix(req.RunID, "run-") {
			t.Fatalf("RunID = %q", req.RunID)
		}
	})
}
