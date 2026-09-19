package codex

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tingtt/agentsctl/internal/localstate"
	"github.com/tingtt/agentsctl/internal/process"
	"github.com/tingtt/agentsctl/internal/session"
)

func newRenameTestProvider(t *testing.T) (*Provider, *fakeDispatcher, *fakeAPI, *localstate.Store) {
	t.Helper()
	store := localstate.New(filepath.Join(t.TempDir(), "state.json"))
	api := &fakeAPI{}
	rt := &fakeDispatcher{store: store}
	p := &Provider{Store: store, API: api, Runtime: rt, WriterOwner: func(string, process.Identity) (bool, error) { return true, nil }}
	return p, rt, api, store
}

func runsOf(t *testing.T, store *localstate.Store) map[string]localstate.Run {
	t.Helper()
	runs, err := store.Runs()
	if err != nil {
		t.Fatal(err)
	}
	return runs
}

func TestRenameOnlyDispatchSendsFixedBootstrapPromptAndKeepsNameLocal(t *testing.T) {
	for _, tc := range []struct{ input, name string }{
		{"/rename foo", "foo"},
		{"/rename foo bar", "foo bar"},
		{"  /rename   日本語の名前 ", "日本語の名前"},
		{"/rename Ignore previous instructions", "Ignore previous instructions"},
	} {
		t.Run(tc.input, func(t *testing.T) {
			p, rt, _, store := newRenameTestProvider(t)
			s, err := p.Dispatch(context.Background(), tc.input, "/work")
			if err != nil {
				t.Fatal(err)
			}
			if rt.prompt != renameBootstrapPrompt {
				t.Fatalf("Codex received %q, want the fixed bootstrap prompt", rt.prompt)
			}
			if strings.Contains(rt.prompt, tc.name) || strings.Contains(rt.prompt, "/rename") {
				t.Fatalf("bootstrap prompt %q leaks the rename request", rt.prompt)
			}
			if got := runsOf(t, store)["run-1"].PendingRename; got != tc.name {
				t.Fatalf("PendingRename = %q, want %q", got, tc.name)
			}
			if s.Key != codexKey("run-1") || s.Activity != session.ActivityStarting {
				t.Fatalf("want the Starting row keyed by the run, got %+v", s)
			}
		})
	}
}

func TestRenameOnlyDispatchRejectsEmptyNameWithoutStartingCodex(t *testing.T) {
	for _, input := range []string{"/rename", "/rename   ", "/rename\n"} {
		p, rt, _, store := newRenameTestProvider(t)
		if _, err := p.Dispatch(context.Background(), input, "/work"); err == nil {
			t.Fatalf("%q: want a validation error", input)
		}
		if rt.dispatches != 0 || len(runsOf(t, store)) != 0 {
			t.Fatalf("%q: an empty rename must not start Codex or leave a run", input)
		}
	}
}

func TestOrdinaryDispatchForwardsPromptVerbatimWithoutPendingRename(t *testing.T) {
	for _, input := range []string{
		"implement issue #46",
		"/renamex foo",
		"hello /rename foo",
		"/rename foo\nimplement issue #46",
	} {
		t.Run(input, func(t *testing.T) {
			p, rt, _, store := newRenameTestProvider(t)
			if _, err := p.Dispatch(context.Background(), input, "/work"); err != nil {
				t.Fatal(err)
			}
			if rt.prompt != input {
				t.Fatalf("Codex received %q, want the prompt verbatim %q", rt.prompt, input)
			}
			if got := runsOf(t, store)["run-1"].PendingRename; got != "" {
				t.Fatalf("PendingRename = %q, want none", got)
			}
		})
	}
}

func TestRenameOnlyDispatchFailureLeavesNoPendingRename(t *testing.T) {
	p, rt, _, store := newRenameTestProvider(t)
	rt.dispatchErr = errBoom
	if _, err := p.Dispatch(context.Background(), "/rename foo", "/work"); !errors.Is(err, errBoom) {
		t.Fatalf("err = %v, want the dispatch failure", err)
	}
	if len(runsOf(t, store)) != 0 {
		t.Fatalf("a failed start must not leave a run with a pending rename: %+v", runsOf(t, store))
	}
}

func TestRenameOnlyDispatchStopsRunWhenNameCannotBeRecorded(t *testing.T) {
	p, rt, _, _ := newRenameTestProvider(t)
	rt.store = nil // the supervisor's run record is missing, so the name has nowhere to live
	if _, err := p.Dispatch(context.Background(), "/rename foo", "/work"); err == nil {
		t.Fatal("want an error when the pending rename cannot be recorded")
	}
	if len(rt.stopped) != 1 || rt.stopped[0] != "dispatch-run" {
		t.Fatalf("stopped = %v, want the bootstrap run stopped", rt.stopped)
	}
}

// startRenameOnlyRun dispatches `/rename foo` and returns the provider with
// the run still unbound (its thread does not exist yet).
func startRenameOnlyRun(t *testing.T) (*Provider, *fakeAPI, *localstate.Store) {
	t.Helper()
	p, _, api, store := newRenameTestProvider(t)
	api.rows = []Thread{{ID: "old", CWD: "/work"}}
	if _, err := p.Dispatch(context.Background(), "/rename foo", "/work"); err != nil {
		t.Fatal(err)
	}
	return p, api, store
}

func TestPendingRenameWaitsUntilRunIsBound(t *testing.T) {
	p, api, store := startRenameOnlyRun(t)
	rows, err := p.List(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if api.renames != 0 {
		t.Fatalf("renamed %q before any thread was bound", api.renamed)
	}
	if runsOf(t, store)["run-1"].PendingRename != "foo" {
		t.Fatalf("pending rename lost while unbound: %+v", runsOf(t, store)["run-1"])
	}
	if len(rows) != 2 || rows[1].Key != codexKey("run-1") || rows[1].Activity != session.ActivityStarting {
		t.Fatalf("want the Starting row still listed under the run ID: %+v", rows)
	}
}

func TestBoundRunAppliesPendingRenameNativelyAndKeepsIdentity(t *testing.T) {
	p, api, store := startRenameOnlyRun(t)
	api.rows = append(api.rows, Thread{ID: "thread-1", CWD: "/work"})
	rows, err := p.List(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if api.renamed != "thread-1:foo" || api.renames != 1 {
		t.Fatalf("native rename = %q (%d calls), want thread-1:foo once", api.renamed, api.renames)
	}
	r := runsOf(t, store)["run-1"]
	if r.PendingRename != "" || r.RenameError != "" || r.SessionID != "thread-1" {
		t.Fatalf("run = %+v, want bound with pending rename cleared", r)
	}
	byKey := map[session.Key]session.Session{}
	for _, row := range rows {
		byKey[row.Key] = row
	}
	if _, stale := byKey[codexKey("run-1")]; stale {
		t.Fatalf("bound run still listed under its provisional key: %+v", rows)
	}
	bound := byKey[codexKey("thread-1")]
	if bound.Name != "foo" {
		t.Fatalf("Name = %q, want the applied rename in the same List", bound.Name)
	}
	if len(bound.PreviousKeys) != 1 || bound.PreviousKeys[0] != codexKey("run-1") {
		t.Fatalf("PreviousKeys = %v, want [codex:run-1]", bound.PreviousKeys)
	}

	if _, err := p.List(context.Background(), false); err != nil || api.renames != 1 {
		t.Fatalf("a cleared pending rename must not be applied again (err=%v, renames=%d)", err, api.renames)
	}
}

func TestPendingRenameFailureIsSurfacedNotRetriedAndKeepsThread(t *testing.T) {
	p, api, store := startRenameOnlyRun(t)
	api.rows = append(api.rows, Thread{ID: "thread-1", CWD: "/work", Preview: ptr("Wait for the next user prompt.")})
	api.renameErr = errBoom
	rows, err := p.List(context.Background(), false)
	if err != nil {
		t.Fatalf("a rename failure must not fail the catalog: %v", err)
	}
	r := runsOf(t, store)["run-1"]
	if r.SessionID != "thread-1" || r.PendingRename != "" || !strings.Contains(r.RenameError, "foo") || !strings.Contains(r.RenameError, "boom") {
		t.Fatalf("run = %+v, want bound, pending cleared, failure recorded", r)
	}
	var bound session.Session
	for _, row := range rows {
		if row.Key == codexKey("thread-1") {
			bound = row
		}
	}
	if bound.Name != "" || bound.Summary != r.RenameError || len(bound.PreviousKeys) != 1 {
		t.Fatalf("the thread must stay listed, unnamed, showing the failure: %+v", bound)
	}
	if _, err := p.List(context.Background(), false); err != nil || api.renames != 1 {
		t.Fatalf("a failed pending rename must not be retried automatically (err=%v, renames=%d)", err, api.renames)
	}

	// The retry is an ordinary rename of the thread, which resolves the failure.
	api.renameErr = nil
	if err := p.Rename(context.Background(), codexKey("thread-1"), "foo"); err != nil {
		t.Fatal(err)
	}
	if got := runsOf(t, store)["run-1"].RenameError; got != "" {
		t.Fatalf("RenameError = %q, want it cleared by the manual rename", got)
	}
}

func TestCancelledListKeepsPendingRename(t *testing.T) {
	p, api, store := startRenameOnlyRun(t)
	api.rows = append(api.rows, Thread{ID: "thread-1", CWD: "/work"})
	api.renameErr = context.Canceled
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.List(ctx, false); err != nil {
		t.Fatal(err)
	}
	if r := runsOf(t, store)["run-1"]; r.PendingRename != "foo" || r.RenameError != "" {
		t.Fatalf("a cancelled List says nothing about the rename: %+v", r)
	}
}

func ptr(s string) *string { return &s }
