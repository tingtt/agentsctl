package codex

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/tingtt/agentsctl/internal/session"
)

// Existing-thread mutations run on the shared daemon, the process that
// holds the thread's writer lock, over a connection of their own.

func TestRenameRunsOnSharedDaemonWithoutInterrupting(t *testing.T) {
	f := newDispatchFixture(t)
	f.d.setThreads(catalogThread("thread-1", 1))
	f.d.setLoaded("thread-1", active())
	if err := f.p.Rename(context.Background(), codexKey("thread-1"), "new name"); err != nil {
		t.Fatal(err)
	}
	f.d.waitClosed(t, 1)
	requireEvents(t, f.d, "ensure", "initialize", "thread/name/set", "close")
	if got := decodeParams(t, f.d.requestsOf("thread/name/set")[0]); !reflect.DeepEqual(got, map[string]any{"threadId": "thread-1", "name": "new name"}) {
		t.Fatalf("thread/name/set params = %v", got)
	}
	if status := f.d.loaded["thread-1"]; status.Type != statusActive {
		t.Fatalf("renaming a running thread changed its status to %+v", status)
	}
}

func TestUnarchiveRunsOnSharedDaemon(t *testing.T) {
	f := newDispatchFixture(t)
	f.d.archived = []Thread{catalogThread("thread-1", 1)}
	if err := f.p.Unarchive(context.Background(), codexKey("thread-1")); err != nil {
		t.Fatal(err)
	}
	f.d.waitClosed(t, 1)
	requireEvents(t, f.d, "ensure", "initialize", "thread/unarchive", "close")
	if len(f.d.threads) != 1 || f.d.threads[0].ID != "thread-1" {
		t.Fatalf("thread not restored: %+v", f.d.threads)
	}
}

func TestMutationsEnsureFailureContactsNoDaemon(t *testing.T) {
	for name, mutate := range map[string]func(*Provider) error{
		"rename":    func(p *Provider) error { return p.Rename(context.Background(), codexKey("thread-1"), "x") },
		"archive":   func(p *Provider) error { return p.Archive(context.Background(), codexKey("thread-1")) },
		"unarchive": func(p *Provider) error { return p.Unarchive(context.Background(), codexKey("thread-1")) },
	} {
		t.Run(name, func(t *testing.T) {
			f := newDispatchFixture(t)
			f.lifecycle.err = errBoom
			if err := mutate(f.p); !errors.Is(err, errBoom) {
				t.Fatalf("%s = %v, want the Ensure failure", name, err)
			}
			requireEvents(t, f.d, "ensure")
		})
	}
}

// A thread at rest is archived on the same connection that just confirmed
// it is at rest.
func TestArchiveThreadAtRestOnSharedDaemon(t *testing.T) {
	for name, status := range map[string]ThreadStatus{
		"idle":        idle,
		"systemError": {Type: statusSystemError},
		"notLoaded":   notLoadedSt, // with no writer (see newDispatchFixture)
	} {
		t.Run(name, func(t *testing.T) {
			f := newDispatchFixture(t)
			f.d.setThreads(catalogThread("thread-1", 1))
			f.d.setLoaded("thread-1", status)
			if err := f.p.Archive(context.Background(), codexKey("thread-1")); err != nil {
				t.Fatal(err)
			}
			f.d.waitClosed(t, 1)
			requireEvents(t, f.d, "ensure", "initialize", "thread/read", "thread/archive", "close")
			if len(f.d.archived) != 1 {
				t.Fatalf("thread not archived: threads=%+v", f.d.threads)
			}
		})
	}
}

// Archive never becomes an implicit Stop: a running thread (working or
// waiting on the user), a thread another process writes, or one whose
// status cannot be established is refused before thread/archive is sent,
// and the thread keeps running and stays listed.
func TestArchiveRefusesUnlessDaemonConfirmsRest(t *testing.T) {
	for name, tc := range map[string]struct {
		status     ThreadStatus
		writerHeld bool
		readFails  bool
		want       error
	}{
		"working":             {status: active(), want: errArchiveRunning},
		"waiting on approval": {status: active(flagWaitingOnApproval), want: errArchiveRunning},
		"waiting on input":    {status: active(flagWaitingOnUserInput), want: errArchiveRunning},
		"external writer":     {status: notLoadedSt, writerHeld: true, want: errArchiveExternalWriter},
		"unknown status":      {status: ThreadStatus{Type: "somethingNew"}},
		"read failure":        {status: idle, readFails: true},
	} {
		t.Run(name, func(t *testing.T) {
			f := newDispatchFixture(t)
			f.p.writerFree = func(string) bool { return !tc.writerHeld }
			f.d.setThreads(catalogThread("thread-1", 1))
			f.d.setLoaded("thread-1", tc.status)
			if tc.readFails {
				f.d.failNext("thread/read", 1)
			}
			err := f.p.Archive(context.Background(), codexKey("thread-1"))
			if err == nil || (tc.want != nil && !errors.Is(err, tc.want)) {
				t.Fatalf("Archive = %v, want refusal %v", err, tc.want)
			}
			f.d.waitClosed(t, 1)
			requireEvents(t, f.d, "ensure", "initialize", "thread/read", "close")
			for _, method := range []string{"thread/archive", "turn/interrupt"} {
				if n := f.d.callCount(method); n != 0 {
					t.Fatalf("%s sent %d times", method, n)
				}
			}
			wantLoaded := tc.status.Type
			if wantLoaded == statusNotLoaded {
				wantLoaded = "" // never loaded on the daemon
			}
			f.d.mu.Lock()
			defer f.d.mu.Unlock()
			if len(f.d.threads) != 1 || f.d.loaded["thread-1"].Type != wantLoaded {
				t.Fatalf("refused Archive changed the thread: threads=%+v loaded=%+v", f.d.threads, f.d.loaded)
			}
		})
	}
}

// Archive availability on a row is advisory, from the same observation as
// its Activity: a running session or one another process writes does not
// offer it. Anything else offers it and leaves the decision to Archive's
// own preflight.
func TestArchiveAvailabilityFollowsObservation(t *testing.T) {
	threads := []Thread{
		{ID: "working", Status: active()},
		{ID: "approval", Status: active(flagWaitingOnApproval)},
		{ID: "input", Status: active(flagWaitingOnUserInput)},
		{ID: "idle", Status: idle},
		{ID: "failed", Status: ThreadStatus{Type: statusSystemError}},
		{ID: "dormant", Status: notLoadedSt},
		{ID: "external", Status: notLoadedSt},
		{ID: "unknown", Status: ThreadStatus{Type: "somethingNew"}},
	}
	p := &Provider{API: &fakeAPI{}, writerFree: func(id string) bool { return id != "external" }}
	rows := p.sessionRows(threads, false, func(t Thread, writerFree func() bool) observation {
		return observeThread(t.Status, writerFree)
	})
	var offered []string
	for _, row := range rows {
		archive := row.Actions[session.ActionArchive]
		if !archive.Available && archive.Reason == "" {
			t.Fatalf("%s: Archive unavailable without a reason", row.Key.ID)
		}
		if archive.Available {
			offered = append(offered, row.Key.ID)
		}
	}
	if want := []string{"idle", "failed", "dormant", "unknown"}; !slices.Equal(offered, want) {
		t.Fatalf("Archive offered for %q, want %q", offered, want)
	}
}
