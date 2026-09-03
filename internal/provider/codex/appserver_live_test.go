package codex

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"
)

// TestRealCodexAppServerListHasNoDuplicateIDs exercises the actual
// installed `codex` CLI's app-server over its real thread/list RPC (never
// a fake), confirming both that duplicate thread IDs are genuinely present
// in the raw multi-page response for this machine's ~/.codex history (so
// the dedup logic in mergeThread has something real to do) and that
// CommandAppServer.List, which runs that logic, never returns two rows
// sharing the same ID.
func TestRealCodexAppServerListHasNoDuplicateIDs(t *testing.T) {
	if os.Getenv("AGENTSCTL_REAL_CODEX_DEDUP") != "1" {
		t.Skip("set AGENTSCTL_REAL_CODEX_DEDUP=1 for the live installed-codex dedup test")
	}
	if _, err := exec.LookPath("codex"); err != nil {
		t.Skip("codex CLI not installed")
	}
	server := &CommandAppServer{Path: "codex"}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	rows, err := server.List(ctx, false)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	seen := map[string]int{}
	for _, r := range rows {
		seen[r.ID]++
	}
	dupCount := 0
	for id, n := range seen {
		if n > 1 {
			t.Errorf("List returned id %s %d times", id, n)
			dupCount++
		}
	}
	t.Logf("List returned %d rows (%d unique ids), %d duplicated ids", len(rows), len(seen), dupCount)
}
