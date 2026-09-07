//go:build darwin || linux

package supervisor

import (
	"context"
	"encoding/json"
	"errors"
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

type attachInputPump func(context.Context, *os.File, *lockedFrames) error

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
	var inputErr error
	go func() {
		defer close(inputDone)
		inputErr = pumpInput(inputCtx, in, frames)
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
			return inputErr
		case msg := <-incomingFrames:
			if msg.err != nil {
				return msg.err
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

func pumpAttachInput(ctx context.Context, in *os.File, frames *lockedFrames) error {
	var scanner terminal.DetachScanner
	buf := make([]byte, 4096)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		ready, err := terminal.PollInput(in, 50*time.Millisecond)
		if err != nil {
			return err
		}
		if !ready {
			continue
		}
		n, err := in.Read(buf)
		if n > 0 {
			before, detach := scanner.Feed(buf[:n])
			if len(before) > 0 {
				if writeErr := frames.write(protocol.Input, before); writeErr != nil {
					return writeErr
				}
			}
			if detach {
				return frames.write(protocol.Detach, nil)
			}
		}
		if err != nil {
			return err
		}
	}
}

// Attach implements codex.Dispatcher's attach capability by delegating to
// Client.Attach.
func (d Dispatcher) Attach(ctx context.Context, runID string, in *os.File, out io.Writer) error {
	return d.Client.Attach(ctx, runID, in, out)
}
