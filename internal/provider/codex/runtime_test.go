package codex

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tingtt/agentsctl/internal/localstate"
	"github.com/tingtt/agentsctl/internal/session"
	"github.com/tingtt/agentsctl/internal/sessionctl"
)

type lifecycleResult struct {
	info DaemonInfo
	err  error
	gate <-chan struct{}
}

type scriptedLifecycle struct {
	mu      sync.Mutex
	results []lifecycleResult
	calls   int
	called  chan int
}

func (l *scriptedLifecycle) Ensure(ctx context.Context) (DaemonInfo, error) {
	l.mu.Lock()
	i := l.calls
	l.calls++
	result := l.results[min(i, len(l.results)-1)]
	l.mu.Unlock()
	if l.called != nil {
		l.called <- i + 1
	}
	if result.gate != nil {
		select {
		case <-result.gate:
		case <-ctx.Done():
			return DaemonInfo{}, ctx.Err()
		}
	}
	return result.info, result.err
}

func readyDaemon(socket string) lifecycleResult {
	return lifecycleResult{info: DaemonInfo{Status: "started", SocketPath: socket}}
}

func TestObserverEnsuresDaemonBeforeConnecting(t *testing.T) {
	d := newFakeDaemon(t)
	d.setThreads(catalogThread("a", 1))
	lifecycle := &scriptedLifecycle{results: []lifecycleResult{readyDaemon(d.socket)}, called: make(chan int, 4)}
	p := &Provider{API: &fakeAPI{}, Store: localstate.New(t.TempDir() + "/state.json"), Daemon: lifecycle}
	rt := p.runtime()
	rt.minBackoff, rt.maxBackoff, rt.catalogGap = 10*time.Millisecond, 50*time.Millisecond, 0
	ch := observe(t, p)

	select {
	case call := <-lifecycle.called:
		if call != 1 {
			t.Fatalf("first lifecycle call=%d", call)
		}
	case <-time.After(time.Second):
		t.Fatal("Ensure was not called")
	}
	d.waitReady(t)
	waitFor(t, ch, "snapshot after Ensure", activityIs("a", session.ActivityIdle))
}

func TestObserverPublishesInitialEnsureFailureThenRecovers(t *testing.T) {
	d := newFakeDaemon(t)
	d.setThreads(catalogThread("a", 1))
	second := make(chan struct{})
	lifecycle := &scriptedLifecycle{results: []lifecycleResult{
		{err: errors.New("daemon package unavailable")},
		{info: readyDaemon(d.socket).info, gate: second},
	}, called: make(chan int, 4)}
	p := &Provider{API: &fakeAPI{}, Store: localstate.New(t.TempDir() + "/state.json"), Daemon: lifecycle}
	rt := p.runtime()
	rt.minBackoff, rt.maxBackoff, rt.catalogGap = 20*time.Millisecond, 40*time.Millisecond, 0
	ch := observe(t, p)

	u := waitFor(t, ch, "initial Ensure error", func(u sessionctl.ProviderUpdate) bool {
		return u.Err != nil && strings.Contains(u.Err.Error(), "daemon package unavailable")
	})
	if u.Sessions != nil {
		t.Fatalf("initial failure must be error-only: %+v", u)
	}
	select {
	case call := <-lifecycle.called:
		if call != 1 {
			t.Fatalf("first lifecycle call=%d", call)
		}
	default:
		t.Fatal("missing first Ensure call")
	}

	select {
	case call := <-lifecycle.called:
		if call != 2 {
			t.Fatalf("second lifecycle call=%d", call)
		}
	case <-time.After(time.Second):
		t.Fatal("reconnect did not re-run Ensure through backoff")
	}
	close(second)
	d.waitReady(t)
	u = waitFor(t, ch, "recovered snapshot", activityIs("a", session.ActivityIdle))
	if u.Err != nil || u.Warning != nil {
		t.Fatalf("recovered update=%+v", u)
	}
}

func TestObserverReEnsuresDaemonAfterAuthorityLoss(t *testing.T) {
	d := newFakeDaemon(t)
	d.setThreads(catalogThread("a", 1))
	d.setLoaded("a", active())
	third := make(chan struct{})
	lifecycle := &scriptedLifecycle{results: []lifecycleResult{
		readyDaemon(d.socket),
		{err: errors.New("daemon absent")},
		{info: readyDaemon(d.socket).info, gate: third},
	}, called: make(chan int, 8)}
	p := &Provider{API: &fakeAPI{}, Store: localstate.New(t.TempDir() + "/state.json"), Daemon: lifecycle}
	rt := p.runtime()
	rt.minBackoff, rt.maxBackoff, rt.catalogGap = 20*time.Millisecond, 40*time.Millisecond, 0
	ch := observe(t, p)
	d.waitReady(t)
	waitFor(t, ch, "initial authority", activityIs("a", session.ActivityWorking))

	d.stop()
	u := waitFor(t, ch, "Ensure failure after authority", func(u sessionctl.ProviderUpdate) bool {
		return u.Err == nil && u.Warning != nil && strings.Contains(u.Warning.Error(), "daemon absent")
	})
	if len(u.Sessions) != 1 || u.Sessions[0].Activity != session.ActivityUnknown || u.Sessions[0].Runtime != session.RuntimeUnknown {
		t.Fatalf("daemon failure snapshot=%+v", u)
	}

	for {
		select {
		case call := <-lifecycle.called:
			if call == 3 {
				d.start()
				close(third)
				goto recovering
			}
		case <-time.After(time.Second):
			t.Fatal("reconnect did not Ensure again")
		}
	}

recovering:
	d.waitReady(t)
	u = waitFor(t, ch, "snapshot after daemon restart", func(u sessionctl.ProviderUpdate) bool {
		return activityIs("a", session.ActivityWorking)(u) && u.Warning == nil
	})
	if u.Err != nil {
		t.Fatalf("recovered update=%+v", u)
	}
}

// A status change of one thread republishes the whole catalog, never a
// delta holding only the changed thread.
func TestObserverPublishesFullSnapshotOnStatusChange(t *testing.T) {
	d := newFakeDaemon(t)
	d.setThreads(catalogThread("a", 2), catalogThread("b", 1))
	d.setLoaded("a", idle)
	d.setLoaded("b", idle)
	p, _ := newObservedProvider(t, d, nil)
	ch := observe(t, p)
	c := d.waitReady(t)
	waitFor(t, ch, "initial snapshot", activityIs("a", session.ActivityIdle))

	c.statusChanged("a", active())
	u := waitFor(t, ch, "a working", activityIs("a", session.ActivityWorking))
	if len(u.Sessions) != 2 {
		t.Fatalf("snapshot must carry the full catalog, got %d rows: %+v", len(u.Sessions), u.Sessions)
	}
	if b, ok := rowOf(u, "b"); !ok || b.Activity != session.ActivityIdle || b.Runtime != session.RuntimeDetached {
		t.Fatalf("unchanged thread must stay in the snapshot with its own state: %+v", b)
	}
}

func TestObserverFollowsWaitingFlags(t *testing.T) {
	d := newFakeDaemon(t)
	d.setThreads(catalogThread("a", 1))
	d.setLoaded("a", active())
	p, _ := newObservedProvider(t, d, nil)
	ch := observe(t, p)
	c := d.waitReady(t)
	waitFor(t, ch, "initial working", activityIs("a", session.ActivityWorking))

	steps := []struct {
		status ThreadStatus
		want   session.Activity
	}{
		{active("waitingOnApproval"), session.ActivityNeedsInput},
		{active(), session.ActivityWorking},
		{active("waitingOnUserInput"), session.ActivityNeedsInput},
		{active(), session.ActivityWorking},
		{idle, session.ActivityIdle},
	}
	for _, step := range steps {
		c.statusChanged("a", step.status)
		waitFor(t, ch, string(step.want), activityIs("a", step.want))
	}
}

func TestObserverResolvesNotLoadedThroughWriterLockOnly(t *testing.T) {
	d := newFakeDaemon(t)
	d.setThreads(catalogThread("dormant", 3), catalogThread("external", 2), catalogThread("loaded", 1))
	d.setLoaded("loaded", active())
	p, probe := newObservedProvider(t, d, nil)
	probe.writers["external"] = true
	probe.writers["loaded"] = true // the daemon itself holds a loaded thread's lock
	ch := observe(t, p)
	d.waitReady(t)
	u := waitFor(t, ch, "snapshot", activityIs("loaded", session.ActivityWorking))

	want := map[string]observation{
		"dormant":  {session.ActivityIdle, session.RuntimeNone},
		"external": {session.ActivityUnknown, session.RuntimeExternal},
		"loaded":   {session.ActivityWorking, session.RuntimeDetached},
	}
	for id, w := range want {
		s, _ := rowOf(u, id)
		if (observation{s.Activity, s.Runtime}) != w {
			t.Fatalf("%s=%s/%s, want %+v", id, s.Activity, s.Runtime, w)
		}
	}
	// Open goes through the shared daemon: a thread it has loaded or a
	// dormant one can be opened, one running outside it cannot. Native Stop
	// is offered only for the daemon-loaded active thread.
	for id, want := range map[string]struct{ open, stop bool }{
		"dormant":  {open: true},
		"external": {},
		"loaded":   {open: true, stop: true},
	} {
		s, _ := rowOf(u, id)
		if s.Actions.Available(session.ActionOpen) != want.open || s.Actions.Available(session.ActionStop) != want.stop {
			t.Fatalf("%s actions=%+v, want Open=%v Stop=%v", id, s.Actions, want.open, want.stop)
		}
	}
}

// A loaded thread's Activity and actions never depend on the writer lock:
// it is not even probed.
func TestObserverLoadedActivityIgnoresWriterLock(t *testing.T) {
	d := newFakeDaemon(t)
	d.setThreads(catalogThread("loaded", 1))
	d.setLoaded("loaded", idle)
	p, probe := newObservedProvider(t, d, nil)
	probe.writers["loaded"] = true
	ch := observe(t, p)
	d.waitReady(t)
	u := waitFor(t, ch, "snapshot", activityIs("loaded", session.ActivityIdle))
	if s, _ := rowOf(u, "loaded"); s.Runtime != session.RuntimeDetached || !s.Actions.Available(session.ActionOpen) {
		t.Fatalf("loaded=%+v", s)
	}
	if n := probe.callsFor("loaded"); n != 0 {
		t.Fatalf("writer lock probed %d times for a loaded thread", n)
	}
}

func TestObserverConvergesAfterReconnect(t *testing.T) {
	d := newFakeDaemon(t)
	d.setThreads(catalogThread("a", 2), catalogThread("b", 1))
	d.setLoaded("a", active())
	d.setLoaded("b", active())
	p, _ := newObservedProvider(t, d, nil)
	ch := observe(t, p)
	d.waitReady(t)
	waitFor(t, ch, "a working", activityIs("a", session.ActivityWorking))

	// The daemon goes away: every thread becomes Unknown, in a full
	// successful snapshot carrying a Warning.
	d.stop()
	u := waitFor(t, ch, "unobserved snapshot", func(u sessionctl.ProviderUpdate) bool {
		return u.Err == nil && u.Warning != nil
	})
	if len(u.Sessions) != 2 {
		t.Fatalf("rows=%+v", u.Sessions)
	}
	for _, s := range u.Sessions {
		if s.Activity != session.ActivityUnknown || s.Runtime != session.RuntimeUnknown {
			t.Fatalf("unobserved row=%+v", s)
		}
	}

	// It comes back with a finished turn on a and b unloaded (absent from
	// the new loaded list): both converge on the current state.
	d.setLoaded("a", idle)
	d.setLoaded("b", notLoadedSt)
	d.start()
	d.waitReady(t)
	u = waitFor(t, ch, "reconnected", func(u sessionctl.ProviderUpdate) bool {
		return activityIs("a", session.ActivityIdle)(u) && u.Warning == nil
	})
	if b, _ := rowOf(u, "b"); b.Activity != session.ActivityIdle || b.Runtime != session.RuntimeNone {
		t.Fatalf("a thread no longer loaded must be notLoaded, not its stale active: %+v", b)
	}
}

// A status change that races the snapshot's own read makes the snapshot
// read the thread again; the stale read is never published.
func TestSnapshotRereadsThreadChangedDuringSnapshot(t *testing.T) {
	d := newFakeDaemon(t)
	d.setThreads(catalogThread("a", 1))
	d.setLoaded("a", active())
	var reads atomic.Int32
	d.beforeRead = func(c *fakeConn, id string) *ThreadStatus {
		if id != "a" || reads.Add(1) != 1 {
			return nil
		}
		// The turn finishes while the first read is in flight: its
		// notification reaches the client first, then the read answers
		// with the status it had captured before.
		c.statusChanged("a", idle)
		stale := active()
		return &stale
	}
	p, _ := newObservedProvider(t, d, nil)
	ch := observe(t, p)
	d.waitReady(t)
	var sawWorking bool
	waitFor(t, ch, "a idle", activityIs("a", session.ActivityIdle), func(u sessionctl.ProviderUpdate) {
		if activityIs("a", session.ActivityWorking)(u) {
			sawWorking = true
		}
	})
	if sawWorking {
		t.Fatal("stale snapshot read was published")
	}
	if n := reads.Load(); n < 2 {
		t.Fatalf("thread changed during snapshot must be read again, reads=%d", n)
	}
}

func TestObserverWithoutDaemonKeepsListAuthorityAndBacksOff(t *testing.T) {
	var attempts atomic.Int32
	p := &Provider{API: &fakeAPI{}, Store: localstate.New(t.TempDir() + "/state.json")}
	rt := newCodexRuntime(func(context.Context) (string, error) {
		attempts.Add(1)
		return "/nonexistent/app-server-control.sock", nil
	})
	rt.minBackoff, rt.maxBackoff = 20*time.Millisecond, 40*time.Millisecond
	p.obs.rt = rt
	ch := observe(t, p)

	// List still works and must not make the Observer publish a successful
	// snapshot (that would take row authority away from List).
	if _, err := p.List(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	select {
	case u := <-ch:
		t.Fatalf("no publication before the daemon was ever reached, got %+v", u)
	case <-time.After(300 * time.Millisecond):
	}
	// 300ms at 20ms..40ms backoff is at most ~10 attempts; a busy loop
	// would be thousands.
	if n := attempts.Load(); n < 2 || n > 15 {
		t.Fatalf("reconnect attempts=%d, want bounded backoff", n)
	}
}

// Rows that only the existing execution path produces -- a new thread
// List found, a provisional run -- reach the Observer's snapshots once it
// owns the rows.
func TestListFeedsObserverCatalogAfterAuthority(t *testing.T) {
	d := newFakeDaemon(t)
	d.setThreads(catalogThread("a", 1))
	api := &fakeAPI{}
	p, _ := newObservedProvider(t, d, api)
	ch := observe(t, p)
	d.waitReady(t)
	waitFor(t, ch, "initial", activityIs("a", session.ActivityIdle))

	if err := p.Store.SaveRun(localstate.Run{ID: "run-1", Provider: "codex", State: "running", CWD: "/work", StartedAt: time.Unix(5, 0)}); err != nil {
		t.Fatal(err)
	}
	api.rows = []Thread{catalogThread("a", 1), catalogThread("fresh", 3)}
	if _, err := p.List(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	u := waitFor(t, ch, "listed rows", func(u sessionctl.ProviderUpdate) bool {
		_, fresh := rowOf(u, "fresh")
		_, run := rowOf(u, "run-1")
		return fresh && run
	})
	if s, _ := rowOf(u, "run-1"); s.Activity != session.ActivityStarting {
		t.Fatalf("provisional run=%+v", s)
	}
	if s, _ := rowOf(u, "fresh"); s.Activity != session.ActivityIdle {
		t.Fatalf("listed thread must take its Activity from the runtime: %+v", s)
	}
}

// A List that started before the installed catalog was fetched never
// replaces it.
func TestOlderCatalogFetchNeverReplacesNewer(t *testing.T) {
	rt := newCodexRuntime(nil)
	older := rt.beginCatalogFetch()
	newer := rt.beginCatalogFetch()
	if !rt.installCatalog(newer, []Thread{catalogThread("new", 1)}) {
		t.Fatal("newer fetch must install")
	}
	if rt.installCatalog(older, []Thread{catalogThread("old", 1)}) {
		t.Fatal("older fetch must not install")
	}
	if v := rt.view(); len(v.catalog) != 1 || v.catalog[0].ID != "new" {
		t.Fatalf("catalog=%+v", v.catalog)
	}
}

func TestObserverResyncsCatalogOnLifecycleNotifications(t *testing.T) {
	d := newFakeDaemon(t)
	d.pageSize = 1 // exercise pagination
	d.setThreads(catalogThread("a", 1), catalogThread("b", 2))
	p, _ := newObservedProvider(t, d, nil)
	ch := observe(t, p)
	c := d.waitReady(t)
	waitFor(t, ch, "initial", func(u sessionctl.ProviderUpdate) bool { return len(u.Sessions) == 2 })

	d.setThreads(catalogThread("a", 1), catalogThread("b", 2), catalogThread("c", 3))
	c.notify(notifyStarted, map[string]any{"thread": catalogThread("c", 3)})
	waitFor(t, ch, "started thread", func(u sessionctl.ProviderUpdate) bool { _, ok := rowOf(u, "c"); return ok })

	renamed := catalogThread("a", 1)
	renamed.Name = ptr("renamed")
	d.setThreads(renamed, catalogThread("b", 2), catalogThread("c", 3))
	c.notify(notifyNameUpdated, map[string]any{"threadId": "a", "threadName": "renamed"})
	waitFor(t, ch, "renamed thread", func(u sessionctl.ProviderUpdate) bool { s, _ := rowOf(u, "a"); return s.Name == "renamed" })

	d.setThreads(renamed, catalogThread("c", 3))
	c.notify(notifyArchived, map[string]any{"threadId": "b"})
	waitFor(t, ch, "archived thread gone", func(u sessionctl.ProviderUpdate) bool { _, ok := rowOf(u, "b"); return !ok && len(u.Sessions) == 2 })
}

func TestObserverNeverShowsTitleGenerationThreads(t *testing.T) {
	d := newFakeDaemon(t)
	d.setThreads(catalogThread("a", 1), titleThread("title-1"))
	d.setLoaded("title-1", active())
	p, _ := newObservedProvider(t, d, nil)
	ch := observe(t, p)
	c := d.waitReady(t)
	hiddenSeen := func(u sessionctl.ProviderUpdate) {
		for _, s := range u.Sessions {
			if s.Key.ID == "title-1" || s.Key.ID == "title-2" {
				t.Errorf("title-generation thread became a row: %+v", s)
			}
		}
	}
	waitFor(t, ch, "initial", activityIs("a", session.ActivityIdle), hiddenSeen)
	lists := d.callCount("thread/list")

	c.notify(notifyStarted, map[string]any{"thread": titleThread("title-2")})
	c.statusChanged("title-2", active())
	c.statusChanged("title-1", idle)
	// A visible change afterwards proves the notifications above were
	// processed (the reader handles them in order).
	c.statusChanged("a", active())
	waitFor(t, ch, "a working", activityIs("a", session.ActivityWorking), hiddenSeen)
	if n := d.callCount("thread/list"); n != lists {
		t.Fatalf("hidden threads must not trigger catalog resyncs: thread/list %d -> %d", lists, n)
	}
}

func TestObserveCancelStopsConnectionAndLoops(t *testing.T) {
	d := newFakeDaemon(t)
	d.setThreads(catalogThread("a", 1))
	p, _ := newObservedProvider(t, d, nil)
	ctx, cancel := context.WithCancel(context.Background())
	ch := p.Observe(ctx)
	c := d.waitReady(t)
	waitFor(t, ch, "initial", activityIs("a", session.ActivityIdle))

	cancel()
	timeout := time.After(5 * time.Second)
	for open := true; open; {
		select {
		case _, open = <-ch:
		case <-timeout:
			t.Fatal("observer channel did not close")
		}
	}
	p.obs.mu.Lock()
	stopped := p.obs.stopped
	p.obs.mu.Unlock()
	select {
	case <-stopped:
	case <-timeout:
		t.Fatal("runtime loops did not stop")
	}
	select {
	case <-c.closed:
	case <-timeout:
		t.Fatal("connection was not closed")
	}
	// Nothing reconnects afterwards.
	select {
	case <-d.ready:
		t.Fatal("runtime reconnected after cancellation")
	case <-time.After(100 * time.Millisecond):
	}
}

// List and notifications racing on the same provider must stay race-free
// (run under -race).
func TestListAndNotificationsRace(t *testing.T) {
	d := newFakeDaemon(t)
	d.setThreads(catalogThread("a", 1), catalogThread("b", 2))
	api := &fakeAPI{rows: []Thread{catalogThread("a", 1), catalogThread("b", 2)}}
	p, _ := newObservedProvider(t, d, api)
	ch := observe(t, p)
	c := d.waitReady(t)
	waitFor(t, ch, "initial", activityIs("a", session.ActivityIdle))

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range 20 {
			_, _ = p.List(context.Background(), false)
		}
	}()
	go func() {
		defer wg.Done()
		for i := range 20 {
			if i%2 == 0 {
				c.statusChanged("a", active())
			} else {
				c.statusChanged("a", idle)
			}
		}
	}()
	wg.Wait()
	c.statusChanged("b", active())
	waitFor(t, ch, "settled", func(u sessionctl.ProviderUpdate) bool {
		return activityIs("b", session.ActivityWorking)(u) && activityIs("a", session.ActivityIdle)(u) && len(u.Sessions) == 2
	})
}

// A thread/read the app-server answers with an error leaves the status
// unestablished: the snapshot fails and is retried, and nothing -- in
// particular no false notLoaded -> Idle -- is published until a snapshot
// succeeds.
func TestSnapshotReadErrorFailsClosedBeforeAuthority(t *testing.T) {
	d := newFakeDaemon(t)
	d.setThreads(catalogThread("a", 1))
	d.setLoaded("a", active())
	d.failNext("thread/read", 3)
	p, _ := newObservedProvider(t, d, nil)
	ch := observe(t, p)

	u := waitFor(t, ch, "first publication", func(sessionctl.ProviderUpdate) bool { return true })
	if !activityIs("a", session.ActivityWorking)(u) || u.Warning != nil {
		t.Fatalf("the first publication must be the confirmed native status, got %+v", u)
	}
	if n := d.callCount("thread/read"); n < 4 {
		t.Fatalf("failed reads must be retried on new snapshots, thread/read calls=%d", n)
	}
}

func TestSnapshotReadErrorAfterAuthorityStaysUnavailable(t *testing.T) {
	d := newFakeDaemon(t)
	d.setThreads(catalogThread("a", 1))
	d.setLoaded("a", active())
	p, _ := newObservedProvider(t, d, nil)
	ch := observe(t, p)
	d.waitReady(t)
	waitFor(t, ch, "a working", activityIs("a", session.ActivityWorking))

	d.failNext("thread/read", 3)
	d.dropAll()
	var sawUnavailable bool
	check := func(u sessionctl.ProviderUpdate) {
		s, _ := rowOf(u, "a")
		switch {
		case s.Activity == session.ActivityUnknown && s.Runtime == session.RuntimeUnknown && u.Warning != nil:
			sawUnavailable = true
		case s.Activity == session.ActivityWorking && u.Warning == nil && sawUnavailable:
		default:
			t.Errorf("neither unavailable nor the confirmed status: %+v (warning %v)", s, u.Warning)
		}
	}
	waitFor(t, ch, "reconverged", func(u sessionctl.ProviderUpdate) bool {
		return sawUnavailable && activityIs("a", session.ActivityWorking)(u) && u.Warning == nil
	}, check)
	if n := d.callCount("thread/read"); n < 5 {
		t.Fatalf("thread/read calls=%d", n)
	}
}

// A failed catalog resync is not dropped: the connection is abandoned and
// the next snapshot converges on the current catalog without any further
// notification.
func TestCatalogResyncFailureReconnectsAndConverges(t *testing.T) {
	d := newFakeDaemon(t)
	d.setThreads(catalogThread("a", 1))
	p, _ := newObservedProvider(t, d, nil)
	ch := observe(t, p)
	c := d.waitReady(t)
	waitFor(t, ch, "initial", activityIs("a", session.ActivityIdle))

	d.setThreads(catalogThread("a", 1), catalogThread("b", 2))
	d.failNext("thread/list", 1)
	// b only becomes visible through the catalog re-read (unlike a
	// thread/started, which shows its thread at once).
	c.notify(notifyUnarchived, map[string]any{"threadId": "b"})

	var sawUnavailable bool
	waitFor(t, ch, "b listed", func(u sessionctl.ProviderUpdate) bool {
		_, ok := rowOf(u, "b")
		return ok && u.Warning == nil
	}, func(u sessionctl.ProviderUpdate) {
		if u.Warning != nil {
			sawUnavailable = true
			if s, _ := rowOf(u, "a"); s.Activity != session.ActivityUnknown || s.Runtime != session.RuntimeUnknown {
				t.Errorf("unavailable row=%+v", s)
			}
		}
	})
	if !sawUnavailable {
		t.Fatal("a failed resync must make the connection unavailable before it converges")
	}
	d.waitReady(t) // the reconnect that converged
}

// Losing the daemon only affects what the daemon observed: provisional
// runs keep the existing managed-run semantics.
func TestDisconnectKeepsProvisionalRunSemantics(t *testing.T) {
	d := newFakeDaemon(t)
	d.setThreads(catalogThread("a", 2), catalogThread("m", 1))
	d.setLoaded("a", active())
	p, _ := newObservedProvider(t, d, nil)
	for _, r := range []localstate.Run{
		{ID: "run-1", Provider: "codex", State: "running", CWD: "/work", StartedAt: time.Unix(5, 0)},
		{ID: "run-2", Provider: "codex", State: "failed", CWD: "/work", StartedAt: time.Unix(6, 0), Error: "exec failed"},
		{ID: "run-3", Provider: "codex", State: "running", SessionID: "m", CWD: "/work", StartedAt: time.Unix(1, 0)},
	} {
		if err := p.Store.SaveRun(r); err != nil {
			t.Fatal(err)
		}
	}
	ch := observe(t, p)
	d.waitReady(t)
	waitFor(t, ch, "a working", activityIs("a", session.ActivityWorking))

	d.stop()
	u := waitFor(t, ch, "unavailable", func(u sessionctl.ProviderUpdate) bool { return u.Err == nil && u.Warning != nil })
	want := map[string]observation{
		"a":     {session.ActivityUnknown, session.RuntimeUnknown},
		"m":     {session.ActivityUnknown, session.RuntimeDetached}, // managed thread: existing Runtime override
		"run-1": {session.ActivityStarting, session.RuntimeDetached},
		"run-2": {session.ActivityFailed, session.RuntimeStopped},
	}
	if len(u.Sessions) != len(want) {
		t.Fatalf("rows=%+v", u.Sessions)
	}
	for id, w := range want {
		s, ok := rowOf(u, id)
		if !ok || (observation{s.Activity, s.Runtime}) != w {
			t.Fatalf("%s=%+v, want %+v", id, s, w)
		}
	}
}

// A thread that leaves the catalog loses its cached status: when it comes
// back without being loaded again (and without any status or closed
// notification) it is notLoaded, never its old live status. The catalog
// changes are driven by the daemon's broadcasts, whether another client
// or this Provider's own Archive/Unarchive caused them.
func TestCatalogRemovalDropsCachedStatus(t *testing.T) {
	a := session.Key{Provider: session.ProviderCodex, ID: "a"}
	paths := map[string]struct{ archive, unarchive func(*Provider, *fakeConn) }{
		"daemon notifications": {
			// The daemon tore the runtime down without a thread/closed
			// notification.
			archive: func(_ *Provider, c *fakeConn) {
				c.d.setThreads()
				c.d.setLoaded("a", notLoadedSt)
				c.notify(notifyArchived, map[string]any{"threadId": "a"})
			},
			unarchive: func(_ *Provider, c *fakeConn) {
				c.d.setThreads(catalogThread("a", 1))
				c.notify(notifyUnarchived, map[string]any{"threadId": "a"})
			},
		},
		"provider actions": {
			// Archive refuses a running thread, so it settles first; its
			// loaded idle status must not outlive the archive either.
			archive: func(p *Provider, c *fakeConn) {
				c.statusChanged("a", idle)
				if err := p.Archive(context.Background(), a); err != nil {
					t.Fatal(err)
				}
			},
			unarchive: func(p *Provider, _ *fakeConn) {
				if err := p.Unarchive(context.Background(), a); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for name, path := range paths {
		t.Run(name, func(t *testing.T) {
			d := newFakeDaemon(t)
			d.setThreads(catalogThread("a", 1))
			d.setLoaded("a", active())
			p, _ := newObservedProvider(t, d, nil)
			ch := observe(t, p)
			c := d.waitReady(t)
			waitFor(t, ch, "a working", activityIs("a", session.ActivityWorking))

			path.archive(p, c)
			waitFor(t, ch, "a gone", func(u sessionctl.ProviderUpdate) bool { _, ok := rowOf(u, "a"); return u.Err == nil && !ok })

			path.unarchive(p, c)
			u := waitFor(t, ch, "a back", func(u sessionctl.ProviderUpdate) bool { _, ok := rowOf(u, "a"); return ok })
			if s, _ := rowOf(u, "a"); s.Activity != session.ActivityIdle || s.Runtime != session.RuntimeNone {
				t.Fatalf("an unarchived, unloaded thread must be notLoaded, not its stale status: %s/%s", s.Activity, s.Runtime)
			}
		})
	}
}

// A catalog install rejected as stale leaves the status cache alone, and a
// status for a thread no catalog has listed yet survives catalog installs
// (a later one that lists the thread needs it).
func TestCatalogStatusPruneOnlyOnInstalledRemoval(t *testing.T) {
	rt := newCodexRuntime(nil)
	stale := rt.beginCatalogFetch()
	current := rt.beginCatalogFetch()
	rt.installCatalog(current, []Thread{catalogThread("a", 1)})
	rt.mu.Lock()
	rt.live = true
	rt.status = map[string]ThreadStatus{"a": active(), "new": active()}
	rt.mu.Unlock()

	if rt.installCatalog(stale, nil) {
		t.Fatal("stale fetch must not install")
	}
	if v := rt.view(); v.status["a"].Type != statusActive {
		t.Fatalf("a stale fetch must not prune the current status: %+v", v.status)
	}

	rt.installCatalog(rt.beginCatalogFetch(), []Thread{catalogThread("b", 1)})
	v := rt.view()
	if _, ok := v.status["a"]; ok {
		t.Fatalf("a thread removed from the installed catalog must lose its status: %+v", v.status)
	}
	if v.status["new"].Type != statusActive {
		t.Fatalf("a not-yet-listed thread's status must survive: %+v", v.status)
	}
}
