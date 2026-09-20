//go:build darwin || linux

// These tests pin the attach input guarantee: Input frames form one
// ordered PTY byte stream, where neither read boundaries nor frame
// boundaries carry meaning, and each Input payload is written completely
// (or the attach fails) before the next frame is processed. This is what
// keeps a long bracketed paste (issue #29) one paste from terminal to
// Codex PTY.
package supervisor

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	creackpty "github.com/creack/pty"
	"github.com/tingtt/agentsctl/internal/localstate"
	"github.com/tingtt/agentsctl/internal/supervisor/protocol"
	"golang.org/x/term"
)

// recordingWriter records what reaches it. maxWrites, when non-empty,
// makes each Write accept only that many bytes (cycling through the
// list) and report no error -- the short write an io.Writer is allowed to
// make less often than it is written for.
type recordingWriter struct {
	mu        sync.Mutex
	got       bytes.Buffer
	maxWrites []int
	calls     int
	failAt    int // 1-based call number that fails with err after accepting failN bytes; 0 disables
	failN     int
	err       error
}

func (w *recordingWriter) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls++
	if w.failAt != 0 && w.calls == w.failAt {
		n := min(w.failN, len(b))
		w.got.Write(b[:n])
		return n, w.err
	}
	n := len(b)
	if len(w.maxWrites) > 0 {
		n = min(n, w.maxWrites[(w.calls-1)%len(w.maxWrites)])
	}
	w.got.Write(b[:n])
	return n, nil
}

func (w *recordingWriter) bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]byte(nil), w.got.Bytes()...)
}

// pasteStream builds a bracketed paste of roughly size bytes that mixes
// multiline text with CR/LF, UTF-8 Japanese, a long single line, and
// bytes that look like agentsctl's own detach key in every encoding.
func pasteStream(size int) []byte {
	const unit = "日本語のプロンプト行\r\nsecond line\nthird\rfourth " +
		"\x1d \x1b[27;5;93~ \x1b[93;5u " + "long-single-line-segment-0123456789 "
	var b strings.Builder
	b.WriteString("\x1b[200~")
	for b.Len() < size {
		b.WriteString(unit)
	}
	b.WriteString("\x1b[201~")
	return []byte(b.String())
}

func TestWriteInputRetriesShortWritesUntilPayloadIsComplete(t *testing.T) {
	payload := pasteStream(20 << 10)
	w := &recordingWriter{maxWrites: []int{17, 3, 4096, 1, 511}}
	if err := writeInput(w, payload); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(w.bytes(), payload) {
		t.Fatalf("recorded %d bytes, want the %d-byte payload exactly once and in order", len(w.bytes()), len(payload))
	}
	if w.calls < 2 {
		t.Fatalf("calls=%d: the writer never made a short write", w.calls)
	}
}

func TestWriteInputFailsWithoutDroppingSilentlyOrRetryingPastAnError(t *testing.T) {
	wantErr := errors.New("pty gone")
	payload := bytes.Repeat([]byte("x"), 100)
	w := &recordingWriter{failAt: 2, failN: 5, err: wantErr, maxWrites: []int{10}}
	if err := writeInput(w, payload); !errors.Is(err, wantErr) {
		t.Fatalf("err=%v, want %v", err, wantErr)
	}
	if w.calls != 2 || w.got.Len() != 15 {
		t.Fatalf("calls=%d recorded=%d, want writing to stop at the failing call (2 calls, 15 bytes)", w.calls, w.got.Len())
	}
}

func TestWriteInputTreatsZeroProgressAsFailureNotAnInfiniteLoop(t *testing.T) {
	done := make(chan error, 1)
	go func() { done <- writeInput(&recordingWriter{maxWrites: []int{4, 0}}, []byte("0123456789")) }()
	select {
	case err := <-done:
		if !errors.Is(err, io.ErrShortWrite) {
			t.Fatalf("err=%v, want io.ErrShortWrite", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("writeInput looped on a writer that makes no progress")
	}
}

// attachedServer starts Server.attach over a pipe for a process whose PTY
// input is w, returning the client end after the attach handshake.
func attachedServer(t *testing.T, w io.Writer) (client net.Conn, p *process, attachDone <-chan struct{}) {
	t.Helper()
	p = &process{run: localstate.Run{ID: "r"}, input: w, subscribers: map[*subscriber]struct{}{}, done: make(chan struct{})}
	srv := &Server{runs: map[string]*process{"r": p}}
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close() })
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.attach(server, "r")
	}()
	if kind, _, err := protocol.Read(client); err != nil || kind != protocol.Response {
		t.Fatalf("attach response kind=%q err=%v", kind, err)
	}
	return client, p, done
}

// TestAttachDeliversInputFramesAsOneByteStreamWhereverTheyAreCut cuts one
// bracketed paste into Input frames at every interesting boundary --
// inside both markers, at several sizes, one byte at a time -- and
// requires the PTY side to see exactly the original bytes, through a
// writer that also makes short writes.
func TestAttachDeliversInputFramesAsOneByteStreamWhereverTheyAreCut(t *testing.T) {
	stream := pasteStream(9 << 10)
	endMarker := len(stream) - len("\x1b[201~")
	cuts := map[string][]int{
		"one frame":                {},
		"inside begin marker":      {1, 2, 3, 4, 5, 6},
		"inside end marker":        {endMarker + 1, endMarker + 2, endMarker + 3, endMarker + 4, endMarker + 5},
		"both markers and payload": {3, 4096, 4099, 8192, endMarker + 2},
	}
	for name, at := range cuts {
		t.Run(name, func(t *testing.T) {
			w := &recordingWriter{maxWrites: []int{17, 3, 700}}
			client, _, attachDone := attachedServer(t, w)
			prev := 0
			for _, cut := range append(at, len(stream)) {
				if err := protocol.Write(client, protocol.Input, stream[prev:cut]); err != nil {
					t.Fatal(err)
				}
				prev = cut
			}
			// Detach is processed after every earlier frame, so once
			// attach returns, all input has been written.
			if err := protocol.Write(client, protocol.Detach, nil); err != nil {
				t.Fatal(err)
			}
			select {
			case <-attachDone:
			case <-time.After(5 * time.Second):
				t.Fatal("attach did not return after detach")
			}
			if !bytes.Equal(w.bytes(), stream) {
				t.Fatalf("PTY received %d bytes, want the %d-byte stream unchanged", len(w.bytes()), len(stream))
			}
		})
	}
}

// TestAttachInputWriteFailureEndsAttachWithFailureAndKeepsProcess pins the
// error contract: a PTY input write that cannot complete ends the attach
// with an explicit protocol.Failure -- never a silent drop that keeps
// forwarding later input onto a corrupted stream -- and does not touch
// the managed process, whose lifetime is independent of attach.
func TestAttachInputWriteFailureEndsAttachWithFailureAndKeepsProcess(t *testing.T) {
	w := &recordingWriter{failAt: 1, failN: 3, err: errors.New("input/output error")}
	client, p, attachDone := attachedServer(t, w)
	if err := protocol.Write(client, protocol.Input, []byte("0123456789")); err != nil {
		t.Fatal(err)
	}
	kind, data, err := protocol.Read(client)
	if err != nil || kind != protocol.Failure || !strings.Contains(string(data), "input/output error") {
		t.Fatalf("kind=%q data=%q err=%v, want a Failure frame naming the write error", kind, data, err)
	}
	select {
	case <-attachDone:
	case <-time.After(3 * time.Second):
		t.Fatal("attach did not end after an input write failure")
	}
	select {
	case <-p.done:
		t.Fatal("an attach input failure must not stop the managed process")
	default:
	}
	if got := w.bytes(); string(got) != "012" {
		t.Fatalf("PTY received %q: nothing after the failed write may be forwarded", got)
	}
}

// serveAttach listens on a fresh Unix socket and hands its one accepted
// connection, after the attach Request, to srv.attach for runID.
func serveAttach(t *testing.T, srv *Server, runID string) (socket string) {
	t.Helper()
	// Short path: see fakeSupervisorSocket.
	dir, err := os.MkdirTemp("/tmp", "supervisor-sock-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	ln, err := net.Listen("unix", dir+"/s.sock")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		if kind, _, err := protocol.Read(conn); err != nil || kind != protocol.Request {
			_ = conn.Close()
			return
		}
		srv.attach(conn, runID)
	}()
	return dir + "/s.sock"
}

// TestClientAttachKeepsLongPasteIntactThroughDetachScanning drives the real
// client path -- terminal bytes, 4 KiB reads, DetachScanner, Input frames,
// Server.attach, PTY writer -- with a paste whose payload contains detach
// look-alikes. Read and frame boundaries fall wherever the OS puts them;
// the PTY must see the original bytes regardless, and a real detach after
// the paste must still work.
func TestClientAttachKeepsLongPasteIntactThroughDetachScanning(t *testing.T) {
	master, slave, err := creackpty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	defer slave.Close()
	// Match what Client.attach sets, so the writes below cannot race it.
	if _, err := term.MakeRaw(int(slave.Fd())); err != nil {
		t.Fatal(err)
	}

	w := &recordingWriter{maxWrites: []int{17, 3, 4096}}
	p := &process{run: localstate.Run{ID: "r"}, input: w, subscribers: map[*subscriber]struct{}{}, done: make(chan struct{})}
	srv := &Server{runs: map[string]*process{"r": p}}

	sock := serveAttach(t, srv, "r")

	done := make(chan error, 1)
	go func() { done <- (Client{Socket: sock}).Attach(context.Background(), "r", slave, io.Discard) }()

	paste := pasteStream(20 << 10)
	// Written from a goroutine: if Attach ends early, master.Write would
	// otherwise block this test instead of letting it report the failure.
	go func() {
		for rest := paste; len(rest) > 0; {
			n := min(len(rest), 1531) // deliberately not a divisor of the 4096-byte read size
			if _, err := master.Write(rest[:n]); err != nil {
				return
			}
			rest = rest[n:]
		}
		_, _ = master.Write([]byte("\x1d")) // detach, after the paste
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Attach err=%v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Attach did not return after the detach that follows the paste")
	}
	if !bytes.Equal(w.bytes(), paste) {
		t.Fatalf("PTY received %d bytes, want the %d-byte paste unchanged", len(w.bytes()), len(paste))
	}
}
