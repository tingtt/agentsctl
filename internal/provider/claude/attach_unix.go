//go:build darwin || linux

package claude

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"

	creackpty "github.com/creack/pty"
	"golang.org/x/term"
)

// startClaudeAttachRaw is creackpty.StartWithSize(cmd, nil), except the
// pty's slave side is switched to raw mode BEFORE the child process
// starts rather than after.
//
// This closes a real startup race: creackpty.Start leaves the slave in
// the kernel's default cooked mode (ISIG etc. enabled) until the child
// process gets around to calling its own raw-mode setup. If the detach
// byte (literal Ctrl+Z, 0x1a -- Claude's own attach-client detach
// convention, distinct from agentsctl's outer-terminal detach key) arrives
// in that window, the kernel's line discipline intercepts it as the VSUSP
// special character and raises SIGTSTP instead of ever delivering it to
// the app as input -- verified against the installed `claude` CLI: sending
// Ctrl+] immediately after attach starts visibly echoes "^Z" into the
// client's output and the client never exits, so the detach silently does
// nothing. Configuring raw mode here, before fork/exec, means every byte
// -- no matter how early it arrives -- is queued as literal data for the
// child to read once it starts, never intercepted as a signal. The
// child's own subsequent raw-mode call (every interactive `claude attach`
// session makes one) is a harmless no-op once this has already run.
//
// Shared by Open (an interactive attach with a real outer terminal) and
// the native rename transport (a transient, headless attach client) --
// both start a `claude attach` child the same way.
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
// This byte-consumption behavior is only reachable at all because the
// client is started via startClaudeAttachRaw, not a bare creackpty.Start:
// without that, a detach sent early enough to race the client's own
// raw-mode setup is intercepted by the kernel as SIGTSTP before the
// client ever sees it as data. See startClaudeAttachRaw's doc comment for
// how that was found and closed.
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

func normalizeExit(err error) error {
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 0 {
		return nil
	}
	return err
}
