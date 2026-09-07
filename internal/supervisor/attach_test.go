//go:build darwin || linux

package supervisor

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	creackpty "github.com/creack/pty"
	"github.com/tingtt/agentsctl/internal/supervisor/protocol"
	"github.com/tingtt/agentsctl/internal/terminal"
)

// fakeSupervisorSocket starts a minimal fake supervisor: it accepts one
// connection, reads the attach Request frame, always answers with an OK
// Response, then hands the connection to handle to drive the rest of the
// exchange (e.g. sending an Exit frame to model a session ending on its
// own, or just blocking to let the caller drive an explicit detach).
func fakeSupervisorSocket(t *testing.T, handle func(conn net.Conn)) string {
	t.Helper()
	// A short-path temp dir, not t.TempDir() (which nests under a long
	// per-test-name directory): unix socket paths are limited to ~104
	// bytes (sockaddr_un) on macOS, and a path built from the full test
	// name here routinely exceeds that.
	dir, err := os.MkdirTemp("/tmp", "supervisor-sock-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "supervisor.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		kind, _, err := protocol.Read(conn)
		if err != nil || kind != protocol.Request {
			return
		}
		res, _ := json.Marshal(Response{OK: true})
		if protocol.Write(conn, protocol.Response, res) != nil {
			return
		}
		handle(conn)
	}()
	return sock
}

// TestClientAttachExplicitDetachReturnsCleanly models the DetachKey
// (Ctrl+]) path end to end at the Client.Attach level: local input carries
// the detach byte, Attach must intercept it (never forward it as session
// input), send protocol.Detach, and return with no error.
func TestClientAttachExplicitDetachReturnsCleanly(t *testing.T) {
	detachSeen := make(chan struct{}, 1)
	sock := fakeSupervisorSocket(t, func(conn net.Conn) {
		for {
			kind, _, err := protocol.Read(conn)
			if err != nil {
				return
			}
			if kind == protocol.Detach {
				detachSeen <- struct{}{}
				return
			}
		}
	})
	master, slave, err := creackpty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	defer slave.Close()

	client := Client{Socket: sock}
	done := make(chan error, 1)
	go func() { done <- client.Attach(context.Background(), "run1", slave, io.Discard) }()
	if _, err := master.Write([]byte{terminal.DetachKey}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-detachSeen:
	case <-time.After(3 * time.Second):
		t.Fatal("server never observed a protocol.Detach frame")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Attach err=%v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Attach did not return after sending Detach")
	}
}

// TestClientAttachNaturalExitReturnsCleanly models the session ending on
// its own (the remote Codex run exits, sending protocol.Exit) with no
// local Ctrl+] ever pressed -- this must also return with no error.
func TestClientAttachNaturalExitReturnsCleanly(t *testing.T) {
	sock := fakeSupervisorSocket(t, func(conn net.Conn) {
		_ = protocol.Write(conn, protocol.Exit, nil)
	})
	master, slave, err := creackpty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	defer slave.Close()

	client := Client{Socket: sock}
	if err := client.Attach(context.Background(), "run1", slave, io.Discard); err != nil {
		t.Fatalf("Attach err=%v", err)
	}
}

// TestClientAttachFailureReturnsError covers an attach-time/runtime error
// (protocol.Failure).
func TestClientAttachFailureReturnsError(t *testing.T) {
	sock := fakeSupervisorSocket(t, func(conn net.Conn) {
		_ = protocol.Write(conn, protocol.Failure, []byte("boom"))
	})
	master, slave, err := creackpty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	defer slave.Close()

	client := Client{Socket: sock}
	if err := client.Attach(context.Background(), "run1", slave, io.Discard); err == nil {
		t.Fatal("expected an error from a protocol.Failure frame")
	}
}

// TestClientAttachSendsInitialSizeAsARedrawResizeFrame drives Client.Attach
// end to end against a fake supervisor and asserts the very first frame it
// sends is a Resize frame with Redraw set (matching the real terminal's
// current size) -- the signal syncPTYSize (server side, see
// supervisor_unix.go) uses to force a same-size reattach repaint (see the
// DesignDoc's PTY attach and redraw section).
func TestClientAttachSendsInitialSizeAsARedrawResizeFrame(t *testing.T) {
	master, slave, err := creackpty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	defer slave.Close()
	if err := creackpty.Setsize(slave, &creackpty.Winsize{Rows: 24, Cols: 80}); err != nil {
		t.Fatal(err)
	}
	firstFrame := make(chan struct {
		kind byte
		data []byte
	}, 1)
	sock := fakeSupervisorSocket(t, func(conn net.Conn) {
		kind, data, err := protocol.Read(conn)
		if err != nil {
			return
		}
		firstFrame <- struct {
			kind byte
			data []byte
		}{kind, data}
		<-time.After(200 * time.Millisecond) // hold the connection open long enough to observe the frame
	})
	client := Client{Socket: sock}
	done := make(chan error, 1)
	go func() { done <- client.Attach(context.Background(), "run1", slave, io.Discard) }()
	select {
	case frame := <-firstFrame:
		var size protocol.TerminalSize
		if err := json.Unmarshal(frame.data, &size); err != nil {
			t.Fatal(err)
		}
		if frame.kind != protocol.Resize || !size.Redraw || size.Rows != 24 || size.Cols != 80 {
			t.Fatalf("kind=%q size=%+v", frame.kind, size)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Attach never sent an initial Resize frame")
	}
	// Wait for Attach to return (the fake server closes the connection
	// once its handler above returns) before the deferred master/slave
	// Close calls run, so Attach's own in-flight PollInput/Read never
	// races those closes.
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Attach did not return after the fake server closed the connection")
	}
}
