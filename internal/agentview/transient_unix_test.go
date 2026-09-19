//go:build darwin || linux

package agentview

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tingtt/agentsctl/internal/session"
	"github.com/tingtt/agentsctl/internal/sessionctl"
)

// scriptedProvider lists a scripted catalog: call n (from 0) returns
// script[min(n, len-1)]. gate, when set, holds every List until the test
// sends on it, so a "slow" List is under deterministic control.
type scriptedProvider struct {
	*fakeProvider
	mu     sync.Mutex
	script [][]session.Session
	calls  atomic.Int32
	gate   chan struct{}
	err    error
}

func (p *scriptedProvider) List(ctx context.Context, _ bool) ([]session.Session, error) {
	n := int(p.calls.Add(1)) - 1
	if p.gate != nil {
		select {
		case <-p.gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return nil, p.err
	}
	rows := p.script[min(n, len(p.script)-1)]
	return append([]session.Session(nil), rows...), nil
}

// refreshCounter is a provider that counts the background Refresh calls a
// full reload requests from a sessionctl.Refresher.
type refreshCounter struct {
	*scriptedProvider
	refreshes atomic.Int32
}

func (p *refreshCounter) Refresh(context.Context) { p.refreshes.Add(1) }

func codexRow(id, name string, activity session.Activity, prev ...session.Key) session.Session {
	return session.Session{Key: session.Key{Provider: session.ProviderCodex, ID: id}, Name: name, CWD: "/work", Activity: activity, PreviousKeys: prev,
		Actions: session.Actions{session.ActionOpen: {Available: true}, session.ActionStop: {Available: true}}}
}

// manualClock replaces the transient refresh timer: each armed round waits
// for the test to send on fire, and the requested intervals are recorded.
type manualClock struct {
	fire      chan time.Time
	mu        sync.Mutex
	intervals []time.Duration
}

func newManualClock(rt *Runtime) *manualClock {
	c := &manualClock{fire: make(chan time.Time)}
	rt.transient.after = func(d time.Duration) <-chan time.Time {
		c.mu.Lock()
		c.intervals = append(c.intervals, d)
		c.mu.Unlock()
		return c.fire
	}
	return c
}

func (c *manualClock) armed() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.intervals)
}

func transientRuntime(providers ...sessionctl.Source) *Runtime {
	rt := &Runtime{Controller: sessionctl.Controller{Providers: providers, Pins: &fakePins{}}, State: NewState(), CWD: "/work"}
	rt.syncReload(context.Background())
	return rt
}

func TestTransientRefreshIsScheduledOnlyWhileAProviderListsAStartingSession(t *testing.T) {
	starting := &scriptedProvider{fakeProvider: &fakeProvider{id: session.ProviderCodex}, script: [][]session.Session{{codexRow("run-1", "Starting", session.ActivityStarting)}}}
	rt := transientRuntime(starting)
	clock := newManualClock(rt)

	rt.transient.schedule(rt)
	if rt.transient.tick == nil || clock.armed() != 1 {
		t.Fatalf("a Starting session must arm a refresh round (armed=%d)", clock.armed())
	}
	rt.transient.schedule(rt)
	if clock.armed() != 1 {
		t.Fatalf("a round is already pending; armed=%d", clock.armed())
	}

	settled := &scriptedProvider{fakeProvider: &fakeProvider{id: session.ProviderCodex}, script: [][]session.Session{{codexRow("t", "foo", session.ActivityIdle)}}}
	rt = transientRuntime(settled)
	newManualClock(rt)
	rt.transient.schedule(rt)
	if rt.transient.tick != nil {
		t.Fatal("no Starting session: no timer must be scheduled")
	}
}

func TestTransientRefreshListsOnlyStartingProvidersAndNeverRequestsARefresh(t *testing.T) {
	idle := &refreshCounter{scriptedProvider: &scriptedProvider{
		fakeProvider: &fakeProvider{id: session.ProviderChatGPT},
		script:       [][]session.Session{{{Key: session.Key{Provider: session.ProviderChatGPT, ID: "c"}, CWD: "/work", Activity: session.ActivityIdle}}},
	}}
	starting := &scriptedProvider{fakeProvider: &fakeProvider{id: session.ProviderCodex}, script: [][]session.Session{{codexRow("run-1", "Starting", session.ActivityStarting)}}}
	rt := transientRuntime(idle, starting)
	newManualClock(rt)
	idleLists, refreshes := idle.calls.Load(), idle.refreshes.Load()
	if refreshes != 1 {
		t.Fatalf("setup: the full reload should have requested one Refresh, got %d", refreshes)
	}

	rt.transient.schedule(rt)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rt.transient.refresh(ctx, rt)
	res := <-rt.transient.results
	if res.ps.Provider != session.ProviderCodex {
		t.Fatalf("targeted List of %s, want codex", res.ps.Provider)
	}
	if idle.calls.Load() != idleLists || idle.refreshes.Load() != refreshes {
		t.Fatalf("the idle provider must be neither Listed (%d->%d) nor Refreshed (%d->%d)", idleLists, idle.calls.Load(), refreshes, idle.refreshes.Load())
	}
	if starting.calls.Load() != 2 {
		t.Fatalf("codex Lists = %d, want 1 from the reload + 1 targeted", starting.calls.Load())
	}
}

func TestTransientRefreshNeverOverlapsAndStopsOnceSettled(t *testing.T) {
	p := &scriptedProvider{
		fakeProvider: &fakeProvider{id: session.ProviderCodex},
		script: [][]session.Session{
			{codexRow("run-1", "Starting", session.ActivityStarting)},
			{codexRow("run-1", "Starting", session.ActivityStarting)}, // targeted, still starting
			{codexRow("thread-1", "foo", session.ActivityIdle, session.Key{Provider: session.ProviderCodex, ID: "run-1"})},
		},
	}
	rt := transientRuntime(p)
	clock := newManualClock(rt)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p.gate = make(chan struct{})
	rt.transient.schedule(rt)
	rt.transient.refresh(ctx, rt) // targeted List #2, held open by the gate
	waitFor(t, 2*time.Second, func() bool { return p.calls.Load() == 2 })

	rt.transient.refresh(ctx, rt) // a second round while #2 is still running
	rt.transient.schedule(rt)
	if p.calls.Load() != 2 || rt.transient.tick != nil || clock.armed() != 1 {
		t.Fatalf("overlap: Lists=%d tick armed=%v rounds armed=%d, want the in-flight List to be the only one", p.calls.Load(), rt.transient.tick != nil, clock.armed())
	}

	p.gate <- struct{}{}
	if !rt.transient.apply(rt, <-rt.transient.results) {
		t.Fatal("a current result must update State")
	}
	if rt.transient.tick == nil || clock.armed() != 2 {
		t.Fatalf("still Starting after the result: the next round must be armed (armed=%d)", clock.armed())
	}

	rt.transient.refresh(ctx, rt)
	p.gate <- struct{}{} // targeted List #3 settles the session
	waitFor(t, 2*time.Second, func() bool { return p.calls.Load() == 3 })
	rt.transient.apply(rt, <-rt.transient.results)
	if rt.transient.tick != nil || clock.armed() != 2 {
		t.Fatalf("no transient session left: polling must stop (armed=%d)", clock.armed())
	}
	for _, r := range rt.State.Rows {
		if r.Activity == session.ActivityStarting {
			t.Fatalf("Starting row remains: %+v", r)
		}
	}
}

func TestTransientRefreshIsBoundedAndResetByReload(t *testing.T) {
	p := &scriptedProvider{fakeProvider: &fakeProvider{id: session.ProviderCodex}, script: [][]session.Session{{codexRow("run-1", "Starting", session.ActivityStarting)}}}
	rt := transientRuntime(p)
	clock := newManualClock(rt)

	rt.transient.attempts = maxTransientRefreshes
	rt.transient.schedule(rt)
	if rt.transient.tick != nil {
		t.Fatal("a spent budget must not schedule another round")
	}
	rt.syncReload(context.Background())
	if rt.transient.attempts != 0 {
		t.Fatalf("a reload starts a new budget, attempts=%d", rt.transient.attempts)
	}
	rt.transient.schedule(rt)
	if rt.transient.tick == nil || clock.armed() != 1 {
		t.Fatal("after a reload the round must be schedulable again")
	}
}

func TestTransientRefreshErrorKeepsLastKnownRowsAndWarns(t *testing.T) {
	p := &scriptedProvider{fakeProvider: &fakeProvider{id: session.ProviderCodex}, script: [][]session.Session{{codexRow("run-1", "Starting", session.ActivityStarting)}}}
	rt := transientRuntime(p)
	newManualClock(rt)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p.mu.Lock()
	p.err = errors.New("app-server unavailable")
	p.mu.Unlock()
	rt.transient.schedule(rt)
	rt.transient.refresh(ctx, rt)
	rt.transient.apply(rt, <-rt.transient.results)

	if len(rt.State.Rows) != 1 || rt.State.Rows[0].Key.ID != "run-1" {
		t.Fatalf("a failed targeted List must keep the last-known rows: %+v", rt.State.Rows)
	}
	if rt.State.Warnings[session.ProviderCodex] == nil {
		t.Fatal("the failure must surface as the provider's warning")
	}
	if rt.transient.tick == nil {
		t.Fatal("the session is still Starting, so a later round is still due")
	}
}

func TestTransientRefreshDropsResultStartedBeforeALaterReload(t *testing.T) {
	p := &scriptedProvider{fakeProvider: &fakeProvider{id: session.ProviderCodex}, script: [][]session.Session{
		{codexRow("run-1", "Starting", session.ActivityStarting)},
		{codexRow("thread-1", "foo", session.ActivityIdle)},
	}}
	rt := transientRuntime(p)
	newManualClock(rt)
	stale := transientResult{gen: rt.catalogGen - 1, ps: sessionctl.ProviderSnapshot{Provider: session.ProviderCodex, Sessions: []session.Session{codexRow("thread-1", "foo", session.ActivityIdle)}, ListOwnsStatus: true}}
	if rt.transient.apply(rt, stale) {
		t.Fatal("a result from before the latest reload must not be applied")
	}
	if rt.State.Rows[0].Key.ID != "run-1" {
		t.Fatalf("rows changed by a stale result: %+v", rt.State.Rows)
	}
}

// TestRenameOnlySessionConvergesWithoutManualReload drives the real event
// loop: a rename-only Codex session listed as Starting (Waiting rename)
// becomes its named thread purely through timer rounds -- the test never
// sends a key (no Ctrl+L) until it ends the loop.
func TestRenameOnlySessionConvergesWithoutManualReload(t *testing.T) {
	runKey := session.Key{Provider: session.ProviderCodex, ID: "run-1"}
	waiting := codexRow("run-1", "Starting (Waiting rename)", session.ActivityStarting)
	waiting.Actions = session.Actions{session.ActionOpen: {Reason: "Codex rename-only session is still starting"}, session.ActionStop: {Available: true}}
	named := codexRow("thread-1", "foo", session.ActivityIdle, runKey)
	p := &scriptedProvider{fakeProvider: &fakeProvider{id: session.ProviderCodex}, script: [][]session.Session{{waiting}, {waiting}, {waiting}, {named}}}
	pins := &fakePins{pinned: map[string]bool{runKey.String(): true}}

	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer inR.Close()
	defer inW.Close()
	out := &syncBuffer{}
	rt := &Runtime{Controller: sessionctl.Controller{Providers: []sessionctl.Source{p}, Pins: pins}, State: NewState(), CWD: "/work", Input: inR, Output: out}
	clock := newManualClock(rt)

	// The loop itself loads the catalog (as Run does at startup) and arms
	// the first round from that arrival: nothing here schedules a round.
	rt.requestReload(context.Background())
	keyIn := make(chan keyResult)
	loopDone := make(chan error, 1)
	go func() {
		loopDone <- rt.eventLoop(context.Background(), bufio.NewReader(rt.Input), func(*bufio.Reader) (KeyEvent, error) { r := <-keyIn; return r.ev, r.err })
	}()
	waitFor(t, 2*time.Second, func() bool { return strings.Contains(latestFrame(out.String()), "Starting (Waiting rename)") })

	for round := 1; round <= 3; round++ {
		select { // received only once the loop has an armed round
		case clock.fire <- time.Now():
		case <-time.After(2 * time.Second):
			t.Fatalf("round %d: the loop never armed a refresh round", round)
		}
		round := round
		waitFor(t, 2*time.Second, func() bool { return int(p.calls.Load()) == 1+round })
	}
	waitFor(t, 2*time.Second, func() bool {
		f := latestFrame(out.String())
		return strings.Contains(f, "foo") && !strings.Contains(f, "Starting")
	})

	keyIn <- keyResult{err: io.EOF}
	select {
	case <-loopDone:
	case <-time.After(2 * time.Second):
		t.Fatal("eventLoop did not exit")
	}
	got, ok := rt.State.SelectedRow()
	if !ok || got.Key.ID != "thread-1" || got.Name != "foo" || !got.Actions.Available(session.ActionOpen) {
		t.Fatalf("selected = %+v ok=%v, want the named thread, openable", got, ok)
	}
	for _, r := range rt.State.Rows {
		if r.Key == runKey || r.Activity == session.ActivityStarting {
			t.Fatalf("provisional row remains: %+v", r)
		}
	}
	if pins.pinned[runKey.String()] || !pins.pinned["codex:thread-1"] {
		t.Fatalf("the pin must follow the session to its thread key: %v", pins.pinned)
	}
	if rt.transient.tick != nil {
		t.Fatal("polling must have stopped")
	}
}
