//go:build darwin

package process

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

func OwnsWriterLock(path string, want Identity) (bool, error) {
	info, contended, err := lockContended(path)
	if err != nil || !contended {
		return false, err
	}
	out, err := exec.Command("lsof", "-Fpu", "--", path).Output()
	if err != nil {
		return false, err
	}
	pids := map[int]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "p") {
			pid, e := strconv.Atoi(line[1:])
			if e != nil {
				return false, e
			}
			pids[pid] = true
		}
	}
	if len(pids) != 1 || !pids[want.PID] {
		return false, nil
	}
	if err := Match(want); err != nil {
		return false, err
	}
	after, err := os.Stat(path)
	if err != nil || !os.SameFile(info, after) {
		return false, errors.New("writer lock changed")
	}
	return true, nil
}
