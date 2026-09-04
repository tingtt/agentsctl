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
	"strconv"
	"strings"
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
	var scanner detachScanner
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
			before, detach := scanner.feed(buf[:n])
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
	child, err := startClaudeAttachRaw(cmd)
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
	var scanner detachScanner
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
			before, detach := scanner.feed(buf[:n])
			if len(before) > 0 {
				_, _ = child.Write(before)
			}
			if detach {
				return detachClaudeClient(ctx, cmd, child, wait, timeout)
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

// startClaudeAttachRaw is creackpty.StartWithSize(cmd, nil), except the
// pty's slave side is switched to raw mode BEFORE the child process
// starts rather than after.
//
// This closes a real startup race: creackpty.Start leaves the slave in
// the kernel's default cooked mode (ISIG etc. enabled) until the child
// process gets around to calling its own raw-mode setup. If the detach
// byte (literal Ctrl+Z, 0x1a) arrives in that window, the kernel's line
// discipline intercepts it as the VSUSP special character and raises
// SIGTSTP instead of ever delivering it to the app as input -- verified
// against the installed `claude` CLI: sending Ctrl+] immediately after
// attach starts visibly echoes "^Z" into the client's output and the
// client never exits, so the detach silently does nothing. Configuring
// raw mode here, before fork/exec, means every byte -- no matter how
// early it arrives -- is queued as literal data for the child to read
// once it starts, never intercepted as a signal. The child's own
// subsequent raw-mode call (every interactive `claude attach` session
// makes one) is a harmless no-op once this has already run.
func startClaudeAttachRaw(cmd *exec.Cmd) (*os.File, error) {
	master, slave, err := creackpty.Open()
	if err != nil {
		return nil, err
	}
	defer func() { _ = slave.Close() }()
	if _, err := term.MakeRaw(int(slave.Fd())); err != nil {
		_ = master.Close()
		return nil, err
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true
	cmd.SysProcAttr.Setctty = true
	if cmd.Stdin == nil {
		cmd.Stdin = slave
	}
	if cmd.Stdout == nil {
		cmd.Stdout = slave
	}
	if cmd.Stderr == nil {
		cmd.Stderr = slave
	}
	if err := cmd.Start(); err != nil {
		_ = master.Close()
		return nil, err
	}
	return master, nil
}

// detachClaudeClient ends only the `claude attach` client process; the
// background session is owned and kept alive by Claude's native daemon
// (verified with the installed CLI: `claude agents --json --all` shows the
// same session, still running, after the client below exits).
//
// The client is verified (against the installed `claude` build) to treat a
// literal Ctrl+Z byte (0x1a) on its stdin as its own "detach" hotkey: it
// unwinds its raw terminal mode and alternate screen and exits on its own,
// which is why this is tried first — it is the client restoring the real
// terminal itself, rather than agentsctl guessing at cleanup. SIGHUP/SIGTERM
// to the client's process group are kept only as a fallback for a client
// that does not consume the byte (e.g. mid some other input-owning submode),
// never as the primary path.
//
// This byte-consumption behavior is only reachable at all because
// AttachClaude starts the client via startClaudeAttachRaw, not a bare
// creackpty.Start: without that, a detach sent early enough to race the
// client's own raw-mode setup is intercepted by the kernel as SIGTSTP
// before the client ever sees it as data. See startClaudeAttachRaw's doc
// comment for how that was found and closed.
func detachClaudeClient(ctx context.Context, cmd *exec.Cmd, child *os.File, wait <-chan error, timeout time.Duration) error {
	if cmd.Process == nil {
		return errors.New("Claude attach client did not start")
	}
	_, _ = child.Write([]byte{0x1a})
	if _, ok := waitForAttachment(ctx, wait, timeout); ok {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return nil
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGHUP); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("detach Claude client: %w", err)
	}
	if _, ok := waitForAttachment(ctx, wait, timeout); ok {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return nil
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("terminate Claude attach client: %w", err)
	}
	if _, ok := waitForAttachment(ctx, wait, timeout); ok {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return nil
	}
	return errors.New("Claude attach client did not exit after detach")
}

func waitForAttachment(ctx context.Context, wait <-chan error, timeout time.Duration) (error, bool) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-wait:
		return err, true
	case <-ctx.Done():
		return ctx.Err(), true
	case <-timer.C:
		return nil, false
	}
}

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

// detachScanner recognizes the detach key (Ctrl+]) in the raw byte stream
// from the real, outer terminal -- whether that terminal sends it as the
// classic C0 byte (DetachKey, 0x1d) or as a CSI-u ("Kitty keyboard
// protocol" / xterm modifyOtherKeys) escape sequence instead.
//
// The CSI-u path is real, not hypothetical: the attached child (`claude`,
// and potentially other clients) negotiates that extension for its own
// enhanced key handling by writing an escape sequence to its pty, which
// AttachClaude/AttachCodex proxy verbatim to the real outer terminal via
// their output copy. The outer terminal has no way to know that sequence
// "really" came from a nested child rather than agentsctl itself, so a
// terminal that honors the extension (confirmed: iTerm2) switches its own
// key encoding in response -- meaning it starts sending Ctrl+] as
// `ESC [ 93 ; 5 u` (93 = ']', 5 = 1+Ctrl) instead of the literal byte.
// Terminal.app doesn't support the extension and keeps sending the
// classic byte regardless, which is why the same build detaches cleanly
// there but not in iTerm2: a scanner that only ever looked for the literal
// byte would forward the CSI-u form straight into the child as ordinary
// (unbound, silently ignored) input instead of detaching, matching the
// exact "Ctrl+] does nothing at all" symptom reported against iTerm2.
//
// One scanner instance must be used for an entire attach session (not
// recreated per read): a CSI-u sequence can arrive split across separate
// Read() calls, and the scanner holds back a possibly-incomplete trailing
// escape sequence across feed calls rather than risk forwarding partial
// escape bytes to the child or splitting a real match in two.
type detachScanner struct {
	pending []byte
}

// feed processes one new chunk of raw terminal input and returns the
// bytes that should be forwarded to the child right now (deliver) and
// whether the detach key was found in this chunk (detach). Mirrors
// splitDetach's contract: when detach is true, everything from the
// detach key onward in this chunk (including any bytes after it) is
// intentionally dropped, not delivered.
func (d *detachScanner) feed(chunk []byte) (deliver []byte, detach bool) {
	buf := append(d.pending, chunk...)
	d.pending = nil
	for i := 0; i < len(buf); i++ {
		if buf[i] == DetachKey {
			return buf[:i], true
		}
		if buf[i] != 0x1b {
			continue
		}
		n, complete := scanEscape(buf[i:])
		if !complete {
			// Possibly-incomplete escape sequence at the tail: hold it
			// back for the next read instead of forwarding partial bytes
			// or risking a match split across two chunks.
			d.pending = append(d.pending, buf[i:]...)
			return buf[:i], false
		}
		if isCtrlBracketCSIu(buf[i : i+n]) {
			return buf[:i], true
		}
		i += n - 1 // -1 to offset the loop's i++
	}
	return buf, false
}

// scanEscape reports how many leading bytes of buf (which starts with ESC)
// form one complete escape sequence, and whether a complete sequence was
// actually found (false means buf is truncated so far and more input is
// needed). Only the CSI form (ESC '[' ... final-byte in 0x40-0x7E, which
// covers CSI-u) is parsed structurally; any other escape form is treated
// as complete after its second byte so it is never held back indefinitely.
func scanEscape(buf []byte) (n int, complete bool) {
	if len(buf) < 2 {
		return 0, false
	}
	if buf[1] != '[' {
		return 2, true
	}
	for i := 2; i < len(buf); i++ {
		if buf[i] >= 0x40 && buf[i] <= 0x7e {
			return i + 1, true
		}
	}
	return 0, false
}

// isCtrlBracketCSIu reports whether seq is a complete CSI-u sequence
// encoding Ctrl+']' (unicode codepoint 93), in either the plain
// (`93;5u`) or event-typed (`93;5:1u`) modifier form, and tolerating an
// alternate-key-codes suffix on the key-code field itself (`93:125;5u`).
// The modifier field is 1 + the sum of active modifier bits (Shift=1,
// Alt=2, Ctrl=4, ...); Ctrl being held is (mod-1)&4 != 0.
func isCtrlBracketCSIu(seq []byte) bool {
	if len(seq) < 4 || seq[0] != 0x1b || seq[1] != '[' || seq[len(seq)-1] != 'u' {
		return false
	}
	body := string(seq[2 : len(seq)-1])
	parts := strings.SplitN(body, ";", 2)
	keyCode := parts[0]
	if idx := strings.IndexByte(keyCode, ':'); idx >= 0 {
		keyCode = keyCode[:idx]
	}
	if keyCode != "93" {
		return false
	}
	if len(parts) < 2 {
		return false // no modifier field at all -- not Ctrl+]
	}
	modField := parts[1]
	if idx := strings.IndexByte(modField, ':'); idx >= 0 {
		modField = modField[:idx]
	}
	mod, err := strconv.Atoi(modField)
	if err != nil || mod < 1 {
		return false
	}
	return (mod-1)&0x4 != 0
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
	send := func(redraw bool) {
		cols, rows, err := term.GetSize(int(f.Fd()))
		if err == nil {
			b, _ := json.Marshal(protocol.TerminalSize{Rows: uint16(rows), Cols: uint16(cols), Redraw: redraw})
			_ = w.write(protocol.Resize, b)
		}
	}
	send(true)
	ch := make(chan os.Signal, 1)
	done := make(chan struct{})
	signal.Notify(ch, syscall.SIGWINCH)
	go func() {
		defer signal.Stop(ch)
		for {
			select {
			case <-ch:
				send(false)
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
