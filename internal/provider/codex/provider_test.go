package codex

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/tingtt/agentsctl/internal/localstate"
	"github.com/tingtt/agentsctl/internal/process"
	"github.com/tingtt/agentsctl/internal/session"
	"github.com/tingtt/agentsctl/internal/sessionctl"
)

var errBoom = errors.New("boom")

type fakeAPI struct {
	rows       []Thread
	archived   string
	unarchived string
	renamed    string

	rateLimits    AccountRateLimits
	rateLimitsErr error
}

func TestArchiveAndUnarchiveUseAppServerWithoutRuntimeStop(t *testing.T) {
	api := &fakeAPI{}
	store := localstate.New(filepath.Join(t.TempDir(), "state.json"))
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

func durationMins(m int) *int { return &m }

// TestUsageClassifiesWindowsByDurationNotSlotPosition fixes the core
// review finding: which of FiveHour/Weekly a RateLimitWindow becomes must
// depend only on its own WindowDurationMins, never on whether it arrived
// as Primary or Secondary. Each subtest places the 300/10080-minute
// windows in a different Primary/Secondary arrangement and expects the
// same FiveHour/Weekly classification regardless.
func TestUsageClassifiesWindowsByDurationNotSlotPosition(t *testing.T) {
	resets5h := int64(1000)
	resetsWeek := int64(2000)
	cases := []struct {
		name      string
		primary   *RateLimitWindow
		secondary *RateLimitWindow
	}{
		{
			name:      "primary 300 / secondary 10080",
			primary:   &RateLimitWindow{UsedPercent: 42, ResetsAt: &resets5h, WindowDurationMins: durationMins(300)},
			secondary: &RateLimitWindow{UsedPercent: 7, ResetsAt: &resetsWeek, WindowDurationMins: durationMins(10080)},
		},
		{
			name:      "primary 10080 / secondary 300",
			primary:   &RateLimitWindow{UsedPercent: 7, ResetsAt: &resetsWeek, WindowDurationMins: durationMins(10080)},
			secondary: &RateLimitWindow{UsedPercent: 42, ResetsAt: &resets5h, WindowDurationMins: durationMins(300)},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := &fakeAPI{rateLimits: AccountRateLimits{RateLimits: RateLimitSnapshot{Primary: tc.primary, Secondary: tc.secondary}}}
			p := Provider{API: api}
			got, err := p.Usage(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if got.Provider != session.ProviderCodex {
				t.Fatalf("Provider=%v, want codex", got.Provider)
			}
			if got.FiveHour.State != session.UsageAvailable || got.FiveHour.Percent != 42 || got.FiveHour.Reset.Unix() != resets5h {
				t.Fatalf("FiveHour=%+v, want Available/42%%/reset %d regardless of slot", got.FiveHour, resets5h)
			}
			if got.Weekly.State != session.UsageAvailable || got.Weekly.Percent != 7 || got.Weekly.Reset.Unix() != resetsWeek {
				t.Fatalf("Weekly=%+v, want Available/7%%/reset %d regardless of slot", got.Weekly, resetsWeek)
			}
		})
	}
}

// TestUsageWeeklyOnlyAccountLeavesFiveHourUnavailable fixes an account that
// only has a weekly window active (Primary=10080, Secondary=nil): FiveHour
// must be unavailable, not guessed from the weekly reading or from
// Secondary's absence.
func TestUsageWeeklyOnlyAccountLeavesFiveHourUnavailable(t *testing.T) {
	resetsWeek := int64(2000)
	api := &fakeAPI{rateLimits: AccountRateLimits{RateLimits: RateLimitSnapshot{
		Primary:   &RateLimitWindow{UsedPercent: 7, ResetsAt: &resetsWeek, WindowDurationMins: durationMins(10080)},
		Secondary: nil,
	}}}
	p := Provider{API: api}
	got, err := p.Usage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.FiveHour.State == session.UsageAvailable {
		t.Fatalf("FiveHour=%+v, want Available=false when only a weekly window was reported", got.FiveHour)
	}
	if got.Weekly.State != session.UsageAvailable || got.Weekly.Percent != 7 || got.Weekly.Reset.Unix() != resetsWeek {
		t.Fatalf("Weekly=%+v, want Available/7%%/reset %d", got.Weekly, resetsWeek)
	}
}

// TestUsageUnknownWindowDurationIsIgnoredNotGuessed fixes fail-closed
// handling of a window whose WindowDurationMins doesn't match either known
// duration: it must not be guessed into FiveHour or Weekly, it must simply
// not classify.
func TestUsageUnknownWindowDurationIsIgnoredNotGuessed(t *testing.T) {
	resetsAt := int64(1000)
	api := &fakeAPI{rateLimits: AccountRateLimits{RateLimits: RateLimitSnapshot{
		Primary:   &RateLimitWindow{UsedPercent: 42, ResetsAt: &resetsAt, WindowDurationMins: durationMins(60)},
		Secondary: nil,
	}}}
	p := Provider{API: api}
	got, err := p.Usage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.FiveHour.State == session.UsageAvailable || got.Weekly.State == session.UsageAvailable {
		t.Fatalf("got=%+v, want both windows unavailable for an unrecognized 60-minute duration", got)
	}
}

// TestUsageNilWindowDurationIsIgnoredNotGuessed fixes fail-closed handling
// of a window with no WindowDurationMins at all (nil) -- it must not be
// assumed to be the 5h window (or any other), matching the "primary is not
// always 5h" contract.
func TestUsageNilWindowDurationIsIgnoredNotGuessed(t *testing.T) {
	resetsAt := int64(1000)
	api := &fakeAPI{rateLimits: AccountRateLimits{RateLimits: RateLimitSnapshot{
		Primary:   &RateLimitWindow{UsedPercent: 42, ResetsAt: &resetsAt, WindowDurationMins: nil},
		Secondary: nil,
	}}}
	p := Provider{API: api}
	got, err := p.Usage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.FiveHour.State == session.UsageAvailable || got.Weekly.State == session.UsageAvailable {
		t.Fatalf("got=%+v, want both windows unavailable for a nil WindowDurationMins", got)
	}
}

// TestUsageZeroPercentIsAvailableNotUnavailable fixes that a genuinely
// reported 0% utilization (ResetsAt present) renders as an available 0%
// reading, not as "unavailable" -- Available and Percent==0 are
// independent, never conflated.
func TestUsageZeroPercentIsAvailableNotUnavailable(t *testing.T) {
	resetsAt := int64(1000)
	api := &fakeAPI{rateLimits: AccountRateLimits{RateLimits: RateLimitSnapshot{
		Primary:   &RateLimitWindow{UsedPercent: 0, ResetsAt: &resetsAt, WindowDurationMins: durationMins(300)},
		Secondary: nil,
	}}}
	p := Provider{API: api}
	got, err := p.Usage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.FiveHour.State != session.UsageAvailable || got.FiveHour.Percent != 0 {
		t.Fatalf("FiveHour=%+v, want Available=true/Percent=0 for a genuinely reported 0%%", got.FiveHour)
	}
}

// TestUsageMissingResetIsUnavailableNotZero fixes that a window the
// backend reported without a reset time renders as unavailable rather than
// a false 0%, even though its duration is recognized.
func TestUsageMissingResetIsUnavailableNotZero(t *testing.T) {
	api := &fakeAPI{rateLimits: AccountRateLimits{RateLimits: RateLimitSnapshot{
		Primary:   nil,
		Secondary: &RateLimitWindow{UsedPercent: 0, ResetsAt: nil, WindowDurationMins: durationMins(10080)},
	}}}
	p := Provider{API: api}
	got, err := p.Usage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.FiveHour.State == session.UsageAvailable {
		t.Fatalf("FiveHour=%+v, want Available=false for a nil Primary window", got.FiveHour)
	}
	if got.Weekly.State == session.UsageAvailable {
		t.Fatalf("Weekly=%+v, want Available=false for a window with no resetsAt", got.Weekly)
	}
}

// TestUsagePropagatesAPIError fixes that a failed account/rateLimits/read
// call is surfaced as an error, not silently reported as empty usage.
func TestUsagePropagatesAPIError(t *testing.T) {
	api := &fakeAPI{rateLimitsErr: errBoom}
	p := Provider{API: api}
	if _, err := p.Usage(context.Background()); err == nil {
		t.Fatal("want an error when the app-server call fails")
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
func (f *fakeAPI) RateLimits(context.Context) (AccountRateLimits, error) {
	return f.rateLimits, f.rateLimitsErr
}

func TestAmbiguousThreadBindingIsNeverGuessed(t *testing.T) {
	store := localstate.New(filepath.Join(t.TempDir(), "state.json"))
	started := time.Now()
	if err := store.StartRun(localstate.Run{ID: "r", Provider: "codex", CWD: "/work", State: "running", StartedAt: started, Baseline: []string{"old"}}); err != nil {
		t.Fatal(err)
	}
	p := Provider{Store: store, API: &fakeAPI{}, WriterOwner: func(string, process.Identity) (bool, error) { return true, nil }}
	threads := []Thread{{ID: "new-1", CWD: "/work"}, {ID: "new-2", CWD: "/work"}}
	if err := p.reconcile(threads); err != nil {
		t.Fatal(err)
	}
	runs, _ := store.Runs()
	r := runs["r"]
	if r.SessionID != "" || r.Error == "" {
		t.Fatalf("run=%+v", r)
	}
}
func TestUniqueThreadBindingPersistsAcrossClientRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store := localstate.New(path)
	if err := store.StartRun(localstate.Run{ID: "r", Provider: "codex", CWD: "/work", State: "running", Baseline: []string{"old"}}); err != nil {
		t.Fatal(err)
	}
	p := Provider{Store: store, API: &fakeAPI{}, WriterOwner: func(string, process.Identity) (bool, error) { return true, nil }}
	if err := p.reconcile([]Thread{{ID: "new", CWD: "/work"}}); err != nil {
		t.Fatal(err)
	}
	reopened, _ := localstate.New(path).Runs()
	if reopened["r"].SessionID != "new" {
		t.Fatalf("run=%+v", reopened["r"])
	}
}

func TestArchivedBoundRunDoesNotReappearAsUnbound(t *testing.T) {
	store := localstate.New(filepath.Join(t.TempDir(), "state.json"))
	if err := store.StartRun(localstate.Run{ID: "run", Provider: "codex", SessionID: "archived-thread", CWD: "/work", State: "stopped"}); err != nil {
		t.Fatal(err)
	}
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
	store := localstate.New(filepath.Join(t.TempDir(), "state.json"))
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
// produce a catalog with at most one row per session.Key.
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
	store := localstate.New(filepath.Join(t.TempDir(), "state.json"))
	provider := &Provider{API: &CommandAppServer{Path: fakePath}, Store: store}
	controller := sessionctl.Controller{Providers: []sessionctl.Source{provider}}
	snap := controller.Load(context.Background(), session.Scope{CurrentDirectory: "/work", Directory: session.ScopeAll})
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

// TestFailedUnboundRunExposesArchiveNotStop fixes the action-availability
// shape for a local run that started but was never proven to any
// app-server thread (localstate.Run.SessionID == "") and reached a
// terminal state: it must be archivable (a local cleanup, see below) but
// not stoppable, and the startup error must not leak into the
// availability reason -- "why the run failed" and "whether this row can
// be archived" are unrelated (the error remains visible only as
// diagnostic Summary text).
func TestFailedUnboundRunExposesArchiveNotStop(t *testing.T) {
	store := localstate.New(filepath.Join(t.TempDir(), "state.json"))
	if err := store.StartRun(localstate.Run{ID: "r", Provider: "codex", CWD: "/work", State: "failed", Error: "fork/exec /usr/local/bin/codex: operation not permitted"}); err != nil {
		t.Fatal(err)
	}
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
	if row.Actions.Available(session.ActionStop) {
		t.Fatalf("unbound failed run must not be stoppable: %+v", row.Actions)
	}
	if !row.Actions.Available(session.ActionArchive) {
		t.Fatalf("unbound failed run must be archivable: %+v", row.Actions)
	}
	if row.Actions.Reason(session.ActionStop) == row.Summary {
		t.Fatalf("startup error leaked into the Stop-unavailable reason: %q", row.Actions.Reason(session.ActionStop))
	}
}

// TestArchiveUnboundRunIsLocalCleanupNotThreadArchive is the core
// regression for the reported bug: archiving a failed "Unbound run" (whose
// Key.ID is agentsctl's own run ID, never a real Codex thread ID) must
// delete the local run record and must NOT call the app-server's
// thread/archive — passing a non-thread ID to thread/archive would either
// error or, worse, silently no-op against an unrelated/nonexistent thread.
func TestArchiveUnboundRunIsLocalCleanupNotThreadArchive(t *testing.T) {
	store := localstate.New(filepath.Join(t.TempDir(), "state.json"))
	if err := store.StartRun(localstate.Run{ID: "r", Provider: "codex", CWD: "/work", State: "failed", Error: "fork/exec: operation not permitted"}); err != nil {
		t.Fatal(err)
	}
	api := &fakeAPI{}
	p := Provider{Store: store, API: api}
	if err := p.Archive(context.Background(), session.Key{Provider: session.ProviderCodex, ID: "r"}); err != nil {
		t.Fatal(err)
	}
	if api.archived != "" {
		t.Fatalf("unbound run archive called app-server thread/archive with id=%q", api.archived)
	}
	runs, _ := store.Runs()
	if _, ok := runs["r"]; ok {
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

// TestArchiveRejectsRunningOrStartingUnboundRun guards the "active/starting
// run を誤って消さない" requirement, fail-closed at the provider boundary
// (not just relying on the UI never offering Archive for such a row): a
// running/starting unbound run's Key.ID is agentsctl's own local run ID,
// never a Codex thread ID, so it must be neither deleted locally nor
// forwarded to the app-server's thread/archive under any circumstance —
// Provider.Archive must instead return an error.
func TestArchiveRejectsRunningOrStartingUnboundRun(t *testing.T) {
	for _, runState := range []string{"running", "starting"} {
		t.Run(runState, func(t *testing.T) {
			store := localstate.New(filepath.Join(t.TempDir(), "state.json"))
			if err := store.StartRun(localstate.Run{ID: "r", Provider: "codex", CWD: "/work", State: runState}); err != nil {
				t.Fatal(err)
			}
			api := &fakeAPI{}
			p := Provider{Store: store, API: api}
			err := p.Archive(context.Background(), session.Key{Provider: session.ProviderCodex, ID: "r"})
			if err == nil {
				t.Fatal("expected an error archiving an active unbound run, got nil")
			}
			runs, _ := store.Runs()
			if _, ok := runs["r"]; !ok {
				t.Fatal("running/starting unbound run was locally deleted by Archive")
			}
			if api.archived != "" {
				t.Fatalf("running/starting unbound run's local ID reached the app-server thread/archive: id=%q", api.archived)
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
