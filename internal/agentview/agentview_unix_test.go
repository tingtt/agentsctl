//go:build darwin || linux

package agentview

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"os"
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
// responsiveness guarantee: reload's catalog load must complete and
// return even while a provider's Usage call is still blocked -- usage is
// never on the catalog's critical path (see Runtime.reload's doc
// comment).
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
		rt.reload(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("reload did not return while the usage provider was blocked -- usage must never gate catalog loading")
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

// fakeReadKey returns an eventLoop-compatible readKeyFn driven entirely by
// ch: each call blocks until the test sends one keyResult, giving the test
// deterministic, one-key-at-a-time control over what Run's one-shot
// key-read goroutine (see startKeyRead) observes -- never a real terminal
// or a time-based guess.
func fakeReadKey(ch <-chan keyResult) func(*bufio.Reader) (KeyEvent, error) {
	return func(*bufio.Reader) (KeyEvent, error) {
		r := <-ch
		return r.ev, r.err
	}
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

	rt.usageCh <- usageEvent{gen: 1, provider: session.ProviderClaude, usage: session.Usage{Provider: session.ProviderClaude, FiveHour: session.UsageWindow{State: session.UsageAvailable, Percent: 42, Reset: time.Now()}}}

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
// guarantee: one physical key (here, Ctrl+G, a plain reload-triggering
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
	rt.reload(context.Background()) // baseline load, like Run's own initial reload
	baseline := p.listCalls.Load()

	keyIn := make(chan keyResult)
	loopDone := make(chan error, 1)
	go func() {
		loopDone <- rt.eventLoop(context.Background(), bufio.NewReader(rt.Input), fakeReadKey(keyIn))
	}()

	keyIn <- keyResult{ev: KeyEvent{Key: KeyCtrlG}}
	waitFor(t, 2*time.Second, func() bool { return p.listCalls.Load() == baseline+1 })

	// Give any errant duplicate a bounded window to show up, then confirm
	// it never does.
	time.Sleep(50 * time.Millisecond)
	if got := p.listCalls.Load(); got != baseline+1 {
		t.Fatalf("listCalls=%d, want exactly %d (baseline+1) after a single Ctrl+G", got, baseline+1)
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
	rt := &Runtime{Controller: sessionctl.Controller{Providers: []sessionctl.Source{p}, Pins: &fakePins{}}, State: NewState(), Input: inR, Output: &syncBuffer{}, CWD: "/work"}
	rt.reload(context.Background())
	rt.State.selectIndex(0)

	keyIn := make(chan keyResult)
	loopDone := make(chan error, 1)
	go func() {
		loopDone <- rt.eventLoop(context.Background(), bufio.NewReader(rt.Input), fakeReadKey(keyIn))
	}()

	keyIn <- keyResult{ev: KeyEvent{Key: KeyEnter}} // empty composer + selection -> IntentOpen

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

	// reload must complete (including lazily initializing r.usageCh)
	// before eventLoop starts reading it -- exactly the sequencing Run()
	// itself always uses (reload, then the event loop, both on Run's own
	// single goroutine); starting them concurrently here would race on
	// r.usageCh's initialization, not exercise anything Run() actually does.
	rt.reload(context.Background())

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
