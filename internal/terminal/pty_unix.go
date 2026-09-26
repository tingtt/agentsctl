//go:build darwin || linux

package terminal

import (
	"os"
	"os/exec"
	"syscall"

	creackpty "github.com/creack/pty"
	"golang.org/x/term"
)

// StartRawPTY starts cmd on a new PTY and returns its master side. The
// PTY's slave side is switched to raw mode, and sized to size when size is
// not nil, before the child starts rather than after: a PTY left in the
// kernel's default cooked mode until the child sets up its own raw mode
// turns a control byte arriving in that window (Ctrl+Z, Ctrl+C) into a
// signal instead of input. The child runs in a new session with the PTY as
// its controlling terminal, so its process group is its own; cmd's stdio
// left nil is connected to the PTY.
func StartRawPTY(cmd *exec.Cmd, size *creackpty.Winsize) (*os.File, error) {
	master, slave, err := creackpty.Open()
	if err != nil {
		return nil, err
	}
	defer func() { _ = slave.Close() }()
	if _, err := term.MakeRaw(int(slave.Fd())); err != nil {
		_ = master.Close()
		return nil, err
	}
	if size != nil {
		if err := creackpty.Setsize(master, size); err != nil {
			_ = master.Close()
			return nil, err
		}
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
