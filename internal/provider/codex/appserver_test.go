package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestRPCIgnoresNotificationsAndCorrelatesMachineResponse(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	rpc := &rpcClient{enc: json.NewEncoder(client), scan: bufio.NewScanner(client)}
	go func() {
		var req map[string]any
		_ = json.NewDecoder(server).Decode(&req)
		_, _ = server.Write([]byte("{\"method\":\"thread/updated\"}\n"))
		_, _ = server.Write([]byte("{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"data\":[]}}\n"))
	}()
	var out struct {
		Data []Thread `json:"data"`
	}
	if err := rpc.call(context.Background(), "thread/list", map[string]any{}, &out); err != nil {
		t.Fatal(err)
	}
}

func TestRPCRejectsMalformedResponseStream(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	rpc := &rpcClient{enc: json.NewEncoder(client), scan: bufio.NewScanner(client)}
	go func() {
		var req map[string]any
		_ = json.NewDecoder(server).Decode(&req)
		_, _ = server.Write([]byte("not-json\n"))
		_ = server.Close()
	}()
	if err := rpc.call(context.Background(), "thread/list", map[string]any{}, nil); err == nil {
		t.Fatal("malformed response stream accepted")
	}
}

// TestMergeThreadKeepsNewestUpdatedAtDeterministically models the real
// codex app-server behaviour observed against the installed CLI: the same
// thread ID can appear more than once in a thread/list response (a resumed
// session gets a new rollout file under the same thread ID), with
// different UpdatedAt values. mergeThread is what CommandAppServer.List
// uses to collapse those into a single canonical row per ID.
func TestMergeThreadKeepsNewestUpdatedAtDeterministically(t *testing.T) {
	byID := map[string]Thread{}
	var order []string
	mergeThread(byID, &order, Thread{ID: "a", UpdatedAt: 10})
	mergeThread(byID, &order, Thread{ID: "b", UpdatedAt: 5})
	mergeThread(byID, &order, Thread{ID: "a", UpdatedAt: 50}) // newer duplicate of "a" replaces it
	mergeThread(byID, &order, Thread{ID: "a", UpdatedAt: 20}) // older duplicate of "a" is ignored
	if len(order) != 2 || order[0] != "a" || order[1] != "b" {
		t.Fatalf("order=%v", order)
	}
	if byID["a"].UpdatedAt != 50 {
		t.Fatalf("canonical row for duplicate ID: %+v", byID["a"])
	}
}

func fakeCodexPath(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available for fake codex CLI")
	}
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test source")
	}
	return filepath.Join(filepath.Dir(file), "..", "..", "testkit", "fakecli", "codex")
}

func writeFakeCodexCatalog(t *testing.T, dir string, rows []map[string]any) {
	t.Helper()
	b, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "codex.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestCommandAppServerListDedupesWithinASinglePage exercises the real
// subprocess/JSON-RPC boundary (the actual "codex provider / app-server"
// boundary, not a Go interface fake): CommandAppServer.List talks to the
// fake codex CLI exactly as it would talk to the real `codex app-server
// --stdio`, and two catalog rows sharing an ID inside one thread/list page
// must still collapse to a single row, keeping the newer UpdatedAt.
func TestCommandAppServerListDedupesWithinASinglePage(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENTSCTL_FAKE_DIR", dir)
	writeFakeCodexCatalog(t, dir, []map[string]any{
		{"id": "solo", "name": "solo", "cwd": "/work", "createdAt": 1, "updatedAt": 1, "status": map[string]any{"type": "idle"}, "archived": false},
		{"id": "dup-thread", "name": "old-rollout", "cwd": "/work", "createdAt": 2, "updatedAt": 5, "status": map[string]any{"type": "idle"}, "archived": false},
		{"id": "dup-thread", "name": "new-rollout", "cwd": "/work", "createdAt": 2, "updatedAt": 500, "status": map[string]any{"type": "idle"}, "archived": false},
	})
	api := &CommandAppServer{Path: fakeCodexPath(t)}
	threads, err := api.List(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]Thread{}
	for _, thr := range threads {
		if _, dup := byID[thr.ID]; dup {
			t.Fatalf("duplicate thread ID %q in result: %+v", thr.ID, threads)
		}
		byID[thr.ID] = thr
	}
	if len(threads) != 2 {
		t.Fatalf("threads=%+v, want 2 rows", threads)
	}
	got := byID["dup-thread"]
	if got.UpdatedAt != 500 || value(got.Name) != "new-rollout" {
		t.Fatalf("canonical duplicate row=%+v, want the newest UpdatedAt copy", got)
	}
}

// TestCommandAppServerListDedupesAcrossPaginationBoundary places a
// duplicate thread ID on either side of the app-server's page boundary
// (List requests limit=100) to prove the pagination-merging loop in
// CommandAppServer.List also collapses cross-page duplicates, not just
// same-page ones.
func TestCommandAppServerListDedupesAcrossPaginationBoundary(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENTSCTL_FAKE_DIR", dir)
	rows := make([]map[string]any, 0, 101)
	for i := 0; i < 99; i++ {
		id := fmt.Sprintf("solo-%03d", i)
		rows = append(rows, map[string]any{"id": id, "name": id, "cwd": "/work", "createdAt": i, "updatedAt": i, "status": map[string]any{"type": "idle"}, "archived": false})
	}
	// Index 99 is the last row of page 1 (limit=100); index 100 is the
	// first row of page 2. Both share an ID.
	rows = append(rows,
		map[string]any{"id": "dup-thread", "name": "page1-copy", "cwd": "/work", "createdAt": 100, "updatedAt": 100, "status": map[string]any{"type": "idle"}, "archived": false},
		map[string]any{"id": "dup-thread", "name": "page2-copy", "cwd": "/work", "createdAt": 100, "updatedAt": 900, "status": map[string]any{"type": "idle"}, "archived": false},
	)
	writeFakeCodexCatalog(t, dir, rows)
	api := &CommandAppServer{Path: fakeCodexPath(t)}
	threads, err := api.List(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(threads) != 100 {
		t.Fatalf("threads=%d, want 100 unique rows (99 solo + 1 deduped)", len(threads))
	}
	seen := map[string]bool{}
	var canonical Thread
	for _, thr := range threads {
		if seen[thr.ID] {
			t.Fatalf("duplicate thread ID %q survived pagination merge", thr.ID)
		}
		seen[thr.ID] = true
		if thr.ID == "dup-thread" {
			canonical = thr
		}
	}
	if canonical.UpdatedAt != 900 || value(canonical.Name) != "page2-copy" {
		t.Fatalf("canonical cross-page row=%+v, want the newest UpdatedAt copy", canonical)
	}
}
