//go:build darwin || linux

// Package terminal holds provider-agnostic raw-terminal mechanics shared by
// every provider's Open transport: raw mode, resize-signal watching,
// detach-key (Ctrl+]) decoding from the outer terminal's byte stream, raw
// PTY startup, outer-screen ownership, and ForegroundPTY, the Open-scoped
// PTY bridge Codex Open runs its remote TUI client through.
// Provider-specific attach semantics -- Claude's `claude attach` subprocess
// conventions, Codex's remote resume invocation -- do not live here; see
// provider/claude and provider/codex, which depend on this package rather
// than duplicating raw terminal handling.
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
// Claude's attach client and ForegroundPTY (Codex Open) both own their
// child PTY in this process and inherit its size directly from f in
// onResize. stop does not wait for an onResize already running.
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
