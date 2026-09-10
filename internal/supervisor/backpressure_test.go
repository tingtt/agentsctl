//go:build darwin || linux

// This file fixes issue #31: an attach whose consumer fell behind used to
// be judged by how many discrete PTY read() chunks were outstanding, not
// by how many bytes were outstanding. A burst of many small chunks (a
// Codex redraw made of many short escape sequences is the case the issue
// reports) could exhaust that chunk-counted queue while the underlying
// byte volume was still trivial, silently dropping the subscriber and
// closing its connection with no explanation -- the client saw nothing
// but a bare EOF, and the managed Codex process (whose lifetime is
// intentionally independent of any attach connection) kept running and
// holding its thread-store writer lock with no way for the user to know
// why the screen went blank.
package supervisor

import (
	"bytes"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/tingtt/agentsctl/internal/localstate"
	"github.com/tingtt/agentsctl/internal/supervisor/protocol"
)

// TestBroadcastToleratesBurstOfManyTinyChunksUnderTheByteCap directly
// models the issue's reported trigger: far more PTY read() chunks than
// the old 64-chunk queue could ever hold, but few enough total bytes that
// a byte-bounded buffer must never disconnect the subscriber over it.
func TestBroadcastToleratesBurstOfManyTinyChunksUnderTheByteCap(t *testing.T) {
	sub := newSubscriber()
	p := &process{subscribers: map[*subscriber]struct{}{sub: {}}, done: make(chan struct{})}

	const chunks = 100_000 // the old chunk-counted queue held at most 64
	for i := range chunks {
		p.broadcast([]byte{byte(i)})
	}

	p.mu.Lock()
	_, attached := p.subscribers[sub]
	p.mu.Unlock()
	if !attached {
		t.Fatal("subscriber was disconnected although the buffered bytes stayed far under the byte cap")
	}
	sub.mu.Lock()
	buffered, reason := len(sub.buf), sub.reason
	sub.mu.Unlock()
	if reason != subscriberOpen {
		t.Fatalf("reason=%v, want subscriberOpen", reason)
	}
	if buffered != chunks {
		t.Fatalf("buffered=%d, want all %d one-byte chunks retained", buffered, chunks)
	}
}

// TestOverflowFlushesBufferedBytesThenSendsExplicitFailureReason covers
// the deterministic byte-cap boundary end to end: once buffered output
// would exceed subscriberMaxBufferedBytes, the subscriber must be
// disconnected, but (a) every byte accepted before the boundary is still
// delivered, in order, with none lost or duplicated, and (b) the client
// sees an explicit protocol.Failure reason -- never a bare close a caller
// could mistake for io.EOF.
//
// No writer runs while the buffer is filled: append's byte accounting is
// tested here in isolation (deterministically, with no I/O and hence no
// scheduling race), so the fill loop's arithmetic lands on the boundary
// exactly. writeTo is only attached afterward, to verify it correctly
// flushes a buffer whose final content and close reason were already
// decided -- production always races the two (see
// TestReattachSucceedsAfterOverflowDisconnectsAnEarlierSubscriber for that
// end-to-end case), but that race is irrelevant to what this test checks.
func TestOverflowFlushesBufferedBytesThenSendsExplicitFailureReason(t *testing.T) {
	sub := newSubscriber()
	p := &process{subscribers: map[*subscriber]struct{}{sub: {}}, done: make(chan struct{})}

	const chunkSize = 1024
	var sent []byte
	for len(sent)+chunkSize <= subscriberMaxBufferedBytes {
		chunk := bytes.Repeat([]byte{byte(len(sent) % 251)}, chunkSize)
		p.broadcast(chunk)
		sent = append(sent, chunk...)
	}
	p.mu.Lock()
	_, stillAttached := p.subscribers[sub]
	p.mu.Unlock()
	if !stillAttached {
		t.Fatal("subscriber was disconnected before its byte cap was exceeded")
	}

	// Tips the buffer past subscriberMaxBufferedBytes; this chunk is
	// rejected (never appended), so it must never appear in what the
	// client receives.
	p.broadcast(bytes.Repeat([]byte{'!'}, chunkSize))

	p.mu.Lock()
	_, attachedAfterOverflow := p.subscribers[sub]
	p.mu.Unlock()
	if attachedAfterOverflow {
		t.Fatal("subscriber remained attached after its byte cap was exceeded")
	}

	client, server := net.Pipe()
	defer client.Close()
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		sub.writeTo(server)
	}()

	var received []byte
	sawFailure := false
	for {
		kind, data, err := protocol.Read(client)
		if err != nil {
			t.Fatalf("read before observing a Failure frame: %v", err)
		}
		if kind == protocol.Failure {
			if len(data) == 0 {
				t.Fatal("Failure frame carried no reason")
			}
			sawFailure = true
			break
		}
		if kind != protocol.Output {
			t.Fatalf("frame kind=%q, want Output or Failure", kind)
		}
		received = append(received, data...)
	}
	if !sawFailure {
		t.Fatal("connection ended without a Failure frame")
	}
	if !bytes.Equal(received, sent) {
		t.Fatalf("received %d bytes, want exactly the %d bytes accepted before overflow, unmodified and in order", len(received), len(sent))
	}
	if _, _, err := protocol.Read(client); err == nil {
		t.Fatal("connection remained open after its Failure frame")
	}
	select {
	case <-writerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("writer goroutine did not exit after sending its Failure frame")
	}
}

// TestAggregatedOutputPreservesExactByteStreamAcrossManyChunkSizes checks
// data integrity end to end through the real writeTo pump: aggregating
// many differently sized broadcast chunks into fewer, larger frames must
// never lose, duplicate, or reorder a single byte, even while the client
// is actively (if unevenly) draining concurrently with production.
func TestAggregatedOutputPreservesExactByteStreamAcrossManyChunkSizes(t *testing.T) {
	sub := newSubscriber()
	p := &process{subscribers: map[*subscriber]struct{}{sub: {}}, done: make(chan struct{})}
	client, server := net.Pipe()
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		sub.writeTo(server)
	}()

	var want []byte
	sizes := []int{1, 1, 1, 2, 3, 5, 8, 13, 1, 1, 500, 1, 4096, 1, 1, 1, 60}
	for i, size := range sizes {
		chunk := bytes.Repeat([]byte{byte('a' + i%26)}, size)
		want = append(want, chunk...)
	}

	var received []byte
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		for {
			kind, data, err := protocol.Read(client)
			if err != nil {
				return
			}
			if kind == protocol.Output {
				received = append(received, data...)
			}
		}
	}()

	for i, size := range sizes {
		chunk := bytes.Repeat([]byte{byte('a' + i%26)}, size)
		p.broadcast(chunk)
	}
	p.finishSubscribers()

	select {
	case <-writerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("writer goroutine did not exit after process exit")
	}
	select {
	case <-readDone:
	case <-time.After(2 * time.Second):
		t.Fatal("client read loop did not observe the connection close")
	}
	_ = client.Close()
	if !bytes.Equal(received, want) {
		t.Fatalf("received %d bytes, want the %d input bytes unmodified and in order", len(received), len(want))
	}
}

// TestReattachSucceedsAfterOverflowDisconnectsAnEarlierSubscriber covers
// two acceptance criteria together: an overflow disconnect must not touch
// the managed process's tracked lifetime (p.done stays open), and a fresh
// attach must be able to replace the dropped subscriber and keep
// receiving live output -- exactly the reattach path a user needs after
// a burst disconnects their terminal.
func TestReattachSucceedsAfterOverflowDisconnectsAnEarlierSubscriber(t *testing.T) {
	p := &process{run: localstate.Run{ID: "r"}, subscribers: map[*subscriber]struct{}{}, done: make(chan struct{})}
	srv := &Server{runs: map[string]*process{"r": p}}

	firstClient, firstServer := net.Pipe()
	defer firstClient.Close()
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		srv.attach(firstServer, "r")
	}()
	if kind, _, err := protocol.Read(firstClient); err != nil || kind != protocol.Response {
		t.Fatalf("first attach response kind=%q err=%v", kind, err)
	}
	waitForSubscriberCount(t, p, 1)

	// The first client never reads again from here on, so its writer
	// goroutine's very first flush blocks on the unread connection --
	// stealing whatever had accumulated by then out of the byte-counted
	// buffer before this loop can observe it. Broadcasting several times
	// subscriberMaxBufferedBytes (instead of stopping right at the
	// boundary, as the byte-accounting test above does with no writer
	// racing it) guarantees overflow regardless of exactly how much that
	// steal was.
	const chunkSize = 1024
	for total := 0; total < 3*subscriberMaxBufferedBytes; total += chunkSize {
		p.broadcast(bytes.Repeat([]byte{'x'}, chunkSize))
	}
	// firstDone can only fire once the writer's stuck write hits
	// subscriberWriteTimeout, so this must outlast that deadline.
	select {
	case <-firstDone:
	case <-time.After(subscriberWriteTimeout + 2*time.Second):
		t.Fatal("first attach did not end after its subscriber overflowed")
	}

	select {
	case <-p.done:
		t.Fatal("managed process was torn down by an overflow disconnect")
	default:
	}
	waitForSubscriberCount(t, p, 0)

	secondClient, secondServer := net.Pipe()
	defer secondClient.Close()
	secondDone := make(chan struct{})
	go func() {
		defer close(secondDone)
		srv.attach(secondServer, "r")
	}()
	if kind, _, err := protocol.Read(secondClient); err != nil || kind != protocol.Response {
		t.Fatalf("reattach response kind=%q err=%v", kind, err)
	}
	waitForSubscriberCount(t, p, 1)

	p.broadcast([]byte("hello again"))
	kind, data, err := protocol.Read(secondClient)
	if err != nil || kind != protocol.Output || string(data) != "hello again" {
		t.Fatalf("reattach output kind=%q data=%q err=%v", kind, data, err)
	}

	_ = protocol.Write(secondClient, protocol.Detach, nil)
	select {
	case <-secondDone:
	case <-time.After(3 * time.Second):
		t.Fatal("reattach did not return after detach")
	}
}

func waitForSubscriberCount(t *testing.T, p *process, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		p.mu.Lock()
		got := len(p.subscribers)
		p.mu.Unlock()
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("subscribers=%d, want %d", got, want)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestConcurrentBroadcastSubscribeUnsubscribeAndProcessExitHasNoRaceOrPanic
// exercises the concurrency surface the redesign touches -- broadcast,
// addSubscriber/removeSubscriber, and a single finishSubscribers -- all
// running against each other, so `go test -race` can catch a send on a
// closed channel, a double close, a deadlock, or a data race. Every
// goroutine runs a fixed number of iterations gated by a WaitGroup, never
// a sleep, so the test's outcome does not depend on scheduling luck.
func TestConcurrentBroadcastSubscribeUnsubscribeAndProcessExitHasNoRaceOrPanic(t *testing.T) {
	p := &process{subscribers: map[*subscriber]struct{}{}, done: make(chan struct{})}
	var wg sync.WaitGroup

	const broadcasters = 4
	const broadcastsPerWorker = 2_000
	for w := range broadcasters {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := range broadcastsPerWorker {
				p.broadcast([]byte(fmt.Sprintf("w%di%d", worker, i)))
			}
		}(w)
	}

	const attachers = 4
	const attachCyclesPerWorker = 500
	for range attachers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range attachCyclesPerWorker {
				sub := newSubscriber()
				if !p.addSubscriber(sub) {
					return // process already finished
				}
				p.removeSubscriber(sub)
			}
		}()
	}

	wg.Wait()
	p.finishSubscribers()

	select {
	case <-p.done:
	default:
		t.Fatal("finishSubscribers did not close p.done")
	}
	p.mu.Lock()
	remaining := len(p.subscribers)
	p.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("subscribers=%d, want 0 after finishSubscribers", remaining)
	}
}

// TestAttachSurvivesBurstEvenWhenDrivenThroughTheProtocolLayer is a
// higher-level companion to the buffer-level tests above: it drives the
// real Server.attach over the wire protocol with a burst of chunk counts
// the old queue could never have held, and a consumer that reads
// concurrently (mirroring a real, if unhurried, terminal client), and
// asserts attach is never dropped and every byte arrives intact.
func TestAttachSurvivesBurstEvenWhenDrivenThroughTheProtocolLayer(t *testing.T) {
	p := &process{run: localstate.Run{ID: "r"}, subscribers: map[*subscriber]struct{}{}, done: make(chan struct{})}
	srv := &Server{runs: map[string]*process{"r": p}}
	client, server := net.Pipe()
	defer client.Close()
	attachDone := make(chan struct{})
	go func() {
		defer close(attachDone)
		srv.attach(server, "r")
	}()
	if kind, _, err := protocol.Read(client); err != nil || kind != protocol.Response {
		t.Fatalf("attach response kind=%q err=%v", kind, err)
	}
	waitForSubscriberCount(t, p, 1)

	var want []byte
	const chunks = 20_000
	for i := range chunks {
		chunk := []byte{byte(i)}
		want = append(want, chunk...)
	}

	var received []byte
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		for len(received) < len(want) {
			kind, data, err := protocol.Read(client)
			if err != nil {
				return
			}
			if kind == protocol.Output {
				received = append(received, data...)
			}
		}
	}()

	for i := range chunks {
		p.broadcast([]byte{byte(i)})
	}

	select {
	case <-readDone:
	case <-time.After(5 * time.Second):
		t.Fatalf("did not receive all output before timeout; got %d/%d bytes", len(received), len(want))
	}
	if !bytes.Equal(received, want) {
		t.Fatalf("received %d bytes, want %d bytes unmodified and in order", len(received), len(want))
	}

	_ = protocol.Write(client, protocol.Detach, nil)
	select {
	case <-attachDone:
	case <-time.After(3 * time.Second):
		t.Fatal("attach did not return after detach")
	}
}
