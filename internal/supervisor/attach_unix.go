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

// Attach implements the Codex half of the DesignDoc's common Agent View
// "Open selected session" intent: it connects to the supervisor's Unix
// socket, requests the managed PTY for runID, and forwards the real
// terminal's input/output to it until the outer terminal's detach key (see
// internal/terminal) is seen or the remote session ends on its own. The
// managed process and its PTY are owned by the supervisor, not this
// client; detaching (or this process exiting) never stops them -- see the
// DesignDoc's Codex supervisor Lifetime section.
func (c Client) Attach(ctx context.Context, runID string, in *os.File, out io.Writer) error {
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
	defer stopResize()
	type incoming struct {
		kind byte
		data []byte
		err  error
	}
	incomingFrames := make(chan incoming, 1)
	go func() {
		for {
			kind, data, err := protocol.Read(conn)
			incomingFrames <- incoming{kind, data, err}
			if err != nil {
				return
			}
		}
	}()
	var scanner terminal.DetachScanner
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
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
		default:
		}
		ready, err := terminal.PollInput(in, 50*time.Millisecond)
		if err != nil {
			return err
		}
		if !ready {
			continue
		}
		buf := make([]byte, 4096)
		n, err := in.Read(buf)
		if n > 0 {
			before, detach := scanner.Feed(buf[:n])
			if len(before) > 0 {
				if err := frames.write(protocol.Input, before); err != nil {
					return err
				}
			}
			if detach {
				if err := frames.write(protocol.Detach, nil); err != nil {
					return err
				}
				return nil
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
