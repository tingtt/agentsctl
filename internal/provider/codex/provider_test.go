package codex

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/tingtt/agentsctl/internal/process"
	"github.com/tingtt/agentsctl/internal/session"
	"github.com/tingtt/agentsctl/internal/state"
)

type fakeAPI struct {
	rows       []Thread
	archived   string
	unarchived string
	renamed    string
}

func TestArchiveAndUnarchiveUseAppServerWithoutRuntimeStop(t *testing.T) {
	api := &fakeAPI{}
	store := state.New(filepath.Join(t.TempDir(), "state.json"))
	p := Provider{API: api, Store: store}
	key := session.Key{Provider: session.ProviderCodex, ID: "thread"}
	if err := p.Archive(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	if api.archived != "thread" {
		t.Fatal("native archive not called")
	}
	if err := p.Unarchive(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	if api.unarchived != "thread" {
		t.Fatal("native unarchive not called")
	}
}

func (f *fakeAPI) List(context.Context, bool) ([]Thread, error) {
	return append([]Thread(nil), f.rows...), nil
}
func (f *fakeAPI) Rename(_ context.Context, id, name string) error {
	f.renamed = id + ":" + name
	return nil
}
func (f *fakeAPI) Archive(_ context.Context, id string) error   { f.archived = id; return nil }
func (f *fakeAPI) Unarchive(_ context.Context, id string) error { f.unarchived = id; return nil }
func (f *fakeAPI) CodexHome() string                            { return "" }

func TestAmbiguousThreadBindingIsNeverGuessed(t *testing.T) {
	store := state.New(filepath.Join(t.TempDir(), "state.json"))
	started := time.Now()
	_ = store.Update(func(d *state.Data) error {
		d.Runs["r"] = state.Run{ID: "r", Provider: "codex", CWD: "/work", State: "running", StartedAt: started, Baseline: []string{"old"}}
		return nil
	})
	p := Provider{Store: store, API: &fakeAPI{}, WriterOwner: func(string, process.Identity) (bool, error) { return true, nil }}
	threads := []Thread{{ID: "new-1", CWD: "/work"}, {ID: "new-2", CWD: "/work"}}
	if err := p.reconcile(threads); err != nil {
		t.Fatal(err)
	}
	d, _ := store.Load()
	r := d.Runs["r"]
	if r.SessionID != "" || r.Error == "" {
		t.Fatalf("run=%+v", r)
	}
}
func TestUniqueThreadBindingPersistsAcrossClientRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store := state.New(path)
	_ = store.Update(func(d *state.Data) error {
		d.Runs["r"] = state.Run{ID: "r", Provider: "codex", CWD: "/work", State: "running", Baseline: []string{"old"}}
		return nil
	})
	p := Provider{Store: store, API: &fakeAPI{}, WriterOwner: func(string, process.Identity) (bool, error) { return true, nil }}
	if err := p.reconcile([]Thread{{ID: "new", CWD: "/work"}}); err != nil {
		t.Fatal(err)
	}
	reopened, _ := state.New(path).Load()
	if reopened.Runs["r"].SessionID != "new" {
		t.Fatalf("run=%+v", reopened.Runs["r"])
	}
}

func TestArchivedBoundRunDoesNotReappearAsUnbound(t *testing.T) {
	store := state.New(filepath.Join(t.TempDir(), "state.json"))
	_ = store.Update(func(d *state.Data) error {
		d.Runs["run"] = state.Run{ID: "run", Provider: "codex", SessionID: "archived-thread", CWD: "/work", State: "stopped"}
		return nil
	})
	p := Provider{Store: store, API: &fakeAPI{rows: nil}}
	rows, err := p.List(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("bound archived run leaked into active catalog: %+v", rows)
	}
}

func TestListUsesAppServerCreatedAtAndPreservesActivityMapping(t *testing.T) {
	store := state.New(filepath.Join(t.TempDir(), "state.json"))
	api := &fakeAPI{rows: []Thread{
		{ID: "working", CreatedAt: 100, UpdatedAt: 900, Status: ThreadStatus{Type: "active"}},
		{ID: "idle", CreatedAt: 200, UpdatedAt: 800, Status: ThreadStatus{Type: "idle"}},
		{ID: "unknown", CreatedAt: 300, UpdatedAt: 700, Status: ThreadStatus{Type: "future"}},
	}}
	p := Provider{API: api, Store: store}
	rows, err := p.List(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if !rows[0].CreatedAt.Equal(time.Unix(100, 0)) || rows[0].Activity != session.ActivityWorking {
		t.Fatalf("working=%+v", rows[0])
	}
	if rows[1].Activity != session.ActivityIdle || rows[2].Activity != session.ActivityUnknown {
		t.Fatalf("rows=%+v", rows)
	}
}

// TestCatalogNeverReceivesDuplicateCodexKeys is the end-to-end guarantee:
// a real CommandAppServer talking (over a real subprocess/JSON-RPC boundary)
// to a fake codex CLI that reports the same thread ID twice must still
// produce a session.Catalog with at most one row per session.Key.
func TestCatalogNeverReceivesDuplicateCodexKeys(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available for fake codex CLI")
	}
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test source")
	}
	fakePath := filepath.Join(filepath.Dir(file), "..", "..", "testkit", "fakecli", "codex")
	dir := t.TempDir()
	t.Setenv("AGENTSCTL_FAKE_DIR", dir)
	rows := []map[string]any{
		{"id": "solo", "name": "solo", "cwd": "/work", "createdAt": 1, "updatedAt": 1, "status": map[string]any{"type": "idle"}, "archived": false},
		{"id": "dup-thread", "name": "old-rollout", "cwd": "/work", "createdAt": 2, "updatedAt": 5, "status": map[string]any{"type": "idle"}, "archived": false},
		{"id": "dup-thread", "name": "new-rollout", "cwd": "/work", "createdAt": 2, "updatedAt": 500, "status": map[string]any{"type": "idle"}, "archived": false},
	}
	b, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "codex.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	store := state.New(filepath.Join(t.TempDir(), "state.json"))
	provider := &Provider{API: &CommandAppServer{Path: fakePath}, Store: store}
	catalog := session.Catalog{Providers: []session.Provider{provider}}
	snap := catalog.Load(context.Background(), session.Scope{CurrentDirectory: "/work", Directory: session.ScopeAll})
	if snap.Warnings[session.ProviderCodex] != nil {
		t.Fatalf("catalog warning: %v", snap.Warnings[session.ProviderCodex])
	}
	seen := map[session.Key]bool{}
	for _, row := range snap.Sessions {
		if seen[row.Key] {
			t.Fatalf("duplicate session.Key %v reached the catalog: %+v", row.Key, snap.Sessions)
		}
		seen[row.Key] = true
	}
	if len(snap.Sessions) != 2 {
		t.Fatalf("sessions=%+v, want 2 (solo + deduped dup-thread)", snap.Sessions)
	}
}

// TestFailedUnboundRunExposesArchiveNotStop fixes the capability shape for
// a local run that started but was never proven to any app-server thread
// (state.Run.SessionID == "") and reached a terminal state: it must be
// archivable (a local cleanup, see below) but not stoppable, and the
// startup error must not gate Archive — "why the run failed" and "whether
// this row can be archived" are unrelated (the error remains visible only
// as diagnostic Summary text).
func TestFailedUnboundRunExposesArchiveNotStop(t *testing.T) {
	store := state.New(filepath.Join(t.TempDir(), "state.json"))
	_ = store.Update(func(d *state.Data) error {
		d.Runs["r"] = state.Run{ID: "r", Provider: "codex", CWD: "/work", State: "failed", Error: "fork/exec /usr/local/bin/codex: operation not permitted"}
		return nil
	})
	p := Provider{Store: store, API: &fakeAPI{}}
	rows, err := p.List(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows=%+v", rows)
	}
	row := rows[0]
	if row.Name != "Unbound run" || row.Activity != session.ActivityFailed {
		t.Fatalf("row=%+v", row)
	}
	if row.Capabilities.Stop {
		t.Fatalf("unbound failed run must not be stoppable: %+v", row.Capabilities)
	}
	if !row.Capabilities.Archive {
		t.Fatalf("unbound failed run must be archivable: %+v", row.Capabilities)
	}
	if row.Capabilities.Reason != "" {
		t.Fatalf("startup error leaked into Capabilities.Reason (would show as an archive-unavailable reason): %q", row.Capabilities.Reason)
	}
}

// TestArchiveUnboundRunIsLocalCleanupNotThreadArchive is the core
// regression for the reported bug: archiving a failed "Unbound run" (whose
// Key.ID is agentsctl's own run ID, never a real Codex thread ID) must
// delete the local state.Store run and must NOT call the app-server's
// thread/archive — passing a non-thread ID to thread/archive would either
// error or, worse, silently no-op against an unrelated/nonexistent thread.
func TestArchiveUnboundRunIsLocalCleanupNotThreadArchive(t *testing.T) {
	store := state.New(filepath.Join(t.TempDir(), "state.json"))
	_ = store.Update(func(d *state.Data) error {
		d.Runs["r"] = state.Run{ID: "r", Provider: "codex", CWD: "/work", State: "failed", Error: "fork/exec: operation not permitted"}
		return nil
	})
	api := &fakeAPI{}
	p := Provider{Store: store, API: api}
	if err := p.Archive(context.Background(), session.Key{Provider: session.ProviderCodex, ID: "r"}); err != nil {
		t.Fatal(err)
	}
	if api.archived != "" {
		t.Fatalf("unbound run archive called app-server thread/archive with id=%q", api.archived)
	}
	d, _ := store.Load()
	if _, ok := d.Runs["r"]; ok {
		t.Fatal("unbound run was not removed from local state")
	}
	rows, err := p.List(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("archived unbound run still appears in List: %+v", rows)
	}
}

// TestArchiveDoesNotLocallyDeleteRunningOrStartingUnboundRun guards the
// "active/starting run を誤って消さない" requirement: local cleanup only
// ever applies to a run in a terminal state. A running/starting run's Key
// should never reach Archive() through the UI (List gives it no Archive
// capability), but Provider.Archive itself must not delete it either, as a
// second line of defense — it must fall through to the app-server call.
func TestArchiveDoesNotLocallyDeleteRunningOrStartingUnboundRun(t *testing.T) {
	for _, state_ := range []string{"running", "starting"} {
		t.Run(state_, func(t *testing.T) {
			store := state.New(filepath.Join(t.TempDir(), "state.json"))
			_ = store.Update(func(d *state.Data) error {
				d.Runs["r"] = state.Run{ID: "r", Provider: "codex", CWD: "/work", State: state_}
				return nil
			})
			api := &fakeAPI{}
			p := Provider{Store: store, API: api}
			_ = p.Archive(context.Background(), session.Key{Provider: session.ProviderCodex, ID: "r"})
			d, _ := store.Load()
			if _, ok := d.Runs["r"]; !ok {
				t.Fatal("running/starting unbound run was locally deleted by Archive")
			}
			if api.archived != "r" {
				t.Fatalf("expected fallthrough to app-server thread/archive with id=r, got archived=%q", api.archived)
			}
		})
	}
}

func TestCodexNativeStatusMapping(t *testing.T) {
	cases := []struct {
		name   string
		thread Thread
		want   session.Activity
	}{
		{name: "working", thread: Thread{Status: ThreadStatus{Type: "active"}}, want: session.ActivityWorking},
		{name: "needs input", thread: Thread{Status: ThreadStatus{Type: "needsInput"}}, want: session.ActivityNeedsInput},
		{name: "quota", thread: Thread{Status: ThreadStatus{Type: "active", ActiveFlags: []string{"rateLimit"}}}, want: session.ActivityWaitingQuota},
		{name: "idle", thread: Thread{Status: ThreadStatus{Type: "notLoaded"}}, want: session.ActivityIdle},
		{name: "completed", thread: Thread{Status: ThreadStatus{Type: "completed"}}, want: session.ActivityCompleted},
		{name: "failed", thread: Thread{Status: ThreadStatus{Type: "failed"}}, want: session.ActivityFailed},
		{name: "unknown", thread: Thread{Status: ThreadStatus{Type: "future"}}, want: session.ActivityUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := codexActivity(tc.thread); got != tc.want {
				t.Fatalf("activity=%s, want %s", got, tc.want)
			}
		})
	}
}
