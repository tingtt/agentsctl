//go:build linux

package process

import (
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"os"
	"strconv"
	"strings"
	"syscall"
)

func OwnsWriterLock(path string, want Identity) (bool, error) {
	info, contended, err := lockContended(path)
	if err != nil || !contended {
		return false, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false, errors.New("writer lock identity unavailable")
	}
	b, err := os.ReadFile("/proc/locks")
	if err != nil {
		return false, err
	}
	owners := map[int]bool{}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 6 || contains(f, "->") {
			continue
		}
		var major, minor uint32
		var inode uint64
		if _, e := fmt.Sscanf(f[5], "%x:%x:%d", &major, &minor, &inode); e != nil {
			continue
		}
		if major == unix.Major(uint64(st.Dev)) && minor == unix.Minor(uint64(st.Dev)) && inode == st.Ino {
			pid, e := strconv.Atoi(f[4])
			if e != nil {
				return false, e
			}
			owners[pid] = true
		}
	}
	if len(owners) != 1 || !owners[want.PID] {
		return false, nil
	}
	if err := Match(want); err != nil {
		return false, err
	}
	return true, nil
}
func contains(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}
