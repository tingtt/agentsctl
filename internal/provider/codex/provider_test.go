package codex

import (
	"context"
	"path/filepath"
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
	p := Provider{API: api}
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
