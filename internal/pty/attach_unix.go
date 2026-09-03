//go:build darwin || linux

package pty

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
	"time"

	creackpty "github.com/creack/pty"
	"github.com/tingtt/agentsctl/internal/protocol"
	"github.com/tingtt/agentsctl/internal/supervisor"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

const DetachKey byte = 0x1d

type lockedFrames struct {
	mu sync.Mutex
	w  io.Writer
}

func (f *lockedFrames) write(kind byte, b []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return protocol.Write(f.w, kind, b)
}

func AttachCodex(ctx context.Context, socket, runID string, in *os.File, out io.Writer) error {
	conn, err := net.Dial("unix", socket)
	if err != nil {
		return err
	}
	defer conn.Close()
	req, _ := json.Marshal(supervisor.Request{Action: "attach", RunID: runID})
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
	var res supervisor.Response
	if err := json.Unmarshal(b, &res); err != nil {
		return err
	}
	if !res.OK {
		return errors.New(res.Error)
	}
	restore, err := raw(in)
	if err != nil {
		return err
	}
	defer restore()
	frames := &lockedFrames{w: conn}
	stopResize := resizeTerminal(in, frames)
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
		ready, err := pollInput(in, 50*time.Millisecond)
		if err != nil {
			return err
		}
		if !ready {
			continue
		}
		buf := make([]byte, 4096)
		n, err := in.Read(buf)
		if n > 0 {
			before, detach, _ := splitDetach(buf[:n])
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

func AttachClaude(ctx context.Context, path, id string, in *os.File, out io.Writer, timeout time.Duration) error {
	if path == "" {
		path = "claude"
	}
	cmd := exec.CommandContext(ctx, path, "attach", id)
	child, err := creackpty.Start(cmd)
	if err != nil {
		return err
	}
	defer child.Close()
	restore, err := raw(in)
	if err != nil {
		return err
	}
	defer restore()
	stopResize := resizeChildTerminal(in, child)
	defer stopResize()
	go io.Copy(out, child)
	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()
	buf := make([]byte, 4096)
	for {
		select {
		case err := <-wait:
			return normalizeExit(err)
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		ready, pollErr := pollInput(in, 100*time.Millisecond)
		if pollErr != nil {
			return pollErr
		}
		if !ready {
			continue
		}
		n, readErr := in.Read(buf)
		if n > 0 {
			before, detach, after := splitDetach(buf[:n])
			if len(before) > 0 {
				_, _ = child.Write(before)
			}
			if detach {
				_, _ = child.Write(claudeNativeDetachSequence())
				timer := time.NewTimer(timeout)
				select {
				case err := <-wait:
					timer.Stop()
					return normalizeExit(err)
				case <-timer.C:
					fmt.Fprintln(out, "\r\nagentsctl: Claude native detach timed out; attachment remains active")
				case <-ctx.Done():
					timer.Stop()
					return ctx.Err()
				}
				if len(after) > 0 {
					_, _ = child.Write(after)
				}
			}
		}
		if readErr != nil {
			select {
			case err := <-wait:
				return normalizeExit(err)
			default:
				return readErr
			}
		}
		select {
		case err := <-wait:
			return normalizeExit(err)
		default:
		}
	}
}
func claudeNativeDetachSequence() []byte { return []byte{0x1a} }

func forwardInput(r io.Reader, send func([]byte) error, detach func() error) error {
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			before, found, _ := splitDetach(buf[:n])
			if len(before) > 0 {
				if e := send(before); e != nil {
					return e
				}
			}
			if found {
				return detach()
			}
		}
		if err != nil {
			return err
		}
	}
}
func splitDetach(b []byte) (before []byte, found bool, after []byte) {
	for i, v := range b {
		if v == DetachKey {
			return b[:i], true, b[i+1:]
		}
	}
	return b, false, nil
}
func raw(f *os.File) (func(), error) {
	if !term.IsTerminal(int(f.Fd())) {
		return func() {}, nil
	}
	old, err := term.MakeRaw(int(f.Fd()))
	if err != nil {
		return nil, err
	}
	return func() { _ = term.Restore(int(f.Fd()), old) }, nil
}
func resizeTerminal(f *os.File, w *lockedFrames) func() {
	if !term.IsTerminal(int(f.Fd())) {
		return func() {}
	}
	send := func() {
		cols, rows, err := term.GetSize(int(f.Fd()))
		if err == nil {
			b, _ := json.Marshal(struct{ Rows, Cols uint16 }{uint16(rows), uint16(cols)})
			_ = w.write(protocol.Resize, b)
		}
	}
	send()
	ch := make(chan os.Signal, 1)
	done := make(chan struct{})
	signal.Notify(ch, syscall.SIGWINCH)
	go func() {
		defer signal.Stop(ch)
		for {
			select {
			case <-ch:
				send()
			case <-done:
				return
			}
		}
	}()
	return func() { close(done) }
}

func resizeChildTerminal(f *os.File, child *os.File) func() {
	if !term.IsTerminal(int(f.Fd())) {
		return func() {}
	}
	send := func() { _ = creackpty.InheritSize(f, child) }
	send()
	ch := make(chan os.Signal, 1)
	done := make(chan struct{})
	signal.Notify(ch, syscall.SIGWINCH)
	go func() {
		defer signal.Stop(ch)
		for {
			select {
			case <-ch:
				send()
			case <-done:
				return
			}
		}
	}()
	return func() { close(done) }
}

func pollInput(f *os.File, timeout time.Duration) (bool, error) {
	fds := []unix.PollFd{{Fd: int32(f.Fd()), Events: unix.POLLIN}}
	n, err := unix.Poll(fds, int(timeout.Milliseconds()))
	if err != nil && errors.Is(err, syscall.EINTR) {
		return false, nil
	}
	return n > 0, err
}
func normalizeExit(err error) error {
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 0 {
		return nil
	}
	return err
}
