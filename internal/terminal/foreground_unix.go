//go:build darwin || linux

package terminal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	creackpty "github.com/creack/pty"
)

// defaultForegroundStopTimeout bounds each step of ForegroundPTY's
// termination ladder. It is short because the user is waiting on it.
const defaultForegroundStopTimeout = time.Second

// foregroundDrainTimeout bounds how long output the child wrote just before
// exiting on its own is still forwarded.
const foregroundDrainTimeout = 250 * time.Millisecond

// ForegroundPTY runs one interactive client process on an ephemeral PTY
// bridged to the caller's terminal, for as long as a single Open lasts. It
// is a transport only: it owns no session identity and keeps nothing after
// Run returns, and the next Open starts a fresh process on a fresh PTY.
//
// Run relays the outer terminal's input to the child through a
// DetachScanner and returns when the child exits, the user presses the
// detach key, or ctx ends. It ends only the child's process group; whatever
// the child was a client of is not touched.
type ForegroundPTY struct {
	// StopTimeout bounds each step of the SIGHUP, SIGTERM, SIGKILL ladder
	// that ends the child; zero means defaultForegroundStopTimeout.
	StopTimeout time.Duration
}

// Run starts path with args in cwd on a new PTY and bridges it to in and
// out until it ends:
//
//   - the child exits on its own: its exit error, nil for status 0;
//   - the detach key: the child's process group is ended and reaped, and
//     the result is nil whatever status that leaves the child with;
//   - ctx ends: the same cleanup, and ctx's error;
//   - a relay failure: the same cleanup, and that failure.
//
// The child starts in its own session with the PTY as its controlling
// terminal, and the PTY is raw before the child starts. While the child
// runs, in is raw, out is on an alternate screen with bracketed paste
// enabled, and the PTY follows in's size; all of it is undone, in reverse
// order, only after the child is reaped and forwarding has stopped.
func (f ForegroundPTY) Run(ctx context.Context, path string, args []string, cwd string, in *os.File, out io.Writer) (retErr error) {
	cmd := exec.Command(path, args...)
	cmd.Dir = cwd
	size, err := creackpty.GetsizeFull(in)
	if err != nil {
		size = nil
	}
	master, err := StartRawPTY(cmd, size)
	if err != nil {
		return err
	}
	defer master.Close()
	child := &foregroundChild{cmd: cmd, wait: make(chan struct{}), timeout: f.stopTimeout()}
	go func() {
		child.err = cmd.Wait()
		close(child.wait)
	}()
	// Registered first, so it runs last: nothing may outlive Run even when
	// the terminal could not be taken over.
	defer child.stop()

	restore, err := Raw(in)
	if err != nil {
		return err
	}
	defer restore()
	// LIFO: stop the child and forwarding (below), flush the filter,
	// disable paste, leave the screen, then restore the mode.
	defer func() { retErr = errors.Join(retErr, WriteFull(out, []byte(AlternateScreenDisable))) }()
	if err := WriteFull(out, []byte(AlternateScreenEnable)); err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, WriteFull(out, []byte(BracketedPasteDisable))) }()
	if err := WriteFull(out, []byte(BracketedPasteEnable)); err != nil {
		return err
	}
	filtered := NewAlternateScreenLeaveFilter(out)
	defer func() { retErr = errors.Join(retErr, filtered.Flush()) }()

	// WatchResize's stop does not wait for a resize already under way, so
	// resizing guards in and master against one landing after Run returns.
	var resizeMu sync.Mutex
	resizing := true
	stopResize := WatchResize(in, func() {
		resizeMu.Lock()
		defer resizeMu.Unlock()
		if resizing {
			_ = creackpty.InheritSize(in, master)
		}
	})
	defer func() {
		stopResize()
		resizeMu.Lock()
		resizing = false
		resizeMu.Unlock()
	}()

	relayDone := make(chan struct{})
	defer close(relayDone)
	output := readChunks(master, relayDone)
	inputCtx, cancelInput := context.WithCancel(ctx)
	inputDone := make(chan struct{})
	var input inputResult
	go func() {
		defer close(inputDone)
		input = relayInput(inputCtx, in, master)
	}()
	// Runs first: end the child so a relay blocked on the PTY returns, then
	// join the input relay.
	defer func() {
		cancelInput()
		child.stop()
		<-inputDone
	}()

	for {
		select {
		case <-child.wait:
			child.stop() // sweep what the child left in its group
			return errors.Join(child.err, forwardUntil(output, filtered, time.After(foregroundDrainTimeout)))
		case <-ctx.Done():
			return ctx.Err()
		case <-inputDone:
			if input.detached {
				return nil
			}
			return input.err
		case chunk, ok := <-output:
			if !ok {
				// The PTY closed before the exit was observed; the exit
				// status is still the result.
				<-child.wait
				child.stop()
				return child.err
			}
			if err := filtered.Write(chunk); err != nil {
				return err
			}
		}
	}
}

func (f ForegroundPTY) stopTimeout() time.Duration {
	if f.StopTimeout > 0 {
		return f.StopTimeout
	}
	return defaultForegroundStopTimeout
}

// foregroundChild is a started child and the one goroutine reaping it:
// wait closes once cmd.Wait has returned, with err its result.
type foregroundChild struct {
	cmd     *exec.Cmd
	wait    chan struct{}
	err     error
	timeout time.Duration
	stopped bool
}

// stop ends the child's process group and returns once the child is
// reaped: SIGHUP, then SIGTERM, then SIGKILL, each after the previous one
// went unanswered for timeout. Once the child is reaped, what is left of
// its process group is killed. A group that is already gone (ESRCH) is
// not an error. Calling stop again is a no-op.
func (c *foregroundChild) stop() {
	if c.stopped {
		return
	}
	c.stopped = true
	pgid := c.cmd.Process.Pid
	for _, sig := range []syscall.Signal{syscall.SIGHUP, syscall.SIGTERM} {
		if c.exited() {
			break
		}
		_ = signalGroup(pgid, sig)
		select {
		case <-c.wait:
		case <-time.After(c.timeout):
		}
	}
	if !c.exited() {
		if err := signalGroup(pgid, syscall.SIGKILL); err != nil {
			_ = c.cmd.Process.Kill()
		}
		<-c.wait
	}
	// The group ID stays reserved while any member is alive, so this
	// reaches only the child's leftovers.
	_ = signalGroup(pgid, syscall.SIGKILL)
}

func (c *foregroundChild) exited() bool {
	select {
	case <-c.wait:
		return true
	default:
		return false
	}
}

func signalGroup(pgid int, sig syscall.Signal) error {
	if err := syscall.Kill(-pgid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("signal process group %d: %w", pgid, err)
	}
	return nil
}

// readChunks reads r until it fails and delivers each chunk on the returned
// channel, which is closed when reading ends. It gives up delivering once
// done is closed. Only the caller writes the chunks anywhere, so forwarding
// stops as soon as the caller stops receiving.
func readChunks(r io.Reader, done <-chan struct{}) <-chan []byte {
	chunks := make(chan []byte)
	go func() {
		defer close(chunks)
		for {
			buf := make([]byte, 32*1024)
			n, err := r.Read(buf)
			if n > 0 {
				select {
				case chunks <- buf[:n]:
				case <-done:
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	return chunks
}

// forwardUntil forwards output until it closes or deadline fires.
func forwardUntil(output <-chan []byte, filtered *AlternateScreenLeaveFilter, deadline <-chan time.Time) error {
	for {
		select {
		case chunk, ok := <-output:
			if !ok {
				return nil
			}
			if err := filtered.Write(chunk); err != nil {
				return err
			}
		case <-deadline:
			return nil
		}
	}
}

// inputResult is how relayInput ended. detached is set only when the
// detach key was seen, so it alone tells a local detach apart from an
// input failure or cancellation.
type inputResult struct {
	detached bool
	err      error
}

// relayInput copies in to child through one DetachScanner until the detach
// key, an error, or ctx ending. The detach key and everything after it in
// the same read are not delivered.
func relayInput(ctx context.Context, in *os.File, child io.Writer) inputResult {
	var scanner DetachScanner
	buf := make([]byte, 4096)
	for {
		if err := ctx.Err(); err != nil {
			return inputResult{err: err}
		}
		ready, err := PollInput(in, 50*time.Millisecond)
		if err != nil {
			return inputResult{err: err}
		}
		if !ready {
			continue
		}
		n, err := in.Read(buf)
		if n > 0 {
			deliver, detach := scanner.Feed(buf[:n])
			if werr := WriteFull(child, deliver); werr != nil {
				return inputResult{err: werr}
			}
			if detach {
				return inputResult{detached: true}
			}
		}
		if err != nil {
			return inputResult{err: err}
		}
	}
}
