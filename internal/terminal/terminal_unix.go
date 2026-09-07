//go:build darwin || linux

// Package terminal holds provider-agnostic raw-terminal mechanics shared by
// every provider's Open transport: raw mode, resize-signal watching, and
// detach-key (Ctrl+]) decoding from the outer terminal's byte stream.
// Provider-specific attach semantics -- Claude's `claude attach` subprocess
// conventions, Codex's supervisor PTY protocol -- do not live here; see
// provider/claude and internal/supervisor, which both depend on this
// package rather than duplicating raw terminal handling.
package terminal

import (
	"errors"
	"os"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// Raw switches f into raw terminal mode if it is a terminal, returning a
// restore func that must be called to put it back. If f is not a terminal,
// restore is a harmless no-op.
func Raw(f *os.File) (restore func(), err error) {
	if !term.IsTerminal(int(f.Fd())) {
		return func() {}, nil
	}
	old, err := term.MakeRaw(int(f.Fd()))
	if err != nil {
		return nil, err
	}
	return func() { _ = term.Restore(int(f.Fd()), old) }, nil
}

// PollInput reports whether f has input ready to read within timeout.
func PollInput(f *os.File, timeout time.Duration) (bool, error) {
	fds := []unix.PollFd{{Fd: int32(f.Fd()), Events: unix.POLLIN}}
	n, err := unix.Poll(fds, int(timeout.Milliseconds()))
	if err != nil && errors.Is(err, syscall.EINTR) {
		return false, nil
	}
	return n > 0, err
}

// WatchResize invokes onResize once immediately and again on every
// SIGWINCH delivered while f is a terminal, until the returned stop func is
// called. If f is not a terminal, it is a no-op returning a no-op stop.
// The two provider Open transports use this identically but do different
// things in onResize: Claude inherits its child PTY's size directly from
// f; Codex sends a resize frame over the supervisor socket instead (see
// the DesignDoc's PTY attach and redraw section for why the frame path
// additionally needs a same-size reattach bounce, which onResize is
// responsible for, not this function).
func WatchResize(f *os.File, onResize func()) (stop func()) {
	if !term.IsTerminal(int(f.Fd())) {
		return func() {}
	}
	onResize()
	ch := make(chan os.Signal, 1)
	done := make(chan struct{})
	signal.Notify(ch, syscall.SIGWINCH)
	go func() {
		defer signal.Stop(ch)
		for {
			select {
			case <-ch:
				onResize()
			case <-done:
				return
			}
		}
	}()
	return func() { close(done) }
}
