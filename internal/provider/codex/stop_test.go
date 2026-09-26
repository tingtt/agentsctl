package codex

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/tingtt/agentsctl/internal/session"
)

func prepareActiveStop(t *testing.T, status ThreadStatus) dispatchFixture {
	t.Helper()
	f := newDispatchFixture(t)
	f.d.setThreads(catalogThread("thread-1", 1))
	f.d.startActiveTurn("thread-1", "turn-1", status)
	return f
}

func TestStopInterruptsAuthoritativelyResolvedActiveTurn(t *testing.T) {
	f := prepareActiveStop(t, active())
	if err := f.p.Stop(context.Background(), codexKey("thread-1")); err != nil {
		t.Fatal(err)
	}
	f.d.waitClosed(t, 1)
	requireEvents(t, f.d, "ensure", "initialize", "thread/read", "thread/turns/list", "turn/interrupt", "thread/read", "close")
	params := decodeParams(t, f.d.requestsOf("turn/interrupt")[0])
	if want := map[string]any{"threadId": "thread-1", "turnId": "turn-1"}; !reflect.DeepEqual(params, want) {
		t.Fatalf("turn/interrupt params = %v, want %v", params, want)
	}
	if active := f.d.activeTurn("thread-1"); active != "" {
		t.Fatalf("turn remains active: %s", active)
	}
	if status := f.d.loadedStatus("thread-1"); status.Type != statusIdle {
		t.Fatalf("thread status = %+v, want idle", status)
	}
}

func TestStopInterruptsUserWaitsAndResolvesPendingRequest(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  ThreadStatus
		pending bool
	}{
		{name: "approval", status: active(flagWaitingOnApproval), pending: true},
		{name: "user input", status: active(flagWaitingOnUserInput), pending: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := prepareActiveStop(t, tc.status)
			f.d.setPendingRequest("thread-1", tc.pending)
			if err := f.p.Stop(context.Background(), codexKey("thread-1")); err != nil {
				t.Fatal(err)
			}
			if f.d.hasPendingRequest("thread-1") {
				t.Fatal("pending request remained after interrupt")
			}
			if status := f.d.loadedStatus("thread-1"); status.Type != statusIdle {
				t.Fatalf("thread status = %+v, want idle", status)
			}
		})
	}
}

func TestStopAlreadyIdleSucceedsWithoutInterrupt(t *testing.T) {
	f := newDispatchFixture(t)
	f.d.setThreads(catalogThread("thread-1", 1))
	f.d.setLoaded("thread-1", idle)
	if err := f.p.Stop(context.Background(), codexKey("thread-1")); err != nil {
		t.Fatal(err)
	}
	f.d.waitClosed(t, 1)
	requireEvents(t, f.d, "ensure", "initialize", "thread/read", "close")
	if f.d.callCount("turn/interrupt") != 0 {
		t.Fatal("idle Stop sent turn/interrupt")
	}
}

func TestStopTurnCompletesDuringAuthoritativeLookup(t *testing.T) {
	f := prepareActiveStop(t, active())
	f.d.before["thread/turns/list"] = func(c *fakeConn) {
		f.d.completeTurn(c, "thread-1", "turn-1", turnStatusCompleted)
	}
	if err := f.p.Stop(context.Background(), codexKey("thread-1")); err != nil {
		t.Fatalf("Stop = %v, want already-completed success", err)
	}
	if f.d.callCount("turn/interrupt") != 0 {
		t.Fatal("Stop interrupted after the target turn had completed")
	}
}

func TestStopStaleTurnRaceNeverInterruptsReplacement(t *testing.T) {
	f := prepareActiveStop(t, active())
	f.d.before["turn/interrupt"] = func(c *fakeConn) {
		f.d.completeTurn(c, "thread-1", "turn-1", turnStatusCompleted)
		f.d.startActiveTurn("thread-1", "turn-2", active())
		f.d.broadcast(notifyStatusChanged, map[string]any{"threadId": "thread-1", "status": active()})
	}
	err := f.p.Stop(context.Background(), codexKey("thread-1"))
	if err == nil || !strings.Contains(err.Error(), "different active turn turn-2") {
		t.Fatalf("Stop = %v, want fail-closed replacement-turn error", err)
	}
	if active := f.d.activeTurn("thread-1"); active != "turn-2" {
		t.Fatalf("replacement turn = %q, want turn-2 untouched", active)
	}
	if f.d.callCount("turn/interrupt") != 1 {
		t.Fatalf("turn/interrupt calls = %d, want no retry", f.d.callCount("turn/interrupt"))
	}
}

func TestStopRefusesExternalAndUnknownRuntime(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     ThreadStatus
		writerFree bool
	}{
		{name: "external", status: notLoadedSt, writerFree: false},
		{name: "unknown", status: ThreadStatus{Type: "futureStatus"}, writerFree: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newDispatchFixture(t)
			f.p.writerFree = func(string) bool { return tc.writerFree }
			f.d.setThreads(catalogThread("thread-1", 1))
			f.d.setLoaded("thread-1", tc.status)
			if err := f.p.Stop(context.Background(), codexKey("thread-1")); err == nil {
				t.Fatal("Stop succeeded without interrupt authority")
			}
			if f.d.callCount("turn/interrupt") != 0 {
				t.Fatal("unsafe Stop sent turn/interrupt")
			}
		})
	}
}

func TestStopEnsureFailureContactsNoDaemon(t *testing.T) {
	f := newDispatchFixture(t)
	f.lifecycle.err = errBoom
	if err := f.p.Stop(context.Background(), session.Key{Provider: session.ProviderCodex, ID: "thread-1"}); !errors.Is(err, errBoom) {
		t.Fatalf("Stop = %v, want Ensure failure", err)
	}
	requireEvents(t, f.d, "ensure")
}
