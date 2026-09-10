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
	"io"
	"net"
	"os"
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
// TestReattachSucceedsAfterStalledSubscriberIsDropped for that end-to-end
// case), but that race is irrelevant to what this test checks.
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

// TestReattachSucceedsAfterStalledSubscriberIsDropped drives a subscriber
// whose consumer never reads a single byte through the real end-to-end
// srv.attach path (a net.Pipe has no OS-level socket buffer, so this hits
// the write-stall path on the very first flush attempt, well before the
// byte cap could matter -- a small broadcast volume is enough). It covers
// three acceptance criteria together: the stalled attach itself does not
// hang forever and does not leave the first client's Read blocked
// forever either; the managed process's tracked lifetime (p.done) is
// untouched; and a fresh attach can replace the dropped subscriber and
// keep receiving live output -- exactly the reattach path a user needs
// after their terminal stops responding.
//
// It deliberately does not assert the first client saw a Failure frame:
// a consumer that never performs a single Read cannot receive one by
// construction (nothing can be delivered to a receiver that never
// reads), which is exactly why TestStalledOutputStillDeliversExplicitFailureReason
// tests that guarantee at the subscriber level instead, with a fake
// connection that can distinguish "stalled but the wire is still
// healthy" from "never reads at all".
func TestReattachSucceedsAfterStalledSubscriberIsDropped(t *testing.T) {
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

	// The first client never reads again from here on. This alone stalls
	// the writer's very first flush attempt (net.Pipe has no buffering),
	// well under subscriberMaxBufferedBytes, so this is a pure stall, not
	// a byte-cap overflow.
	p.broadcast([]byte("hello"))

	// A stalled flush attempts a Failure frame on its own (also stalled,
	// since this client never reads that either) before giving up, so
	// firstDone can only fire after both subscriberWriteTimeout and
	// subscriberFailureFlushTimeout have elapsed.
	giveUpBudget := subscriberWriteTimeout + subscriberFailureFlushTimeout + 3*time.Second
	select {
	case <-firstDone:
	case <-time.After(giveUpBudget):
		t.Fatal("first attach did not end after its subscriber stalled")
	}

	select {
	case <-p.done:
		t.Fatal("managed process was torn down by a stalled-subscriber disconnect")
	default:
	}
	waitForSubscriberCount(t, p, 0)

	// The first client's own blocked Read must also come back (as some
	// transport-level error, not a hang) once the server gives up and
	// closes -- a stalled subscriber must not leave the client dangling
	// forever either.
	firstReadDone := make(chan struct{})
	go func() {
		defer close(firstReadDone)
		_, _, _ = protocol.Read(firstClient)
	}()
	select {
	case <-firstReadDone:
	case <-time.After(2 * time.Second):
		t.Fatal("first client's Read never returned after the server gave up")
	}

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

// scriptedConn is a minimal net.Conn whose Write follows a fixed script
// of (n, err) results, one per call, falling back to a full, error-free
// write once the script is exhausted. It lets subscriber-level
// stall/torn-write tests be deterministic and independent of real socket
// buffer sizes and timing -- exactly the partial-write and write-failure
// scenarios a real transport can only be coaxed into producing by luck.
// Every other net.Conn method is a harmless no-op or reports closed: these
// tests only ever exercise subscriber.writeTo, which never reads and
// tolerates deadlines being no-ops since scriptedWrite's results are
// unconditional, not actually time-based.
type scriptedConn struct {
	mu     sync.Mutex
	script []scriptedWrite
	writes [][]byte
	closed bool
}

type scriptedWrite struct {
	n   int
	err error
}

func (c *scriptedConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writes = append(c.writes, append([]byte(nil), p...))
	if len(c.script) == 0 {
		return len(p), nil
	}
	next := c.script[0]
	c.script = c.script[1:]
	n := min(next.n, len(p))
	return n, next.err
}
func (c *scriptedConn) Read([]byte) (int, error) { return 0, io.EOF }
func (c *scriptedConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return nil
}
func (c *scriptedConn) LocalAddr() net.Addr              { return nil }
func (c *scriptedConn) RemoteAddr() net.Addr             { return nil }
func (c *scriptedConn) SetDeadline(time.Time) error      { return nil }
func (c *scriptedConn) SetReadDeadline(time.Time) error  { return nil }
func (c *scriptedConn) SetWriteDeadline(time.Time) error { return nil }
func (c *scriptedConn) recordedWrites() [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([][]byte(nil), c.writes...)
}
func (c *scriptedConn) wasClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// TestStalledOutputStillDeliversExplicitFailureReason is the deterministic
// counterpart to TestReattachSucceedsAfterStalledSubscriberIsDropped's
// real-transport version: it proves that when a stall leaves the
// connection at a clean frame boundary (protocol.WriteFrame reports
// torn=false -- the realistic case for a consumer that is merely slow or
// momentarily stopped reading, as opposed to gone for good), writeTo
// still delivers an explicit protocol.Failure frame rather than only
// closing the connection.
func TestStalledOutputStillDeliversExplicitFailureReason(t *testing.T) {
	sub := newSubscriber()
	conn := &scriptedConn{script: []scriptedWrite{
		{n: 0, err: os.ErrDeadlineExceeded}, // the bulk Output flush stalls cleanly
		// no further scripted entries: the Failure frame attempt succeeds
	}}
	sub.buf = []byte("some PTY output the consumer never read")

	sub.writeTo(conn)

	if !conn.wasClosed() {
		t.Fatal("connection was never closed")
	}
	writes := conn.recordedWrites()
	if len(writes) != 2 {
		t.Fatalf("write attempts=%d, want 2 (stalled Output, then Failure)", len(writes))
	}
	kind, payload, err := protocol.Read(bytes.NewReader(writes[1]))
	if err != nil || kind != protocol.Failure {
		t.Fatalf("second write kind=%q err=%v, want a decodable Failure frame", kind, err)
	}
	if len(payload) == 0 {
		t.Fatal("Failure frame carried no reason")
	}
}

// TestTornOutputWriteNeverAttemptsAnotherFrame is
// TestStalledOutputStillDeliversExplicitFailureReason's counterpart for
// the unsafe case: once a write tears (protocol.WriteFrame reports
// torn=true), the connection's framing is desynchronized, and writeTo
// must never attempt to write anything else to it -- only close.
func TestTornOutputWriteNeverAttemptsAnotherFrame(t *testing.T) {
	sub := newSubscriber()
	conn := &scriptedConn{script: []scriptedWrite{
		{n: 3, err: os.ErrDeadlineExceeded}, // torn: some but not all of the frame reached the peer
	}}
	sub.buf = []byte("some PTY output torn mid-write")

	sub.writeTo(conn)

	if !conn.wasClosed() {
		t.Fatal("connection was never closed")
	}
	writes := conn.recordedWrites()
	if len(writes) != 1 {
		t.Fatalf("write attempts=%d, want exactly 1 (the torn Output write, and nothing after it)", len(writes))
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
