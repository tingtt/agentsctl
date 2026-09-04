//go:build linux

package process

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

func Observe(pid int) (Identity, error) {
	path := fmt.Sprintf("/proc/%d/stat", pid)
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Identity{}, ErrNotFound
	}
	if err != nil {
		return Identity{}, err
	}
	end := strings.LastIndexByte(string(b), ')')
	if end < 0 {
		return Identity{}, errors.New("malformed proc stat")
	}
	fields := strings.Fields(string(b[end+2:]))
	if len(fields) <= 19 {
		return Identity{}, errors.New("truncated proc stat")
	}
	start, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return Identity{}, err
	}
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return Identity{}, err
	}
	return Identity{PID: pid, StartTime: start, UID: st.Uid}, nil
}

func name(pid int) (string, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	if errors.Is(err, os.ErrNotExist) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}
