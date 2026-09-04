//go:build darwin

package process

import (
	"bytes"
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"syscall"
)

func Observe(pid int) (Identity, error) {
	info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			return Identity{}, ErrNotFound
		}
		return Identity{}, fmt.Errorf("observe process: %w", err)
	}
	if info.Proc.P_pid != int32(pid) {
		return Identity{}, ErrNotFound
	}
	started := info.Proc.P_starttime
	return Identity{PID: pid, StartTime: uint64(started.Sec)*1_000_000_000 + uint64(started.Usec)*1_000, UID: info.Eproc.Ucred.Uid}, nil
}

func name(pid int) (string, error) {
	info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return "", fmt.Errorf("observe process name: %w", err)
	}
	if info.Proc.P_pid != int32(pid) {
		return "", ErrNotFound
	}
	return cString(info.Proc.P_comm[:]), nil
}

func cString(value []byte) string {
	if end := bytes.IndexByte(value, 0); end >= 0 {
		value = value[:end]
	}
	return string(value)
}
