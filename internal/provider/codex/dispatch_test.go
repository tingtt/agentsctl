package codex

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tingtt/agentsctl/internal/localstate"
	"github.com/tingtt/agentsctl/internal/process"
	"github.com/tingtt/agentsctl/internal/session"
	"github.com/tingtt/agentsctl/internal/sessionctl"
)

// loggingLifecycle ensures d, logging "ensure" into d's event log so a test
// can place readySocket in the RPC sequence.
type loggingLifecycle struct {
	d     *fakeDaemon
	err   error
	calls int
}

func (l *loggingLifecycle) Ensure(context.Context) (DaemonInfo, error) {
	l.d.mu.Lock()
	l.calls++
	l.d.events = append(l.d.events, "ensure")
	l.d.mu.Unlock()
	if l.err != nil {
		return DaemonInfo{}, l.err
	}
	return DaemonInfo{Status: "alreadyRunning", SocketPath: l.d.socket}, nil
}

type dispatchFixture struct {
	p         *Provider
	d         *fakeDaemon
	lifecycle *loggingLifecycle
	legacy    *fakeManagedRuntime
	api       *fakeAPI
	store     *localstate.Store
}

func newDispatchFixture(t *testing.T) dispatchFixture {
	t.Helper()
	d := newFakeDaemon(t)
	f := dispatchFixture{
		d:         d,
		lifecycle: &loggingLifecycle{d: d},
		legacy:    &fakeManagedRuntime{},
		api:       &fakeAPI{},
		store:     localstate.New(filepath.Join(t.TempDir(), "state.json")),
	}
	f.p = &Provider{API: f.api, Store: f.store, Runtime: f.legacy, Daemon: f.lifecycle, writerFree: func(string) bool { return true }}
	return f
}

// requireNoLegacySideEffects checks that Dispatch left nothing behind
// outside the daemon: no local run, no baseline List, no legacy Stop.
func (f dispatchFixture) requireNoLegacySideEffects(t *testing.T) {
	t.Helper()
	if runs := runsOf(t, f.store); len(runs) != 0 {
		t.Fatalf("Dispatch recorded local runs: %+v", runs)
	}
	if f.api.lists != 0 {
		t.Fatalf("Dispatch listed the catalog %d times (baseline capture)", f.api.lists)
	}
	if len(f.legacy.stopped) != 0 {
		t.Fatalf("Dispatch stopped legacy runs %v", f.legacy.stopped)
	}
}

func decodeParams(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func requireEvents(t *testing.T, d *fakeDaemon, want ...string) {
	t.Helper()
	if got := d.eventLog(); !slices.Equal(got, want) {
		t.Fatalf("daemon saw %q, want %q", got, want)
	}
}

func textInput(text string) []any {
	return []any{map[string]any{"type": "text", "text": text, "textElements": []any{}}}
}

// TestDispatchStartsThreadAndTurnOnOwnConnection pins the whole ordinary
// Dispatch: the exact RPC sequence on a connection of its own, the exact
// thread/start and turn/start params (nothing overridden beyond them, the
// prompt verbatim), and a return value that is only the canonical key.
func TestDispatchStartsThreadAndTurnOnOwnConnection(t *testing.T) {
	for _, prompt := range []string{
		"implement issue #46",
		"line one\nline two\n",
		"/review the diff",
		"/renamex foo",
		"hello /rename foo",
		"/rename foo\nimplement issue #46",
	} {
		t.Run(prompt, func(t *testing.T) {
			f := newDispatchFixture(t)
			got, err := f.p.Dispatch(context.Background(), prompt, "/work/repo")
			if err != nil {
				t.Fatal(err)
			}
			f.d.waitClosed(t, 1)
			requireEvents(t, f.d, "ensure", "initialize", "thread/start", "turn/start", "thread/unsubscribe", "close")
			if n := f.d.callCount("initialized"); n != 1 {
				t.Fatalf("initialized sent %d times", n)
			}

			start := decodeParams(t, f.d.requestsOf("thread/start")[0])
			if want := map[string]any{"cwd": "/work/repo", "ephemeral": false, "threadSource": "user"}; !reflect.DeepEqual(start, want) {
				t.Fatalf("thread/start params = %v, want exactly %v", start, want)
			}
			turn := decodeParams(t, f.d.requestsOf("turn/start")[0])
			if want := map[string]any{"threadId": "thread-new-1", "turnTrigger": "user", "input": textInput(prompt)}; !reflect.DeepEqual(turn, want) {
				t.Fatalf("turn/start params = %v, want exactly %v", turn, want)
			}
			unsub := decodeParams(t, f.d.requestsOf("thread/unsubscribe")[0])
			if want := map[string]any{"threadId": "thread-new-1"}; !reflect.DeepEqual(unsub, want) {
				t.Fatalf("thread/unsubscribe params = %v", unsub)
			}

			if want := (session.Session{Key: codexKey("thread-new-1"), CWD: "/work/repo"}); !reflect.DeepEqual(got, want) {
				t.Fatalf("Dispatch = %+v, want only the canonical key and CWD", got)
			}
			f.requireNoLegacySideEffects(t)
		})
	}
}

// Only a rename waits for turn/started: an ordinary Dispatch returns once
// the turn is accepted, even if turn/started never comes.
func TestDispatchDoesNotWaitForTurnStarted(t *testing.T) {
	f := newDispatchFixture(t)
	f.d.turnStarted = "never"
	begin := time.Now()
	if _, err := f.p.Dispatch(context.Background(), "prompt", "/work"); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(begin); elapsed >= dispatchCleanupTimeout/2 {
		t.Fatalf("Dispatch took %s, as if it waited for turn/started", elapsed)
	}
}

func TestDispatchEnsureFailureContactsNoDaemon(t *testing.T) {
	f := newDispatchFixture(t)
	f.lifecycle.err = errBoom
	if _, err := f.p.Dispatch(context.Background(), "prompt", "/work"); !errors.Is(err, errBoom) {
		t.Fatalf("Dispatch = %v, want the Ensure failure", err)
	}
	requireEvents(t, f.d, "ensure")
	if n := f.d.callCount("initialize"); n != 0 {
		t.Fatalf("dialed the daemon after Ensure failed (%d initialize)", n)
	}
	f.requireNoLegacySideEffects(t)
}

func TestDispatchControlSocketOverrideBypassesEnsure(t *testing.T) {
	f := newDispatchFixture(t)
	f.p.ControlSocket = f.d.socket
	f.lifecycle.err = errors.New("real daemon must not be ensured")
	if _, err := f.p.Dispatch(context.Background(), "prompt", "/work"); err != nil {
		t.Fatal(err)
	}
	if f.lifecycle.calls != 0 || f.d.callCount("turn/start") != 1 {
		t.Fatalf("Ensure calls=%d turn/start=%d", f.lifecycle.calls, f.d.callCount("turn/start"))
	}
}

func TestDispatchThreadStartFailureStartsNothing(t *testing.T) {
	for name, inject := range map[string]func(*fakeDaemon){
		"rpc error":       func(d *fakeDaemon) { d.failNext("thread/start", 1) },
		"empty thread id": func(d *fakeDaemon) { d.threadStartResult = map[string]any{"thread": map[string]any{"id": ""}} },
		"no thread":       func(d *fakeDaemon) { d.threadStartResult = map[string]any{} },
	} {
		t.Run(name, func(t *testing.T) {
			f := newDispatchFixture(t)
			inject(f.d)
			if _, err := f.p.Dispatch(context.Background(), "prompt", "/work"); err == nil {
				t.Fatal("want a Dispatch error")
			}
			f.d.waitClosed(t, 1)
			requireEvents(t, f.d, "ensure", "initialize", "thread/start", "close")
			f.requireNoLegacySideEffects(t)
		})
	}
}

// A thread/start that succeeded is not rolled back or disguised when
// turn/start then fails: Dispatch fails, and whatever the daemon itself
// does with the thread is left to its catalog.
func TestDispatchTurnStartFailureIsDispatchFailure(t *testing.T) {
	for name, inject := range map[string]func(*fakeDaemon){
		"rpc error": func(d *fakeDaemon) { d.failNext("turn/start", 1) },
		"empty turn id": func(d *fakeDaemon) {
			d.turnStartResult = map[string]any{"turn": map[string]any{"id": "", "status": "inProgress"}}
		},
		"no turn": func(d *fakeDaemon) { d.turnStartResult = map[string]any{} },
	} {
		t.Run(name, func(t *testing.T) {
			f := newDispatchFixture(t)
			inject(f.d)
			got, err := f.p.Dispatch(context.Background(), "prompt", "/work")
			if err == nil || !strings.Contains(err.Error(), "thread-new-1") {
				t.Fatalf("Dispatch = %v, want a failure naming the started thread", err)
			}
			if !reflect.DeepEqual(got, session.Session{}) {
				t.Fatalf("a failed Dispatch returned a session: %+v", got)
			}
			f.d.waitClosed(t, 1)
			requireEvents(t, f.d, "ensure", "initialize", "thread/start", "turn/start", "close")
			f.requireNoLegacySideEffects(t)
		})
	}
}

// Once turn/start was accepted the turn may be running, so no cleanup
// outcome may turn Dispatch into a failure a retry would duplicate.
func TestDispatchSucceedsWhateverUnsubscribeDoes(t *testing.T) {
	restore := dispatchCleanupTimeout
	dispatchCleanupTimeout = 100 * time.Millisecond
	t.Cleanup(func() { dispatchCleanupTimeout = restore })
	for name, inject := range map[string]func(*fakeDaemon){
		"unsubscribed":     func(d *fakeDaemon) { d.unsubscribeStatus = "unsubscribed" },
		"notSubscribed":    func(d *fakeDaemon) { d.unsubscribeStatus = "notSubscribed" },
		"notLoaded":        func(d *fakeDaemon) { d.unsubscribeStatus = "notLoaded" },
		"unknown status":   func(d *fakeDaemon) { d.unsubscribeStatus = "somethingNew" },
		"malformed status": func(d *fakeDaemon) { d.unsubscribeStatus = 42 },
		"rpc error":        func(d *fakeDaemon) { d.failNext("thread/unsubscribe", 1) },
		"connection lost":  func(d *fakeDaemon) { d.dropOn["thread/unsubscribe"] = true },
		"no response":      func(d *fakeDaemon) { d.hangOn["thread/unsubscribe"] = true },
	} {
		t.Run(name, func(t *testing.T) {
			f := newDispatchFixture(t)
			inject(f.d)
			got, err := f.p.Dispatch(context.Background(), "prompt", "/work")
			if err != nil {
				t.Fatalf("committed Dispatch failed on cleanup: %v", err)
			}
			if got.Key != codexKey("thread-new-1") {
				t.Fatalf("Key = %v", got.Key)
			}
			f.d.waitClosed(t, 1)
			if n := f.d.callCount("thread/unsubscribe"); n != 1 {
				t.Fatalf("thread/unsubscribe attempted %d times", n)
			}
			for _, method := range []string{"turn/interrupt", "thread/archive", "thread/delete"} {
				if n := f.d.callCount(method); n != 0 {
					t.Fatalf("%s called %d times after a cleanup failure", method, n)
				}
			}
			f.requireNoLegacySideEffects(t)
		})
	}
}

// A caller giving up after the commit point does not reach back into the
// started turn: cleanup still runs and Dispatch still reports the session.
func TestDispatchCancelledAfterCommitStillSucceeds(t *testing.T) {
	f := newDispatchFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	f.d.before["thread/unsubscribe"] = func(*fakeConn) { cancel() }
	got, err := f.p.Dispatch(ctx, "prompt", "/work")
	if err != nil || got.Key != codexKey("thread-new-1") {
		t.Fatalf("Dispatch = (%+v, %v), want the committed session", got, err)
	}
	f.d.waitClosed(t, 1)
	if n := f.d.callCount("turn/interrupt"); n != 0 {
		t.Fatalf("turn/interrupt called %d times", n)
	}
}

// Approval and user-input requests racing the Dispatch connection's
// subscription are never answered on the user's behalf.
func TestDispatchLeavesServerRequestsUnanswered(t *testing.T) {
	f := newDispatchFixture(t)
	serverRequest := func(id, method string) func(*fakeConn) {
		return func(c *fakeConn) {
			c.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": map[string]any{"threadId": "thread-new-1", "turnId": "turn-1", "itemId": "item-1"}})
		}
	}
	f.d.before["turn/start"] = serverRequest("server-request-1", "item/commandExecution/requestApproval")
	f.d.before["thread/unsubscribe"] = serverRequest("server-request-2", "item/tool/requestUserInput")

	got, err := f.p.Dispatch(context.Background(), "prompt", "/work")
	if err != nil || got.Key != codexKey("thread-new-1") {
		t.Fatalf("Dispatch = (%+v, %v)", got, err)
	}
	f.d.waitClosed(t, 1)
	requireEvents(t, f.d, "ensure", "initialize", "thread/start", "turn/start", "thread/unsubscribe", "close")
	if responses := f.d.responses(); len(responses) != 0 {
		t.Fatalf("agentsctl answered server requests: %s", responses)
	}
}

// The dispatched thread reaches Agent View only through the native
// catalog: the Observer publishes it from the daemon's own notifications
// and thread/list, under its canonical key, with no provisional row.
func TestDispatchedThreadIsPublishedByObserver(t *testing.T) {
	d := newFakeDaemon(t)
	d.setThreads(catalogThread("old", 1))
	p, _ := newObservedProvider(t, d, nil)
	p.Runtime = &fakeManagedRuntime{}
	ch := observe(t, p)
	waitFor(t, ch, "initial snapshot", func(u sessionctl.ProviderUpdate) bool {
		_, ok := rowOf(u, "old")
		return u.Err == nil && u.Warning == nil && ok
	})

	created, err := p.Dispatch(context.Background(), "prompt", "/work")
	if err != nil {
		t.Fatal(err)
	}
	u := waitFor(t, ch, "dispatched thread working", activityIs(created.Key.ID, session.ActivityWorking))
	if len(u.Sessions) != 2 {
		t.Fatalf("want the old and the dispatched thread only: %+v", u.Sessions)
	}
	row, _ := rowOf(u, created.Key.ID)
	if row.Key != codexKey("thread-new-1") || row.RunID != "" || len(row.PreviousKeys) != 0 || row.CWD != "/work" {
		t.Fatalf("dispatched row = %+v, want the canonical native row", row)
	}
	if !row.Actions[session.ActionOpen].Available {
		t.Fatalf("dispatched thread must be openable: %+v", row.Actions)
	}
	if !row.Actions[session.ActionStop].Available {
		t.Fatalf("active shared-daemon thread must offer native Stop: %+v", row.Actions)
	}
}

// A legacy managed run still waiting for its thread must not claim a
// thread Dispatch started on the shared daemon, even with the same CWD and
// a newer timestamp: that thread's writer lock is the daemon's, not the
// run's, and ownership is what reconcile requires.
func TestLegacyReconcileDoesNotClaimSharedDaemonThread(t *testing.T) {
	legacyRun := process.Identity{PID: 4242, StartTime: 1, UID: 501}
	daemon := process.Identity{PID: 999, StartTime: 2, UID: 501}
	for _, tc := range []struct {
		name      string
		writer    process.Identity
		wantBound string
	}{
		{name: "daemon owns writer", writer: daemon, wantBound: ""},
		// The same fixture binds once the run itself owns the writer, so
		// the case above fails only for want of ownership.
		{name: "legacy run owns writer", writer: legacyRun, wantBound: "thread-new"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := localstate.New(filepath.Join(t.TempDir(), "state.json"))
			if err := store.StartRun(localstate.Run{ID: "legacy", Provider: "codex", CWD: "/work", State: "running", StartedAt: time.Unix(100, 0), PID: legacyRun.PID, StartTime: legacyRun.StartTime, UID: legacyRun.UID, Baseline: []string{"old"}}); err != nil {
				t.Fatal(err)
			}
			api := &fakeAPI{home: "/home", rows: []Thread{
				{ID: "old", CWD: "/work", CreatedAt: 50},
				{ID: "thread-new", CWD: "/work", CreatedAt: 200},
			}}
			owners := map[string]process.Identity{writerLockPath("/home", "thread-new"): tc.writer}
			p := &Provider{API: api, Store: store, writerFree: func(string) bool { return true }, WriterOwner: func(path string, id process.Identity) (bool, error) {
				return owners[path] == id, nil
			}}
			if _, err := p.List(context.Background(), false); err != nil {
				t.Fatal(err)
			}
			if r := runsOf(t, store)["legacy"]; r.SessionID != tc.wantBound || r.Error != "" {
				t.Fatalf("legacy run = %+v, want SessionID %q", r, tc.wantBound)
			}
		})
	}
}
