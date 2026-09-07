//go:build darwin || linux

package supervisor

import (
	"context"
	"io"
	"net"
	"os"
	"testing"
	"time"

	"github.com/tingtt/agentsctl/internal/supervisor/protocol"
)

// TestClientAttachLocalDetachWinsConnectionCloseRace covers issue #18/PR
// #21's race: the supervisor closes the attach connection right after
// observing protocol.Detach, so the client's incoming socket reader can
// observe that as an EOF for the very same reason the input pump is
// winding down. A successful local Ctrl+] detach must always win that
// race -- Attach must return nil, never surface the EOF as an error.
//
// The input pump here stands in for the real Ctrl+]-scanning
// pumpAttachInput and is deliberately held from returning via holdReturn,
// so inputDone cannot close no matter how fast the EOF arrives -- this
// forces the attach loop to decide the outcome from the incomingFrames
// error alone, exactly the branch the race is about, instead of leaving it
// to chance which of the two ready channels select happens to pick.
// Before releasing the pump, the test waits for it to observe its own
// context (inputCtx) being canceled, which proves the attach loop has
// already committed to a return value on this path (cancelInput only runs
// once a return value is decided, whether from the fix's explicit
// mid-loop call or the trailing defer that runs on every return path) --
// so releasing the pump only afterwards can no longer influence what
// Attach ultimately returns.
func TestClientAttachLocalDetachWinsConnectionCloseRace(t *testing.T) {
	serverSawDetach := make(chan struct{})
	sock := fakeSupervisorSocket(t, func(conn net.Conn) {
		for {
			kind, _, err := protocol.Read(conn)
			if err != nil {
				return
			}
			if kind == protocol.Detach {
				close(serverSawDetach)
				return // conn.Close() runs via fakeSupervisorSocket's defer
			}
		}
	})
	in, inputWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	defer inputWriter.Close()

	readyToDetach := make(chan struct{})
	holdReturn := make(chan struct{})
	raceObserved := make(chan struct{})
	pump := func(ctx context.Context, _ *os.File, frames *lockedFrames) inputOutcome {
		<-readyToDetach
		if err := frames.write(protocol.Detach, nil); err != nil {
			return inputOutcome{err: err}
		}
		select {
		case <-ctx.Done():
			close(raceObserved)
		case <-holdReturn:
			return inputOutcome{detached: true}
		}
		<-holdReturn
		return inputOutcome{detached: true}
	}

	done := make(chan error, 1)
	go func() {
		done <- (Client{Socket: sock}).attach(context.Background(), "run1", in, io.Discard, pump)
	}()

	close(readyToDetach)
	select {
	case <-serverSawDetach:
	case <-time.After(3 * time.Second):
		t.Fatal("server never observed a protocol.Detach frame")
	}
	select {
	case <-raceObserved:
	case <-time.After(3 * time.Second):
		t.Fatal("attach loop never reacted to the connection close while the input pump had not yet reported its outcome")
	}
	close(holdReturn)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Attach err=%v, want nil (a successful local detach must win the connection-close race)", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Attach did not return after the input pump finished")
	}
}

// TestClientAttachUnexpectedConnectionCloseReturnsError is the other side
// of the same fix: without a local detach ever happening, an unexpected
// connection close/EOF must still surface as an error, not be mistaken for
// a clean detach. This guards against an over-broad fix that simply
// ignores io.EOF or turns every connection error into success.
func TestClientAttachUnexpectedConnectionCloseReturnsError(t *testing.T) {
	serverClosed := make(chan struct{})
	sock := fakeSupervisorSocket(t, func(conn net.Conn) {
		close(serverClosed)
		// Returning here closes conn via fakeSupervisorSocket's defer,
		// with no protocol.Detach ever having been sent.
	})
	in, inputWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	defer inputWriter.Close()

	pumpCanceled := make(chan struct{})
	pump := func(ctx context.Context, _ *os.File, _ *lockedFrames) inputOutcome {
		<-ctx.Done()
		close(pumpCanceled)
		return inputOutcome{err: ctx.Err()}
	}

	done := make(chan error, 1)
	go func() {
		done <- (Client{Socket: sock}).attach(context.Background(), "run1", in, io.Discard, pump)
	}()

	select {
	case <-serverClosed:
	case <-time.After(3 * time.Second):
		t.Fatal("fake supervisor never accepted the attach connection")
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Attach err=nil, want an error for an unexpected connection close with no local detach")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Attach did not return after the connection closed unexpectedly")
	}
	select {
	case <-pumpCanceled:
	case <-time.After(3 * time.Second):
		t.Fatal("input pump was not canceled once the connection closed")
	}
}
