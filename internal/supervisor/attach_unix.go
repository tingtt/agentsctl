//go:build darwin || linux

package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"github.com/tingtt/agentsctl/internal/supervisor/protocol"
	"github.com/tingtt/agentsctl/internal/terminal"
	"golang.org/x/term"
)

type lockedFrames struct {
	mu sync.Mutex
	w  io.Writer
}

func (f *lockedFrames) write(kind byte, b []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return protocol.Write(f.w, kind, b)
}

// inputOutcome is how the attach input pump ended. detached is the only
// signal the attach loop trusts to tell a locally requested Ctrl+] detach
// (after which the supervisor closing the attach connection is expected,
// not a failure) apart from every other way the pump can stop -- an input
// error, or the pump's context (derived from the caller's ctx) being
// canceled. It is set only once pumpInput has itself confirmed
// protocol.Detach was sent successfully, so "detached" here always means
// the real thing, never merely "the pump returned with no error".
type inputOutcome struct {
	detached bool
	err      error
}

type attachInputPump func(context.Context, *os.File, *lockedFrames) inputOutcome

// Attach implements the Codex half of the DesignDoc's common Agent View
// "Open selected session" intent: it connects to the supervisor's Unix
// socket, requests the managed PTY for runID, and forwards the real
// terminal's input/output to it until the outer terminal's detach key (see
// internal/terminal) is seen or the remote session ends on its own. The
// managed process and its PTY are owned by the supervisor, not this
// client; detaching (or this process exiting) never stops them -- see the
// DesignDoc's Codex supervisor Lifetime section.
func (c Client) Attach(ctx context.Context, runID string, in *os.File, out io.Writer) error {
	return c.attach(ctx, runID, in, out, pumpAttachInput)
}

func (c Client) attach(ctx context.Context, runID string, in *os.File, out io.Writer, pumpInput attachInputPump) error {
	conn, err := net.Dial("unix", c.Socket)
	if err != nil {
		return err
	}
	defer conn.Close()
	req, _ := json.Marshal(Request{Action: "attach", RunID: runID})
	if err := protocol.Write(conn, protocol.Request, req); err != nil {
		return err
	}
	kind, b, err := protocol.Read(conn)
	if err != nil {
		return err
	}
	if kind != protocol.Response {
		return errors.New("invalid attach response")
	}
	var res Response
	if err := json.Unmarshal(b, &res); err != nil {
		return err
	}
	if !res.OK {
		return errors.New(res.Error)
	}
	restore, err := terminal.Raw(in)
	if err != nil {
		return err
	}
	defer restore()
	frames := &lockedFrames{w: conn}
	sendSize := func(redraw bool) {
		cols, rows, err := term.GetSize(int(in.Fd()))
		if err == nil {
			b, _ := json.Marshal(protocol.TerminalSize{Rows: uint16(rows), Cols: uint16(cols), Redraw: redraw})
			_ = frames.write(protocol.Resize, b)
		}
	}
	first := true
	stopResize := terminal.WatchResize(in, func() {
		sendSize(first)
		first = false
	})
	inputCtx, cancelInput := context.WithCancel(ctx)
	inputDone := make(chan struct{})
	var inputResult inputOutcome
	go func() {
		defer close(inputDone)
		inputResult = pumpInput(inputCtx, in, frames)
	}()
	type incoming struct {
		kind byte
		data []byte
		err  error
	}
	incomingFrames := make(chan incoming, 1)
	incomingDone := make(chan struct{})
	go func() {
		defer close(incomingDone)
		for {
			kind, data, err := protocol.Read(conn)
			select {
			case incomingFrames <- incoming{kind, data, err}:
			case <-inputCtx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	defer func() {
		cancelInput()
		stopResize()
		_ = conn.Close()
		<-inputDone
		<-incomingDone
	}()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-inputDone:
			if inputResult.detached {
				return nil
			}
			return inputResult.err
		case msg := <-incomingFrames:
			if msg.err != nil {
				// The connection just closed or failed. If a local Ctrl+]
				// detach already won this race by writing protocol.Detach
				// first, this is the supervisor closing the socket in
				// response -- expected, not a failure. cancelInput unblocks
				// a pump that is instead genuinely still waiting on
				// terminal input (so this join can't hang), and joining it
				// makes pumpInput -- the only place that knows whether
				// protocol.Detach was actually sent -- the sole authority
				// on that distinction, regardless of which of these two
				// goroutines this select happened to observe first.
				cancelInput()
				<-inputDone
				if inputResult.detached {
					return nil
				}
				return attachTransportError(msg.err)
			}
			switch msg.kind {
			case protocol.Output:
				if _, err := out.Write(msg.data); err != nil {
					return err
				}
			case protocol.Exit:
				return nil
			case protocol.Failure:
				return errors.New(string(msg.data))
			}
		}
	}
}

// attachTransportError normalizes an unexpected attach-connection read
// failure (one that is neither a local Ctrl+] detach nor a caller
// cancellation) into one message the caller can act on. Client.attach
// cannot tell a supervisor-initiated close it was given no reason for
// (see protocol.Failure, handled separately and left untouched by this)
// apart from a supervisor crash, a broken Unix socket, or the peer simply
// going away, so it must not guess a specific cause for any of them --
// doing so risks misattributing, say, a real supervisor crash to "the
// consumer was too slow". err is wrapped, not discarded, so
// errors.Is(result, io.EOF) and similar checks on the underlying cause
// still work for a caller that needs them.
//
// This is applied only to protocol.Read(conn)'s own errors (msg.err in
// the attach loop below), not to inputResult.err: a failure from the
// input pump can just as easily be a local terminal I/O error as a
// remote write failure, and mislabeling a local failure as an attach
// transport problem would be actively misleading.
func attachTransportError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("attach connection closed unexpectedly: %w", err)
}

func pumpAttachInput(ctx context.Context, in *os.File, frames *lockedFrames) inputOutcome {
	var scanner terminal.DetachScanner
	buf := make([]byte, 4096)
	for {
		if err := ctx.Err(); err != nil {
			return inputOutcome{err: err}
		}
		ready, err := terminal.PollInput(in, 50*time.Millisecond)
		if err != nil {
			return inputOutcome{err: err}
		}
		if !ready {
			continue
		}
		n, err := in.Read(buf)
		if n > 0 {
			before, detach := scanner.Feed(buf[:n])
			if len(before) > 0 {
				if writeErr := frames.write(protocol.Input, before); writeErr != nil {
					return inputOutcome{err: writeErr}
				}
			}
			if detach {
				if writeErr := frames.write(protocol.Detach, nil); writeErr != nil {
					return inputOutcome{err: writeErr}
				}
				return inputOutcome{detached: true}
			}
		}
		if err != nil {
			return inputOutcome{err: err}
		}
	}
}

// Attach implements codex.Dispatcher's attach capability by delegating to
// Client.Attach.
func (d Dispatcher) Attach(ctx context.Context, runID string, in *os.File, out io.Writer) error {
	return d.Client.Attach(ctx, runID, in, out)
}
