package chatgpt

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/tingtt/agentsctl/internal/session"
	"github.com/tingtt/agentsctl/internal/sessionctl"
)

// browserResult is one queued sequenceBrowser.List outcome.
type browserResult struct {
	conversations []conversation
	err           error
}

// sequenceBrowser returns its queued results in order, one per List call,
// repeating the last one for any call beyond the queue's length --
// deterministic, non-blocking control over a sequence of successful and
// failed enumerations (see e.g. TestFailedRefreshPreservesCacheAndWarning).
type sequenceBrowser struct {
	mu      sync.Mutex
	results []browserResult
	idx     int
}

func (s *sequenceBrowser) List(context.Context, string) ([]conversation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.results[s.idx]
	if s.idx < len(s.results)-1 {
		s.idx++
	}
	return r.conversations, r.err
}
func (s *sequenceBrowser) Open(context.Context, string, *os.File, io.Writer) error { return nil }
func (s *sequenceBrowser) Close() error                                            { return nil }

// gatedCall is one gatedBrowser.List invocation, held open until the test
// closes release.
type gatedCall struct {
	ctx     context.Context
	release chan struct{}
}

// gatedBrowser gives a test deterministic, one-call-at-a-time control over
// exactly when a background enumeration completes -- the chatgpt-package
// counterpart to agentview's own gatedProvider, needed to exercise
// Provider's single-flight refresh state machine and the "Open never waits
// on an in-flight refresh" guarantee without any time-based guessing.
type gatedBrowser struct {
	calls chan *gatedCall
}

func newGatedBrowser() *gatedBrowser { return &gatedBrowser{calls: make(chan *gatedCall, 8)} }

func (g *gatedBrowser) List(ctx context.Context, _ string) ([]conversation, error) {
	call := &gatedCall{ctx: ctx, release: make(chan struct{})}
	g.calls <- call
	select {
	case <-call.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return nil, nil
}
func (g *gatedBrowser) Open(context.Context, string, *os.File, io.Writer) error { return nil }
func (g *gatedBrowser) Close() error                                            { return nil }

func awaitGatedCall(t *testing.T, calls <-chan *gatedCall, timeout time.Duration) *gatedCall {
	t.Helper()
	select {
	case call := <-calls:
		return call
	case <-time.After(timeout):
		t.Fatal("expected browser.List call never arrived")
		return nil
	}
}

func conv(id, title string, at time.Time) conversation {
	return conversation{ID: id, Title: title, CreatedAt: at, UpdatedAt: at}
}

// TestListServesEmptyCacheWithoutTriggeringRemoteWork fixes both the
// "initial empty" contract and the List-is-a-pure-cache-read contract
// together (see Provider.List's doc comment): before any refresh has ever
// completed, List returns an empty *successful* snapshot immediately, and
// -- unlike this Provider's earlier behavior -- never itself initiates a
// background refresh as a side effect. Initial-refresh ownership belongs
// entirely to Refresh (sessionctl.Refresher), which Agent View's own
// reload cycle already calls independently of List; a List-triggered
// refresh here would only race and needlessly coalesce an extra
// enumeration immediately after that already-requested one.
func TestListServesEmptyCacheWithoutTriggeringRemoteWork(t *testing.T) {
	browser := newGatedBrowser()
	p := &Provider{config: Config{ProjectID: "g-p-x", Root: "/w"}, browser: browser}

	for i := 0; i < 3; i++ {
		rows, err := p.List(context.Background(), false)
		if err != nil || rows == nil || len(rows) != 0 {
			t.Fatalf("List #%d: rows=%+v err=%v, want empty successful snapshot", i, rows, err)
		}
	}
	select {
	case call := <-browser.calls:
		t.Fatalf("List must never call browser.List on its own, but got a call (ctx=%v)", call.ctx)
	case <-time.After(150 * time.Millisecond):
	}

	// Only an explicit Refresh performs the enumeration.
	p.Refresh(context.Background())
	call := awaitGatedCall(t, browser.calls, time.Second)
	close(call.release)

	select {
	case <-browser.calls:
		t.Fatal("expected exactly one browser.List call from the single explicit Refresh")
	case <-time.After(150 * time.Millisecond):
	}
}

// TestSuccessfulRefreshReplacesCacheAndPublishes fixes cache replacement
// and Observer publication together: a COMPLETE enumeration atomically
// replaces the cache, and List/Observe both reflect it afterward.
func TestSuccessfulRefreshReplacesCacheAndPublishes(t *testing.T) {
	at := time.Now()
	browser := &sequenceBrowser{results: []browserResult{{conversations: []conversation{conv(conversationA, "A", at), conv(conversationB, "B", at)}}}}
	p := &Provider{config: Config{Root: "/w"}, browser: browser}

	updates := p.Observe(context.Background())
	p.Refresh(context.Background())
	upd := waitForUpdate(t, updates)
	if upd.Err != nil || len(upd.Sessions) != 2 {
		t.Fatalf("update=%+v", upd)
	}

	rows, err := p.List(context.Background(), false)
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
}

// TestFailedRefreshPreservesCacheAndWarning fixes the last-known-good
// guarantee: a refresh failure after a prior successful cache leaves the
// cache exactly as it was and publishes a failure (never an empty
// replacement) instead.
func TestFailedRefreshPreservesCacheAndWarning(t *testing.T) {
	at := time.Now()
	browser := &sequenceBrowser{results: []browserResult{
		{conversations: []conversation{conv(conversationA, "A", at)}},
		{err: errors.New("timeout")},
	}}
	p := &Provider{config: Config{Root: "/w"}, browser: browser}
	updates := p.Observe(context.Background())

	p.Refresh(context.Background())
	first := waitForUpdate(t, updates)
	if first.Err != nil || len(first.Sessions) != 1 {
		t.Fatalf("first update=%+v", first)
	}

	p.Refresh(context.Background())
	second := waitForUpdate(t, updates)
	if second.Err == nil {
		t.Fatal("expected the second refresh's failure to be published")
	}
	if second.Sessions != nil {
		t.Fatalf("a failure update must never carry Sessions (would look like an empty-replacement): %+v", second)
	}

	rows, err := p.List(context.Background(), false)
	if err != nil || len(rows) != 1 || rows[0].Key.ID != conversationA {
		t.Fatalf("cache must survive a failed refresh: rows=%+v err=%v", rows, err)
	}
}

// TestPartialPaginationFailurePreservesCache fixes the same guarantee for
// the specific failure mode the DesignDoc calls out: a pagination walk
// that never observed a terminal cursor (modeled here as browser.List
// itself returning that error, exactly what runtime.List/enumerate
// already does for an incomplete cursor chain -- see enumerate_test.go)
// must not overwrite an existing cache with the partial results.
func TestPartialPaginationFailurePreservesCache(t *testing.T) {
	at := time.Now()
	browser := &sequenceBrowser{results: []browserResult{
		{conversations: []conversation{conv(conversationA, "A", at), conv(conversationB, "B", at)}},
		{err: errors.New("Project cursor enumeration did not reach an explicit terminal page within defensive bounds")},
	}}
	p := &Provider{config: Config{Root: "/w"}, browser: browser}
	updates := p.Observe(context.Background())

	p.Refresh(context.Background())
	waitForUpdate(t, updates)
	p.Refresh(context.Background())
	second := waitForUpdate(t, updates)
	if second.Err == nil {
		t.Fatal("expected the incomplete pagination to be published as a failure")
	}

	rows, err := p.List(context.Background(), false)
	if err != nil || len(rows) != 2 {
		t.Fatalf("cache must survive incomplete pagination: rows=%+v err=%v", rows, err)
	}
}

// TestCompleteEmptyProjectReplacesCacheWithEmptySlice fixes the "COMPLETE
// enumeration with zero conversations" case: it IS a valid cache
// replacement (to []), distinct from a failure that must preserve the
// previous cache.
func TestCompleteEmptyProjectReplacesCacheWithEmptySlice(t *testing.T) {
	at := time.Now()
	browser := &sequenceBrowser{results: []browserResult{
		{conversations: []conversation{conv(conversationA, "A", at)}},
		{conversations: []conversation{}},
	}}
	p := &Provider{config: Config{Root: "/w"}, browser: browser}
	updates := p.Observe(context.Background())

	p.Refresh(context.Background())
	waitForUpdate(t, updates)
	p.Refresh(context.Background())
	second := waitForUpdate(t, updates)
	if second.Err != nil || second.Sessions == nil || len(second.Sessions) != 0 {
		t.Fatalf("a COMPLETE empty-Project enumeration must replace the cache with []: %+v", second)
	}

	rows, err := p.List(context.Background(), false)
	if err != nil || rows == nil || len(rows) != 0 {
		t.Fatalf("rows=%+v err=%v, want empty (not nil) cache", rows, err)
	}
}

// TestCacheSnapshotIsIndependentOfCallerMutation fixes catalogCache's
// copy-isolation guarantee in both directions: mutating a returned
// snapshot must never affect the cache, and mutating replace's input
// after the call must never affect the cache either.
func TestCacheSnapshotIsIndependentOfCallerMutation(t *testing.T) {
	var c catalogCache
	input := []session.Session{{Key: session.Key{Provider: session.ProviderChatGPT, ID: conversationA}, Name: "orig"}}
	c.replace(input)
	input[0].Name = "mutated-after-replace"

	out, ok := c.snapshot()
	if !ok || len(out) != 1 || out[0].Name != "orig" {
		t.Fatalf("replace must copy its input: out=%+v", out)
	}
	out[0].Name = "mutated-after-snapshot"
	out2, _ := c.snapshot()
	if out2[0].Name != "orig" {
		t.Fatalf("snapshot must return an independent copy: out2=%+v", out2)
	}
}

// TestRefreshSingleFlightsAndCoalescesConcurrentRequests fixes the
// single-flight/coalescing state machine: repeated Refresh calls while one
// enumeration is already running must never start a second concurrent
// enumeration, and must start at most one more immediately after the
// first completes.
func TestRefreshSingleFlightsAndCoalescesConcurrentRequests(t *testing.T) {
	browser := newGatedBrowser()
	p := &Provider{config: Config{Root: "/w"}, browser: browser}

	p.Refresh(context.Background())
	first := awaitGatedCall(t, browser.calls, time.Second)

	p.Refresh(context.Background())
	p.Refresh(context.Background())
	p.Refresh(context.Background())

	select {
	case <-browser.calls:
		t.Fatal("a second enumeration must not start while the first is still running")
	case <-time.After(150 * time.Millisecond):
	}

	close(first.release)
	second := awaitGatedCall(t, browser.calls, time.Second)
	close(second.release)

	select {
	case <-browser.calls:
		t.Fatal("more than one coalesced follow-up refresh started")
	case <-time.After(150 * time.Millisecond):
	}
}

// TestCloseCancelsActiveRefreshStopsFollowUpAndClosesObservers fixes
// Close's full shutdown contract: it cancels the in-flight refresh, drops
// any pending coalesced follow-up, and closes every Observer subscription
// -- with no goroutine left running past Close's return.
func TestCloseCancelsActiveRefreshStopsFollowUpAndClosesObservers(t *testing.T) {
	browser := newGatedBrowser()
	p := &Provider{config: Config{Root: "/w"}, browser: browser}
	updates := p.Observe(context.Background())

	p.Refresh(context.Background())
	call := awaitGatedCall(t, browser.calls, time.Second)
	p.Refresh(context.Background()) // queues a pending follow-up

	closeDone := make(chan struct{})
	go func() { _ = p.Close(); close(closeDone) }()

	select {
	case <-call.ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("Close must cancel the in-flight refresh's own context")
	}
	close(call.release)

	select {
	case <-closeDone:
	case <-time.After(time.Second):
		t.Fatal("Close did not return")
	}

	select {
	case <-browser.calls:
		t.Fatal("a pending follow-up refresh must not start after Close")
	case <-time.After(150 * time.Millisecond):
	}

	// The cancelled-but-racing refresh above may still have published one
	// real update before Close's watcher goroutine removed/closed the
	// channel (select between call.release and ctx.Done() is
	// unordered) -- drain past any such buffered update to the actual
	// close signal, rather than asserting on whichever arrives first.
	deadline := time.After(time.Second)
	for {
		select {
		case _, ok := <-updates:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("Observe channel never closed after Close")
		}
	}
}

// TestObserveClosesWhenContextCancelled fixes an individual subscription's
// own lifecycle: it ends when the ctx it was given ends, independent of
// the Provider itself.
func TestObserveClosesWhenContextCancelled(t *testing.T) {
	p := &Provider{config: Config{Root: "/w"}, browser: &fakeBrowser{}}
	ctx, cancel := context.WithCancel(context.Background())
	updates := p.Observe(ctx)
	cancel()
	select {
	case _, ok := <-updates:
		if ok {
			t.Fatal("expected the channel closed after ctx cancellation")
		}
	case <-time.After(time.Second):
		t.Fatal("Observe channel never closed after ctx cancellation")
	}
}

// sessionWithID is a minimal session.Session distinguishable only by key
// ID, enough for the latest-wins tests below to tell which publish a
// received ProviderUpdate came from.
func sessionWithID(id string) session.Session {
	return session.Session{Key: session.Key{Provider: session.ProviderChatGPT, ID: id}}
}

// TestObserverPublishIsLatestWinsUnderBackpressure fixes the Observer
// contract directly: publish is a full-replacement/latest-provider-state
// channel, not an event log (see sessionctl.Observer's doc comment), so a
// subscriber that hasn't drained in between publishes must converge on
// the newest one -- never an older success, and never a failure that a
// later success has already superseded.
func TestObserverPublishIsLatestWinsUnderBackpressure(t *testing.T) {
	p := &Provider{}
	updates := p.Observe(context.Background())

	p.publish(sessionctl.ProviderUpdate{Sessions: []session.Session{sessionWithID(conversationA)}})
	p.publish(sessionctl.ProviderUpdate{Err: errors.New("refresh failed")})
	p.publish(sessionctl.ProviderUpdate{Sessions: []session.Session{sessionWithID(conversationB)}})

	got := waitForUpdate(t, updates)
	if got.Err != nil || len(got.Sessions) != 1 || got.Sessions[0].Key.ID != conversationB {
		t.Fatalf("expected the latest state (B) to survive backpressure, got %+v", got)
	}
	select {
	case extra, ok := <-updates:
		if ok {
			t.Fatalf("expected no queued backlog behind the latest value, got %+v", extra)
		}
	case <-time.After(150 * time.Millisecond):
	}
}

// TestObserverPublishFailureThenSuccessConvergesToSuccess is the same
// guarantee for the specific ordering the DesignDoc calls out: a warning
// queued behind a not-yet-delivered success must never win once the
// subscriber catches up.
func TestObserverPublishFailureThenSuccessConvergesToSuccess(t *testing.T) {
	p := &Provider{}
	updates := p.Observe(context.Background())

	p.publish(sessionctl.ProviderUpdate{Err: errors.New("timeout")})
	p.publish(sessionctl.ProviderUpdate{Sessions: []session.Session{sessionWithID(conversationA)}})

	got := waitForUpdate(t, updates)
	if got.Err != nil || len(got.Sessions) != 1 || got.Sessions[0].Key.ID != conversationA {
		t.Fatalf("expected the success to win over the queued-but-stale failure, got %+v", got)
	}
}

// TestObserverPublishThreeSuccessesConvergesToLast covers the plain
// success/success/success case under the same backpressure.
func TestObserverPublishThreeSuccessesConvergesToLast(t *testing.T) {
	p := &Provider{}
	updates := p.Observe(context.Background())

	p.publish(sessionctl.ProviderUpdate{Sessions: []session.Session{sessionWithID("c1")}})
	p.publish(sessionctl.ProviderUpdate{Sessions: []session.Session{sessionWithID("c2")}})
	p.publish(sessionctl.ProviderUpdate{Sessions: []session.Session{sessionWithID("c3")}})

	got := waitForUpdate(t, updates)
	if got.Err != nil || len(got.Sessions) != 1 || got.Sessions[0].Key.ID != "c3" {
		t.Fatalf("expected convergence to the last of three successes, got %+v", got)
	}
}

// TestObserverPublishConvergesForConcurrentSlowReader exercises
// sendLatest's actual concurrency contract (rather than only the
// happens-before-drain cases above) under -race: many publishes racing a
// reader that only starts consuming partway through must still converge
// on the very last value published, with no panic (e.g. a send racing a
// concurrent close) and no deadlock.
func TestObserverPublishConvergesForConcurrentSlowReader(t *testing.T) {
	p := &Provider{}
	updates := p.Observe(context.Background())

	result := make(chan sessionctl.ProviderUpdate, 1)
	go func() {
		time.Sleep(50 * time.Millisecond)
		result <- <-updates
	}()

	const n = 20
	for i := 0; i < n; i++ {
		p.publish(sessionctl.ProviderUpdate{Sessions: []session.Session{sessionWithID(fmt.Sprintf("c%d", i))}})
	}

	select {
	case got := <-result:
		if len(got.Sessions) != 1 || got.Sessions[0].Key.ID != fmt.Sprintf("c%d", n-1) {
			t.Fatalf("expected convergence to the very latest publish, got %+v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("slow reader never received an update")
	}
}

// blockingDiscoveryBridge blocks its first Captures call until release is
// closed (or ctx ends) -- enough to hold runtime.List/enumerate mid-flight
// deterministically, without any real browser or time-based guessing.
type blockingDiscoveryBridge struct {
	entered     chan struct{}
	enteredOnce sync.Once
	release     chan struct{}
}

func (b *blockingDiscoveryBridge) BeginList(context.Context, string) error { return nil }
func (b *blockingDiscoveryBridge) Captures(ctx context.Context, _ string) ([]capture, error) {
	b.enteredOnce.Do(func() { close(b.entered) })
	select {
	case <-b.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return nil, nil
}
func (b *blockingDiscoveryBridge) ScrollRegion(context.Context, string) (scrollRegion, error) {
	return scrollRegion{}, nil
}
func (b *blockingDiscoveryBridge) Wheel(context.Context, string, int) (wheelResult, error) {
	return wheelResult{}, nil
}
func (b *blockingDiscoveryBridge) Close() error { return nil }

// TestOpenDoesNotWaitOnInFlightRefresh is the regression test the
// DesignDoc's Phase 7 explicitly calls for: a cached conversation must
// remain immediately openable while a background refresh is still
// enumerating, exercising the real runtime's locking (browser.go), not
// just Provider's own cache/refresh bookkeeping.
func TestOpenDoesNotWaitOnInFlightRefresh(t *testing.T) {
	bridge := &blockingDiscoveryBridge{entered: make(chan struct{}), release: make(chan struct{})}
	executor := &fakeBrowserExecutor{}
	helper := &fakeProcessHandle{done: make(chan struct{})}
	rt := &runtime{helper: helper, bridge: bridge, executor: executor, preloadPath: "/owned/preload.js", partition: "agentsctl-chatgpt"}

	listDone := make(chan error, 1)
	go func() {
		_, err := rt.List(context.Background(), "g-p-test")
		listDone <- err
	}()
	select {
	case <-bridge.entered:
	case <-time.After(time.Second):
		t.Fatal("List never reached the (blocked) enumeration step")
	}

	openDone := make(chan error, 1)
	go func() {
		openDone <- rt.Open(context.Background(), conversationA, nil, io.Discard)
	}()
	select {
	case err := <-openDone:
		if err != nil {
			t.Fatalf("Open failed: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Open blocked behind the in-flight background List/refresh")
	}

	close(bridge.release)
	<-listDone
}
