package codex

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"testing"

	"github.com/tingtt/agentsctl/internal/session"
	"github.com/tingtt/agentsctl/internal/sessionctl"
)

// A new thread reaches thread/list only once its rollout is persisted, but
// the daemon announces it (thread/started) at once. The runtime shows the
// announced thread from then on and hands it over to thread/list's copy
// once listed. These tests drive the runtime directly, so every step is
// deterministic.

// newLiveRuntime returns a live runtime whose durable catalog lists "old".
func newLiveRuntime(t *testing.T) *codexRuntime {
	t.Helper()
	rt := newCodexRuntime(nil)
	rt.installCatalog(rt.beginCatalogFetch(), []Thread{catalogThread("old", 1)})
	rt.mu.Lock()
	rt.live = true
	rt.mu.Unlock()
	return rt
}

func notifyRuntime(t *testing.T, rt *codexRuntime, method string, params any) {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	rt.handleNotification(method, raw)
}

func visibleIDs(rt *codexRuntime) []string {
	var ids []string
	for _, t := range rt.view().catalog {
		ids = append(ids, t.ID)
	}
	return ids
}

func visibleThread(t *testing.T, rt *codexRuntime, id string) Thread {
	t.Helper()
	for _, th := range rt.view().catalog {
		if th.ID == id {
			return th
		}
	}
	t.Fatalf("%s not visible: %q", id, visibleIDs(rt))
	return Thread{}
}

func activityOf(t *testing.T, rt *codexRuntime, id string) session.Activity {
	t.Helper()
	view := rt.view()
	return view.observe(visibleThread(t, rt, id), func() bool { return true }).Activity
}

func startedThread(id string) Thread {
	return Thread{ID: id, CWD: "/work/new", CreatedAt: 5, UpdatedAt: 5, Preview: ptr("prompt"), Status: idle}
}

func requireNoStarted(t *testing.T, rt *codexRuntime) {
	t.Helper()
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if len(rt.started) != 0 {
		t.Fatalf("live-started threads left behind: %+v", rt.started)
	}
}

func TestStartedThreadIsVisibleBeforeListed(t *testing.T) {
	rt := newLiveRuntime(t)
	notifyRuntime(t, rt, notifyStarted, map[string]any{"thread": startedThread("new")})

	if got := visibleIDs(rt); !slices.Equal(got, []string{"new", "old"}) {
		t.Fatalf("visible = %q, want the started thread next to the catalog", got)
	}
	if got := visibleThread(t, rt, "new"); got.CWD != "/work/new" || got.CreatedAt != 5 || value(got.Preview) != "prompt" {
		t.Fatalf("visible thread = %+v, want the daemon's own Thread", got)
	}
	if activityOf(t, rt, "new") != session.ActivityIdle {
		t.Fatalf("activity = %s, want the announced status", activityOf(t, rt, "new"))
	}
	rt.mu.Lock()
	need := rt.needCatalog
	rt.mu.Unlock()
	if !need {
		t.Fatal("thread/started must still ask for a catalog re-read")
	}
}

// A re-read that does not list the new thread yet says nothing about it:
// the thread and its status stay.
func TestCatalogWithoutStartedThreadKeepsIt(t *testing.T) {
	rt := newLiveRuntime(t)
	notifyRuntime(t, rt, notifyStarted, map[string]any{"thread": startedThread("new")})
	notifyRuntime(t, rt, notifyStatusChanged, map[string]any{"threadId": "new", "status": active()})

	if !rt.installCatalog(rt.beginCatalogFetch(), []Thread{catalogThread("old", 1)}) {
		t.Fatal("resync not installed")
	}
	if got := visibleIDs(rt); !slices.Equal(got, []string{"new", "old"}) {
		t.Fatalf("visible = %q after a resync without the new thread", got)
	}
	if got := activityOf(t, rt, "new"); got != session.ActivityWorking {
		t.Fatalf("activity = %s, want its status kept", got)
	}
}

func TestStartedThreadFollowsNativeStatus(t *testing.T) {
	rt := newLiveRuntime(t)
	notifyRuntime(t, rt, notifyStarted, map[string]any{"thread": startedThread("new")})
	for _, tc := range []struct {
		status ThreadStatus
		want   session.Activity
	}{
		{active(), session.ActivityWorking},
		{active(flagWaitingOnApproval), session.ActivityNeedsInput},
		{active(flagWaitingOnUserInput), session.ActivityNeedsInput},
		{idle, session.ActivityIdle},
	} {
		notifyRuntime(t, rt, notifyStatusChanged, map[string]any{"threadId": "new", "status": tc.status})
		if got := activityOf(t, rt, "new"); got != tc.want {
			t.Fatalf("%+v: activity = %s, want %s", tc.status, got, tc.want)
		}
	}
}

// A rename shows at once, whether the thread is only live-started (as a
// rename-only Dispatch's thread can be) or already listed.
func TestNameUpdateAppliesToVisibleCatalog(t *testing.T) {
	rt := newLiveRuntime(t)
	before := rt.view().catalog
	notifyRuntime(t, rt, notifyStarted, map[string]any{"thread": startedThread("new")})

	notifyRuntime(t, rt, notifyNameUpdated, map[string]any{"threadId": "new", "threadName": "foo"})
	notifyRuntime(t, rt, notifyNameUpdated, map[string]any{"threadId": "old", "threadName": "bar"})

	if got := value(visibleThread(t, rt, "new").Name); got != "foo" {
		t.Fatalf("live-started name = %q", got)
	}
	if got := value(visibleThread(t, rt, "old").Name); got != "bar" {
		t.Fatalf("listed name = %q", got)
	}
	if value(before[0].Name) != "old" {
		t.Fatal("an earlier view's catalog changed in place")
	}
	notifyRuntime(t, rt, notifyNameUpdated, map[string]any{"threadId": "new"})
	if got := visibleThread(t, rt, "new").Name; got != nil {
		t.Fatalf("a cleared name stayed %q", *got)
	}
}

// Once thread/list lists the thread, its listed copy replaces the
// announced one: one row, the durable data, nothing left over.
func TestListedStartedThreadIsPromoted(t *testing.T) {
	rt := newLiveRuntime(t)
	notifyRuntime(t, rt, notifyStarted, map[string]any{"thread": startedThread("new")})
	notifyRuntime(t, rt, notifyStatusChanged, map[string]any{"threadId": "new", "status": active()})

	durable := Thread{ID: "new", Name: ptr("durable"), CWD: "/work/new", CreatedAt: 5, UpdatedAt: 9}
	rt.installCatalog(rt.beginCatalogFetch(), []Thread{durable, catalogThread("old", 1)})

	if got := visibleIDs(rt); !slices.Equal(got, []string{"new", "old"}) {
		t.Fatalf("visible = %q, want the thread once", got)
	}
	if got := visibleThread(t, rt, "new"); !reflect.DeepEqual(got, durable) {
		t.Fatalf("visible = %+v, want the listed Thread", got)
	}
	if got := activityOf(t, rt, "new"); got != session.ActivityWorking {
		t.Fatalf("activity = %s, want the status kept across promotion", got)
	}
	requireNoStarted(t, rt)

	// Once durable, leaving thread/list is an ordinary removal again.
	rt.installCatalog(rt.beginCatalogFetch(), []Thread{catalogThread("old", 1)})
	if got := visibleIDs(rt); !slices.Equal(got, []string{"old"}) {
		t.Fatalf("visible = %q after the durable thread left the catalog", got)
	}
}

func TestStartedThreadRemovedBeforeListed(t *testing.T) {
	for _, method := range []string{notifyClosed, notifyArchived, notifyDeleted} {
		t.Run(method, func(t *testing.T) {
			rt := newLiveRuntime(t)
			notifyRuntime(t, rt, notifyStarted, map[string]any{"thread": startedThread("new")})
			notifyRuntime(t, rt, method, map[string]any{"threadId": "new"})
			if got := visibleIDs(rt); !slices.Equal(got, []string{"old"}) {
				t.Fatalf("visible = %q, want the started thread gone", got)
			}
			requireNoStarted(t, rt)
		})
	}
}

// A snapshot rebuilds the live-started threads from what the daemon has
// loaded: one announced on an earlier connection and gone since (closed
// while disconnected) does not linger, while one announced during this
// snapshot is kept.
func TestSnapshotReplacesStartedThreads(t *testing.T) {
	rt := newLiveRuntime(t)
	notifyRuntime(t, rt, notifyStarted, map[string]any{"thread": startedThread("gone")})
	rt.disconnected(nil, false)

	rt.mu.Lock()
	rt.snapshotting, rt.dirty, rt.startedNow = true, map[string]bool{}, map[string]bool{}
	rt.mu.Unlock()
	notifyRuntime(t, rt, notifyStarted, map[string]any{"thread": startedThread("during")})
	rt.mu.Lock()
	rt.dirty = map[string]bool{} // re-read, still loaded (see loaded below)
	rt.mu.Unlock()
	loaded := map[string]Thread{"loaded": startedThread("loaded")}
	if dirty := rt.installSnapshot(rt.beginCatalogFetch(), []Thread{catalogThread("old", 1)}, map[string]ThreadStatus{"loaded": idle, "during": idle}, loaded, nil); len(dirty) != 0 {
		t.Fatalf("snapshot not installed: dirty=%v", dirty)
	}
	got := visibleIDs(rt)
	slices.Sort(got)
	if want := []string{"during", "loaded", "old"}; !slices.Equal(got, want) {
		t.Fatalf("visible = %q, want %q", got, want)
	}
}

func TestHiddenStartedThreadNeverVisible(t *testing.T) {
	rt := newLiveRuntime(t)
	notifyRuntime(t, rt, notifyStarted, map[string]any{"thread": titleThread("title")})
	notifyRuntime(t, rt, notifyStatusChanged, map[string]any{"threadId": "title", "status": active()})
	if got := visibleIDs(rt); !slices.Equal(got, []string{"old"}) {
		t.Fatalf("visible = %q, want the title-generation thread hidden", got)
	}
	requireNoStarted(t, rt)
}

// End to end: a Dispatch's thread shows in the Observer's snapshots from
// thread/started on, with its native status, even though thread/list does
// not list it (turn/started never comes, so it never persists) -- and is
// promoted to its listed copy, once, when it does persist.
func TestDispatchedThreadVisibleBeforePersisted(t *testing.T) {
	d := newFakeDaemon(t)
	d.setThreads(catalogThread("old", 1))
	d.turnStarted = "never"
	p, _ := newObservedProvider(t, d, nil)
	p.Runtime = &fakeManagedRuntime{}
	ch := observe(t, p)
	c := d.waitReady(t)
	waitFor(t, ch, "initial", activityIs("old", session.ActivityIdle))

	once := func(u sessionctl.ProviderUpdate) {
		n := 0
		for _, s := range u.Sessions {
			if s.Key == codexKey("thread-new-1") {
				n++
			}
		}
		if n > 1 {
			t.Errorf("thread-new-1 listed %d times", n)
		}
	}
	if _, err := p.Dispatch(context.Background(), "prompt", "/work"); err != nil {
		t.Fatal(err)
	}
	u := waitFor(t, ch, "dispatched thread working", activityIs("thread-new-1", session.ActivityWorking), once)
	if row, _ := rowOf(u, "thread-new-1"); row.CWD != "/work" || row.RunID != "" || len(row.PreviousKeys) != 0 {
		t.Fatalf("row = %+v, want the daemon's canonical thread", row)
	}
	if n := d.callCount("thread/list"); n < 2 {
		t.Fatalf("thread/list called %d times, want the resync the new thread asked for", n)
	}

	d.materialize("thread-new-1")
	c.statusChanged("thread-new-1", idle)
	waitFor(t, ch, "persisted thread idle", activityIs("thread-new-1", session.ActivityIdle), once)
	d.dropAll() // a reconnect's snapshot lists it too, still once
	waitFor(t, ch, "reconnected", func(u sessionctl.ProviderUpdate) bool {
		return u.Warning == nil && activityIs("thread-new-1", session.ActivityIdle)(u)
	}, once)
}

// A rename-only Dispatch's row shows the requested name as soon as the
// daemon announces it.
func TestRenameOnlyDispatchedThreadShowsName(t *testing.T) {
	d := newFakeDaemon(t)
	p, _ := newObservedProvider(t, d, nil)
	p.Runtime = &fakeManagedRuntime{}
	ch := observe(t, p)
	d.waitReady(t)
	waitFor(t, ch, "initial", func(u sessionctl.ProviderUpdate) bool { return u.Err == nil && u.Warning == nil })

	if _, err := p.Dispatch(context.Background(), "/rename foo", "/work"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, ch, "renamed row", func(u sessionctl.ProviderUpdate) bool {
		s, ok := rowOf(u, "thread-new-1")
		return ok && s.Name == "foo"
	})
}

// A connection that comes up while a new thread is loaded but not yet
// listed (e.g. after a reconnect) shows it from its snapshot.
func TestSnapshotShowsLoadedUnlistedThread(t *testing.T) {
	d := newFakeDaemon(t)
	d.setThreads(catalogThread("old", 1))
	d.mu.Lock()
	d.pending["new"] = Thread{ID: "new", CWD: "/work", CreatedAt: 5, UpdatedAt: 5}
	d.mu.Unlock()
	d.setLoaded("new", active())
	p, _ := newObservedProvider(t, d, nil)
	ch := observe(t, p)
	c := d.waitReady(t)
	waitFor(t, ch, "new working", activityIs("new", session.ActivityWorking))

	c.notify(notifyClosed, map[string]any{"threadId": "new"})
	waitFor(t, ch, "closed before persisted", func(u sessionctl.ProviderUpdate) bool {
		_, ok := rowOf(u, "new")
		return u.Err == nil && !ok
	})
}
