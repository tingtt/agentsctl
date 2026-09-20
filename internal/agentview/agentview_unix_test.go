//go:build darwin || linux

package agentview

import (
	"bufio"
	"bytes"
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

// blockingUsageProvider augments fakeProvider with a channel-gated Usage
// call: entered closes the instant Usage is called, then the call blocks
// until release is closed (or ctx is cancelled) -- deterministic (no
// time.Sleep) control over exactly when a "slow" provider's usage becomes
// available, mirroring sessionctl's own blockingUsageSource test double.
type blockingUsageProvider struct {
	*fakeProvider
	entered chan struct{}
	release <-chan struct{}
	usage   session.Usage
}

func (f *blockingUsageProvider) Usage(ctx context.Context) (session.Usage, error) {
	close(f.entered)
	select {
	case <-f.release:
	case <-ctx.Done():
		return session.Usage{}, ctx.Err()
	}
	return f.usage, nil
}

// fastUsageProvider returns its usage immediately -- the "already
// finished" counterpart to blockingUsageProvider for provider-independence
// tests.
type fastUsageProvider struct {
	*fakeProvider
	usage session.Usage
}

func (f *fastUsageProvider) Usage(context.Context) (session.Usage, error) { return f.usage, nil }

// TestReloadReturnsWithoutWaitingForBlockedUsageProvider fixes the core
// responsiveness guarantee: a reload cycle's catalog Snapshot must apply
// even while a provider's Usage call is still blocked -- usage is never on
// the catalog's critical path (see Runtime.requestReload's doc comment).
func TestReloadReturnsWithoutWaitingForBlockedUsageProvider(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // unblocks the provider's Usage call at the end of the test, rather than leaking it
	entered := make(chan struct{})
	release := make(chan struct{}) // deliberately never closed -- cancel() ends the block instead
	p := &blockingUsageProvider{
		fakeProvider: &fakeProvider{id: session.ProviderClaude, rows: []session.Session{{Key: session.Key{Provider: session.ProviderClaude, ID: "a"}, CWD: "/work"}}},
		entered:      entered,
		release:      release,
	}
	rt := &Runtime{Controller: sessionctl.Controller{Providers: []sessionctl.Source{p}, Pins: &fakePins{}}, State: NewState(), CWD: "/work"}

	done := make(chan struct{})
	go func() {
		rt.syncReload(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("reload did not apply its Snapshot while the usage provider was blocked -- usage must never gate catalog loading")
	}
	if len(rt.State.Rows) != 1 {
		t.Fatalf("catalog not loaded by the time reload returned: %+v", rt.State.Rows)
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the background usage fetch never started at all")
	}
}

// syncBuffer is a concurrency-safe io.Writer/bytes accessor: Runtime.Output
// is written from the eventLoop goroutine while a test observes it from
// its own goroutine, so a plain bytes.Buffer would race under -race.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}
func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// fakeReadKey returns an eventLoop-compatible readInputFn driven entirely by
// ch: each call blocks until the test sends one keyResult, giving the test
// deterministic, one-key-at-a-time control over what Run's one-shot
// key-read goroutine (see startKeyRead) observes -- never a real terminal
// or a time-based guess.
func fakeReadKey(ch <-chan keyResult) func(*bufio.Reader) (InputEvent, error) {
	return func(*bufio.Reader) (InputEvent, error) {
		r := <-ch
		return r.ev, r.err
	}
}

// drainCatalog blocks for Runtime.catalogCh's current-generation events
// and applies each to State exactly as eventLoop's own catalogCh case
// would, discarding any stale (gen-mismatched) event still sitting in the
// channel from an earlier reload this test never drained (e.g. one
// act() triggered internally via a Result.Reload before the test called
// this), until the current generation's done event arrives -- test-only
// convenience for integration tests that call act()/requestReload
// directly, with no real event loop draining catalogCh, and need the
// latest reload's fully-merged Snapshot applied before asserting on it
// (a generation with multiple providers can send more than one
// current-generation event -- see catalogEvent's doc comment -- so this
// does not return on the first one). Production code never blocks like
// this; see Runtime.requestReload's own doc comment for why.
func (r *Runtime) drainCatalog(ctx context.Context) {
	deadline := time.After(2 * time.Second)
	for {
		select {
		case upd := <-r.catalogCh:
			if upd.gen != r.catalogGen {
				continue
			}
			r.currentScope = upd.scope
			if upd.ps.Provider != "" {
				r.applyLoadSnapshot(upd.ps.Provider, upd.ps.Sessions, upd.ps.Err, upd.ps.ListOwnsStatus)
			}
			r.recomputeRows()
			if upd.done {
				r.State.CatalogLoading = false
				r.refreshUsageAsync(ctx)
				return
			}
		case <-deadline:
			panic("drainCatalog: no current-generation done event arrived within 2s")
		}
	}
}

// syncReload is requestReload followed by drainCatalog -- the test-only,
// blocking-until-applied counterpart to the old synchronous reload,
// exactly matching what it used to do in one call: request a catalog
// load, wait for its Snapshot, apply it to State, and kick off usage
// refresh.
func (r *Runtime) syncReload(ctx context.Context) {
	r.requestReload(ctx)
	r.drainCatalog(ctx)
}

// waitFor polls cond (which must be safe to call concurrently with
// whatever goroutine it observes -- e.g. reading only atomics or a
// mutex-guarded value) until it reports true or timeout elapses.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("condition never became true within the timeout")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// latestFrame returns the most recently rendered frame from a syncBuffer's
// accumulated output. Runtime.render always prefixes a frame with a
// clear-screen escape (see terminalFrame), and syncBuffer's Write (unlike
// a real terminal) never discards earlier frames -- so asserting that some
// content is ABSENT from the current render (e.g. a stale row that should
// have been replaced) must only look at the frame after the last
// clear-screen marker, not the whole accumulated history where that
// content legitimately appeared in an earlier frame.
func latestFrame(output string) string {
	if i := strings.LastIndex(output, "\x1b[2J\x1b[H"); i >= 0 {
		return output[i:]
	}
	return output
}

// gatedCall is one gatedProvider.List invocation, held open until the test
// closes release (letting it return the provider's current rows) -- ctx is
// exposed so a test can additionally observe whether/when this specific
// reload cycle's own context got cancelled (see requestReload's
// catalogCancel).
type gatedCall struct {
	ctx     context.Context
	release chan struct{}
}

// gatedProvider's List signals entry (a *gatedCall sent on calls) and then
// blocks until the test closes that call's release, giving deterministic,
// one-call-at-a-time control over exactly when a "slow" provider's catalog
// fetch completes -- the Controller.Load-level counterpart to
// blockingUsageProvider, needed now that Runtime.requestReload runs Load
// in the background (see agentview_unix.go). It then returns a fresh copy
// of whatever fakeProvider.rows holds at that moment, so a test can assign
// different content per call before releasing it (see e.g.
// TestCtrlLRefreshKeepsOldRowsVisibleAndInputResponsive).
//
// Unless ignoreCancel is set, List also races ctx.Done() the way a real,
// context-aware provider does (see e.g. the ChatGPT bridge's own ctx
// handling) -- ignoreCancel exists only for tests that need a superseded
// cycle's List call to complete deterministically on its own schedule
// regardless of how fast real cancellation would otherwise resolve it, to
// test eventLoop's own generation check as its own, independent safety
// net (see catalogGen's doc comment: "cancellation -> save work,
// generation -> correctness").
type gatedProvider struct {
	*fakeProvider
	calls        chan *gatedCall
	ignoreCancel bool
}

func newGatedProvider(fp *fakeProvider) *gatedProvider {
	return &gatedProvider{fakeProvider: fp, calls: make(chan *gatedCall, 8)}
}

func (f *gatedProvider) List(ctx context.Context, archived bool) ([]session.Session, error) {
	call := &gatedCall{ctx: ctx, release: make(chan struct{})}
	f.calls <- call
	if f.ignoreCancel {
		<-call.release
	} else {
		select {
		case <-call.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return f.fakeProvider.List(ctx, archived)
}

// awaitCall blocks until gatedProvider's next List call has signaled entry
// (sent its *gatedCall on calls), returning it so the test can inspect its
// ctx or close its release -- or fails the test if none arrives within
// timeout.
func awaitCall(t *testing.T, calls <-chan *gatedCall, timeout time.Duration) *gatedCall {
	t.Helper()
	select {
	case call := <-calls:
		return call
	case <-time.After(timeout):
		t.Fatal("expected provider List call never arrived")
		return nil
	}
}

// TestEventLoopAppliesUsageUpdateAndRedrawsWithoutConsumingKeyRead fixes
// that a background usage update reaches the rendered output on its own,
// without requiring (or consuming) a key press: the fake key reader here
// is fed nothing and never returns for the whole test.
func TestEventLoopAppliesUsageUpdateAndRedrawsWithoutConsumingKeyRead(t *testing.T) {
	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer inR.Close()
	defer inW.Close()
	out := &syncBuffer{}
	rt := &Runtime{State: NewState(), Input: inR, Output: out, usageCh: make(chan usageEvent, 1), usageGen: 1}

	keyIn := make(chan keyResult)
	loopDone := make(chan error, 1)
	go func() {
		loopDone <- rt.eventLoop(context.Background(), bufio.NewReader(rt.Input), fakeReadKey(keyIn))
	}()

	rt.usageCh <- usageEvent{gen: 1, provider: session.ProviderClaude, usage: session.Usage{Provider: session.ProviderClaude, FiveHour: session.UsageWindow{State: session.UsageAvailable, Percent: 42, Reset: time.Now().Add(time.Hour)}}}

	waitFor(t, 2*time.Second, func() bool { return bytes.Contains([]byte(out.String()), []byte("42%")) })

	keyIn <- keyResult{err: io.EOF}
	select {
	case <-loopDone:
	case <-time.After(2 * time.Second):
		t.Fatal("eventLoop did not exit after the fake key reader returned an error")
	}
}

// TestEventLoopIgnoresStaleGenerationUsageUpdate fixes that a usage update
// tagged with an older generation than the current reload cycle (e.g. it
// was already superseded by a second reload before it arrived) is dropped
// rather than overwriting newer state with stale data.
func TestEventLoopIgnoresStaleGenerationUsageUpdate(t *testing.T) {
	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer inR.Close()
	defer inW.Close()
	out := &syncBuffer{}
	rt := &Runtime{State: NewState(), Input: inR, Output: out, usageCh: make(chan usageEvent, 1), usageGen: 2} // current cycle is gen 2

	keyIn := make(chan keyResult)
	loopDone := make(chan error, 1)
	go func() {
		loopDone <- rt.eventLoop(context.Background(), bufio.NewReader(rt.Input), fakeReadKey(keyIn))
	}()

	rt.usageCh <- usageEvent{gen: 1, provider: session.ProviderClaude, usage: session.Usage{Provider: session.ProviderClaude, FiveHour: session.UsageWindow{State: session.UsageAvailable, Percent: 42}}} // stale gen

	// Give the stale update a bounded window to (wrongly) apply, then
	// prove a fresh gen-2 update still works -- ruling out "usageCh
	// itself is broken" as an alternative explanation for an absent 42%.
	time.Sleep(50 * time.Millisecond)
	rt.usageCh <- usageEvent{gen: 2, provider: session.ProviderCodex, usage: session.Usage{Provider: session.ProviderCodex, FiveHour: session.UsageWindow{State: session.UsageAvailable, Percent: 7}}}
	waitFor(t, 2*time.Second, func() bool { return bytes.Contains([]byte(out.String()), []byte("7%")) })

	if bytes.Contains([]byte(out.String()), []byte("42%")) {
		t.Fatal("a stale-generation usage update was applied and rendered")
	}

	keyIn <- keyResult{err: io.EOF}
	<-loopDone
}

// countingListProvider counts List calls so a test can prove exactly one
// reload happened in response to exactly one physical key.
type countingListProvider struct {
	*fakeProvider
	listCalls atomic.Int32
}

func (f *countingListProvider) List(ctx context.Context, archived bool) ([]session.Session, error) {
	f.listCalls.Add(1)
	return f.fakeProvider.List(ctx, archived)
}

// TestEventLoopHandlesExactlyOneReloadPerKey fixes the no-duplicate-reads
// guarantee: one physical key (here, Ctrl+/, a plain reload-triggering
// key) must cause exactly one additional catalog reload -- not zero (the
// key silently dropped) and not more than one (the one-shot key-read
// goroutine somehow firing twice for a single physical key).
func TestEventLoopHandlesExactlyOneReloadPerKey(t *testing.T) {
	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer inR.Close()
	defer inW.Close()
	p := &countingListProvider{fakeProvider: &fakeProvider{id: session.ProviderClaude}}
	rt := &Runtime{Controller: sessionctl.Controller{Providers: []sessionctl.Source{p}, Pins: &fakePins{}}, State: NewState(), Input: inR, Output: &syncBuffer{}, CWD: "/work"}
	rt.syncReload(context.Background()) // baseline load, like Run's own initial reload
	baseline := p.listCalls.Load()

	keyIn := make(chan keyResult)
	loopDone := make(chan error, 1)
	go func() {
		loopDone <- rt.eventLoop(context.Background(), bufio.NewReader(rt.Input), fakeReadKey(keyIn))
	}()

	keyIn <- keyResult{ev: keyInput(KeyEvent{Key: KeyCtrlSlash})}
	waitFor(t, 2*time.Second, func() bool { return p.listCalls.Load() == baseline+1 })

	// Give any errant duplicate a bounded window to show up, then confirm
	// it never does.
	time.Sleep(50 * time.Millisecond)
	if got := p.listCalls.Load(); got != baseline+1 {
		t.Fatalf("listCalls=%d, want exactly %d (baseline+1) after a single Ctrl+/", got, baseline+1)
	}

	keyIn <- keyResult{err: io.EOF}
	<-loopDone
}

// inputOwnershipProvider's Open reads exactly wantLen bytes from the real
// terminal input it's handed, recording exactly what it received --
// proving (or disproving) that no other reader raced it for those bytes.
type inputOwnershipProvider struct {
	*fakeProvider
	wantLen    int
	openCalled chan struct{}
	readDone   chan struct{}
	got        []byte
	gotErr     error
}

func (f *inputOwnershipProvider) Open(ctx context.Context, s session.Session, in *os.File, out io.Writer) error {
	close(f.openCalled)
	buf := make([]byte, f.wantLen)
	n, err := io.ReadFull(in, buf)
	f.got, f.gotErr = buf[:n], err
	close(f.readDone)
	return nil
}

// TestEventLoopHasNoOutstandingReaderWhileOpenOwnsInput is the input-
// ownership regression: while a provider's Open holds the real terminal
// input, no leftover Agent View key-read goroutine may race it for bytes.
// If one did, some of the bytes written here (simulating the user typing
// to the attached child right after attaching) would be stolen by the
// overview's own reader instead of reaching Open, and this test would
// observe a short or garbled read.
func TestEventLoopHasNoOutstandingReaderWhileOpenOwnsInput(t *testing.T) {
	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer inR.Close()
	defer inW.Close()
	target := session.Key{Provider: session.ProviderClaude, ID: "a"}
	p := &inputOwnershipProvider{
		fakeProvider: &fakeProvider{id: session.ProviderClaude, rows: []session.Session{{Key: target, CWD: "/work", Actions: session.Actions{session.ActionOpen: {Available: true}}}}},
		wantLen:      5,
		openCalled:   make(chan struct{}),
		readDone:     make(chan struct{}),
	}
	rt := &Runtime{Controller: sessionctl.Controller{Providers: []sessionctl.Source{p}, Pins: &fakePins{}}, State: NewState(), Input: inR, Output: &syncBuffer{}, CWD: "/work", terminal: &fakeOverviewLifecycle{}}
	rt.syncReload(context.Background())
	rt.State.selectIndex(0)

	keyIn := make(chan keyResult)
	loopDone := make(chan error, 1)
	go func() {
		loopDone <- rt.eventLoop(context.Background(), bufio.NewReader(rt.Input), fakeReadKey(keyIn))
	}()

	keyIn <- keyResult{ev: keyInput(KeyEvent{Key: KeyEnter})} // empty composer + selection -> IntentOpen

	select {
	case <-p.openCalled:
	case <-time.After(2 * time.Second):
		t.Fatal("provider Open was never reached")
	}

	want := []byte("HELLO")
	if _, err := inW.Write(want); err != nil {
		t.Fatal(err)
	}

	select {
	case <-p.readDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Open's read of the attached input never completed")
	}
	if p.gotErr != nil {
		t.Fatalf("Open's read errored: %v", p.gotErr)
	}
	if string(p.got) != string(want) {
		t.Fatalf("Open received %q, want %q -- a concurrent Agent View reader stole some bytes", p.got, want)
	}

	keyIn <- keyResult{err: io.EOF}
	<-loopDone
}

// TestEventLoopCodexUsageVisibleBeforeSlowClaudeCompletes is the provider-
// independence guarantee applied at the Runtime/eventLoop integration
// level (sessionctl.UsageStream's own test covers the fetch itself): a
// fast provider's usage must reach rendered output while a slower
// provider is still blocked, and releasing the slow one afterward must
// still apply on top rather than being dropped.
func TestEventLoopCodexUsageVisibleBeforeSlowClaudeCompletes(t *testing.T) {
	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer inR.Close()
	defer inW.Close()
	entered := make(chan struct{})
	release := make(chan struct{})
	claude := &blockingUsageProvider{
		fakeProvider: &fakeProvider{id: session.ProviderClaude},
		entered:      entered,
		release:      release,
		usage:        session.Usage{Provider: session.ProviderClaude, FiveHour: session.UsageWindow{State: session.UsageAvailable, Percent: 88}},
	}
	codex := &fastUsageProvider{
		fakeProvider: &fakeProvider{id: session.ProviderCodex},
		usage:        session.Usage{Provider: session.ProviderCodex, FiveHour: session.UsageWindow{State: session.UsageAvailable, Percent: 3}},
	}
	out := &syncBuffer{}
	rt := &Runtime{Controller: sessionctl.Controller{Providers: []sessionctl.Source{claude, codex}, Pins: &fakePins{}}, State: NewState(), Input: inR, Output: out, CWD: "/work"}

	// The reload must complete (including lazily initializing r.usageCh)
	// before eventLoop starts reading it -- exactly the sequencing Run()
	// itself always uses (an initial requestReload applied before the
	// event loop starts, both on Run's own single goroutine); starting
	// them concurrently here would race on r.usageCh's initialization, not
	// exercise anything Run() actually does.
	rt.syncReload(context.Background())

	keyIn := make(chan keyResult)
	loopDone := make(chan error, 1)
	go func() {
		loopDone <- rt.eventLoop(context.Background(), bufio.NewReader(rt.Input), fakeReadKey(keyIn))
	}()

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("claude's usage fetch never started")
	}
	waitFor(t, 2*time.Second, func() bool { return bytes.Contains([]byte(out.String()), []byte("3%")) })
	if bytes.Contains([]byte(out.String()), []byte("88%")) {
		t.Fatal("claude's usage rendered before its blocked call was released")
	}

	close(release)
	waitFor(t, 2*time.Second, func() bool { return bytes.Contains([]byte(out.String()), []byte("88%")) })

	keyIn <- keyResult{err: io.EOF}
	<-loopDone
}

// TestInitialCatalogLoadDoesNotBlockInput fixes the core non-blocking
// guarantee for startup: with the initial requestReload's List call still
// blocked (as ChatGPT's multi-page browser-backed discovery walk can
// legitimately be for several seconds -- see agentview_unix.go's package
// doc references), the event loop must still process a physical key
// (typing into the composer) rather than waiting for the catalog.
func TestInitialCatalogLoadDoesNotBlockInput(t *testing.T) {
	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer inR.Close()
	defer inW.Close()
	p := newGatedProvider(&fakeProvider{id: session.ProviderClaude})
	out := &syncBuffer{}
	rt := &Runtime{Controller: sessionctl.Controller{Providers: []sessionctl.Source{p}, Pins: &fakePins{}}, State: NewState(), Input: inR, Output: out, CWD: "/work"}

	// Mirrors Run()'s own sequencing: an initial requestReload, then a
	// render, both before the event loop -- see Run's doc comment.
	rt.requestReload(context.Background())
	rt.render()
	call := awaitCall(t, p.calls, 2*time.Second)

	keyIn := make(chan keyResult)
	loopDone := make(chan error, 1)
	go func() {
		loopDone <- rt.eventLoop(context.Background(), bufio.NewReader(rt.Input), fakeReadKey(keyIn))
	}()

	// Typing is purely local UI state (IntentNone) -- it must reach
	// rendered output on its own, with the catalog List still blocked.
	keyIn <- keyResult{ev: keyInput(KeyEvent{Key: KeyRune, Rune: 'H'})}
	waitFor(t, 2*time.Second, func() bool { return bytes.Contains([]byte(out.String()), []byte("H")) })

	close(call.release) // let the blocked List finish, rather than leak it
	keyIn <- keyResult{err: io.EOF}
	select {
	case <-loopDone:
	case <-time.After(2 * time.Second):
		t.Fatal("eventLoop did not exit")
	}
}

// TestCtrlLRefreshKeepsOldRowsVisibleAndInputResponsive fixes the refresh
// UX guarantee: while a Ctrl+L-triggered reload's List call is still
// blocked, the previously-loaded rows must stay rendered (never clear to
// empty) and the event loop must stay responsive to further input; once
// the reload completes, the new rows replace the old ones.
func TestCtrlLRefreshKeepsOldRowsVisibleAndInputResponsive(t *testing.T) {
	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer inR.Close()
	defer inW.Close()
	fp := &fakeProvider{id: session.ProviderClaude, rows: []session.Session{
		{Key: key("a"), Name: "RowAlpha", CWD: "/work"},
		{Key: key("b"), Name: "RowBravo", CWD: "/work"},
	}}
	p := newGatedProvider(fp)
	out := &syncBuffer{}
	rt := &Runtime{Controller: sessionctl.Controller{Providers: []sessionctl.Source{p}, Pins: &fakePins{}}, State: NewState(), Input: inR, Output: out, CWD: "/work"}

	// Load the initial catalog synchronously (test setup, not the thing
	// under test here).
	rt.requestReload(context.Background())
	initial := awaitCall(t, p.calls, 2*time.Second)
	close(initial.release)
	rt.drainCatalog(context.Background())
	if len(rt.State.Rows) != 2 {
		t.Fatalf("initial catalog not loaded: %+v", rt.State.Rows)
	}

	keyIn := make(chan keyResult)
	loopDone := make(chan error, 1)
	go func() {
		loopDone <- rt.eventLoop(context.Background(), bufio.NewReader(rt.Input), fakeReadKey(keyIn))
	}()

	keyIn <- keyResult{ev: keyInput(KeyEvent{Key: KeyCtrlL})} // bindingRefresh
	refresh := awaitCall(t, p.calls, 2*time.Second)

	waitFor(t, 2*time.Second, func() bool {
		frame := latestFrame(out.String())
		return strings.Contains(frame, "RowAlpha") &&
			strings.Contains(frame, "RowBravo") &&
			strings.Contains(frame, "loading sessions")
	})

	// Input keeps working while the refresh is still blocked.
	keyIn <- keyResult{ev: keyInput(KeyEvent{Key: KeyRune, Rune: 'Z'})}
	waitFor(t, 2*time.Second, func() bool { return strings.Contains(latestFrame(out.String()), "Z") })

	// Release the refresh with different content -- the old rows must be
	// replaced, not merged or left stale.
	fp.rows = []session.Session{{Key: key("c"), Name: "RowCharlie", CWD: "/work"}}
	close(refresh.release)
	waitFor(t, 2*time.Second, func() bool {
		frame := latestFrame(out.String())
		return strings.Contains(frame, "RowCharlie") && !strings.Contains(frame, "loading sessions")
	})
	if strings.Contains(latestFrame(out.String()), "RowAlpha") {
		t.Fatal("stale row RowAlpha still rendered after the refresh completed")
	}

	keyIn <- keyResult{err: io.EOF}
	<-loopDone
}

// TestStaleReloadGenerationIsIgnored fixes the correctness half of
// requestReload's guarantee (the other half being context cancellation,
// covered by TestSupersededReloadCancelsPreviousContext): an older reload
// cycle's Snapshot must never overwrite a newer cycle's already-applied
// result, even if it (misbehaving, or simply slow) completes after the
// newer one -- see eventLoop's catalogCh case and catalogGen's doc
// comment. The stale cycle's provider call here deliberately ignores its
// own ctx cancellation (gatedProvider's ignoreCancel) so this test proves
// the generation check itself, independent of how fast cancellation would
// otherwise have stopped it.
func TestStaleReloadGenerationIsIgnored(t *testing.T) {
	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer inR.Close()
	defer inW.Close()
	fp := &fakeProvider{id: session.ProviderClaude}
	p := &gatedProvider{fakeProvider: fp, calls: make(chan *gatedCall, 8), ignoreCancel: true}
	out := &syncBuffer{}
	rt := &Runtime{Controller: sessionctl.Controller{Providers: []sessionctl.Source{p}, Pins: &fakePins{}}, State: NewState(), Input: inR, Output: out, CWD: "/work"}

	rt.requestReload(context.Background()) // gen 1
	gen1 := awaitCall(t, p.calls, 2*time.Second)
	rt.requestReload(context.Background()) // gen 2, supersedes gen 1
	gen2 := awaitCall(t, p.calls, 2*time.Second)

	keyIn := make(chan keyResult)
	loopDone := make(chan error, 1)
	go func() {
		loopDone <- rt.eventLoop(context.Background(), bufio.NewReader(rt.Input), fakeReadKey(keyIn))
	}()

	fp.rows = []session.Session{{Key: key("g2"), Name: "GenTwoRow", CWD: "/work"}}
	close(gen2.release)
	waitFor(t, 2*time.Second, func() bool { return bytes.Contains([]byte(out.String()), []byte("GenTwoRow")) })

	// Now let the stale gen-1 call finally complete, with different
	// content, and give it a bounded window to (wrongly) apply.
	fp.rows = []session.Session{{Key: key("g1"), Name: "StaleGenOneRow", CWD: "/work"}}
	close(gen1.release)
	time.Sleep(100 * time.Millisecond)
	if bytes.Contains([]byte(out.String()), []byte("StaleGenOneRow")) {
		t.Fatal("a stale-generation reload overwrote the current generation's already-applied rows")
	}
	if !bytes.Contains([]byte(out.String()), []byte("GenTwoRow")) {
		t.Fatal("the current generation's rows are no longer rendered")
	}

	keyIn <- keyResult{err: io.EOF}
	<-loopDone
}

// TestScopeChangeDuringReloadIgnoresOlderScopeResult is
// TestStaleReloadGenerationIsIgnored applied to the scope-change scenario
// the DesignDoc calls out explicitly: switching directory scope while a
// previous scope's Load is still in flight must never let that older
// scope's result overwrite the newer scope's already-applied one.
func TestScopeChangeDuringReloadIgnoresOlderScopeResult(t *testing.T) {
	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer inR.Close()
	defer inW.Close()
	fp := &fakeProvider{id: session.ProviderClaude}
	p := &gatedProvider{fakeProvider: fp, calls: make(chan *gatedCall, 8), ignoreCancel: true}
	out := &syncBuffer{}
	rt := &Runtime{Controller: sessionctl.Controller{Providers: []sessionctl.Source{p}, Pins: &fakePins{}}, State: NewState(), Input: inR, Output: out, CWD: "/work"}

	rt.State.Scope = session.ScopeSame
	rt.requestReload(context.Background())
	sameScope := awaitCall(t, p.calls, 2*time.Second)

	rt.State.Scope = session.ScopeDescendants
	rt.requestReload(context.Background())
	descendantsScope := awaitCall(t, p.calls, 2*time.Second)

	keyIn := make(chan keyResult)
	loopDone := make(chan error, 1)
	go func() {
		loopDone <- rt.eventLoop(context.Background(), bufio.NewReader(rt.Input), fakeReadKey(keyIn))
	}()

	fp.rows = []session.Session{{Key: key("d"), Name: "DescendantsRow", CWD: "/work"}}
	close(descendantsScope.release)
	waitFor(t, 2*time.Second, func() bool { return bytes.Contains([]byte(out.String()), []byte("DescendantsRow")) })

	fp.rows = []session.Session{{Key: key("s"), Name: "StaleSameScopeRow", CWD: "/work"}}
	close(sameScope.release)
	time.Sleep(100 * time.Millisecond)
	if bytes.Contains([]byte(out.String()), []byte("StaleSameScopeRow")) {
		t.Fatal("the previous scope's result overwrote the newer scope's already-applied result")
	}

	keyIn <- keyResult{err: io.EOF}
	<-loopDone
}

// TestSupersededReloadCancelsPreviousContext fixes the save-work half of
// requestReload's guarantee (see catalogGen's doc comment): starting a new
// reload cycle must cancel the previous cycle's own child context, so a
// superseded ChatGPT List/enumeration stops doing work for a Snapshot that
// will be discarded, rather than running to completion regardless.
func TestSupersededReloadCancelsPreviousContext(t *testing.T) {
	p := newGatedProvider(&fakeProvider{id: session.ProviderClaude})
	rt := &Runtime{Controller: sessionctl.Controller{Providers: []sessionctl.Source{p}, Pins: &fakePins{}}, State: NewState(), CWD: "/work"}

	rt.requestReload(context.Background())
	first := awaitCall(t, p.calls, 2*time.Second)

	rt.requestReload(context.Background()) // supersedes -- must cancel first's ctx

	select {
	case <-first.ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("superseding a reload did not cancel the previous cycle's context")
	}
	if !errors.Is(first.ctx.Err(), context.Canceled) {
		t.Fatalf("first.ctx.Err()=%v, want context.Canceled", first.ctx.Err())
	}

	second := awaitCall(t, p.calls, 2*time.Second)
	close(second.release) // avoid leaking the second cycle's goroutine
}

// TestCatalogCancelStopsInFlightReloadOnQuit fixes the no-leak guarantee
// Run's own defer relies on (see agentview_unix.go's Run): cancelling the
// current reload cycle's recorded catalogCancel -- exactly what that defer
// calls on every return path, including IntentQuit -- must reach and
// cancel an in-flight List call's ctx, tearing the background fetch down
// with the overview instead of leaving it running to completion for a
// Snapshot nothing will ever apply.
func TestCatalogCancelStopsInFlightReloadOnQuit(t *testing.T) {
	p := newGatedProvider(&fakeProvider{id: session.ProviderClaude})
	rt := &Runtime{Controller: sessionctl.Controller{Providers: []sessionctl.Source{p}, Pins: &fakePins{}}, State: NewState(), CWD: "/work"}

	rt.requestReload(context.Background())
	call := awaitCall(t, p.calls, 2*time.Second)

	if rt.catalogCancel == nil {
		t.Fatal("requestReload did not record a cancel func")
	}
	rt.catalogCancel() // what Run's deferred cleanup calls on the way out

	select {
	case <-call.ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("cancelling catalogCancel (as Run's quit-path defer does) did not cancel the in-flight List call")
	}
}

// TestFastProviderRowsAppearBeforeSlowProviderCompletes fixes the
// per-provider incremental catalog guarantee: a fast provider's rows
// (e.g. Claude, Codex) must render -- and stay selectable/actionable --
// before a slower provider (e.g. ChatGPT's multi-page browser-backed
// discovery walk) finishes its own List call, rather than waiting behind
// it for one atomic batch (see catalogEvent's doc comment and
// sessionctl.Controller.LoadStream).
func TestFastProviderRowsAppearBeforeSlowProviderCompletes(t *testing.T) {
	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer inR.Close()
	defer inW.Close()
	fast := &fakeProvider{id: session.ProviderClaude, rows: []session.Session{
		{Key: session.Key{Provider: session.ProviderClaude, ID: "a"}, Name: "FastRow", CWD: "/work"},
	}}
	slowFP := &fakeProvider{id: session.ProviderChatGPT}
	slow := newGatedProvider(slowFP)
	out := &syncBuffer{}
	rt := &Runtime{Controller: sessionctl.Controller{Providers: []sessionctl.Source{fast, slow}, Pins: &fakePins{}}, State: NewState(), Input: inR, Output: out, CWD: "/work"}

	// Mirrors Run()'s own sequencing (see Run's doc comment).
	rt.requestReload(context.Background())
	rt.render()
	call := awaitCall(t, slow.calls, 2*time.Second)

	keyIn := make(chan keyResult)
	loopDone := make(chan error, 1)
	go func() {
		loopDone <- rt.eventLoop(context.Background(), bufio.NewReader(rt.Input), fakeReadKey(keyIn))
	}()

	// The fast provider's row must render, with the loading indicator
	// still up for the still-blocked slow provider.
	waitFor(t, 2*time.Second, func() bool {
		frame := latestFrame(out.String())
		return strings.Contains(frame, "FastRow") && strings.Contains(frame, "loading sessions")
	})

	// It must also stay actionable: typing into the composer works while
	// ChatGPT's List is still blocked.
	keyIn <- keyResult{ev: keyInput(KeyEvent{Key: KeyRune, Rune: 'Q'})}
	waitFor(t, 2*time.Second, func() bool { return strings.Contains(latestFrame(out.String()), "Q") })

	slowFP.rows = []session.Session{{Key: session.Key{Provider: session.ProviderChatGPT, ID: "c"}, Name: "SlowRow", CWD: "/work"}}
	close(call.release)
	waitFor(t, 2*time.Second, func() bool {
		frame := latestFrame(out.String())
		return strings.Contains(frame, "FastRow") && strings.Contains(frame, "SlowRow") && !strings.Contains(frame, "loading sessions")
	})

	keyIn <- keyResult{err: io.EOF}
	<-loopDone
}
