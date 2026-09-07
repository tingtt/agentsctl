// Package writerlock checks whether a specific process holds the exclusive
// flock on a Codex app-server thread's writer-lock file -- Codex's own
// on-disk convention for which process may currently write to a thread
// (see the DesignDoc's Codex run-to-thread binding). This is Codex-
// specific: nothing outside internal/provider/codex uses it, so it stays
// out of the generic internal/process package (identity/ownership
// concerns any provider could need), depending on it rather than the
// reverse.
package writerlock

import (
	"errors"
	"golang.org/x/sys/unix"
	"os"
)

func lockContended(path string) (os.FileInfo, bool, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, false, err
	}
	if !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 {
		return nil, false, errors.New("untrusted writer lock")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return nil, false, errors.New("writer lock changed")
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err == nil {
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		return opened, false, nil
	} else if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return opened, true, nil
	} else {
		return nil, false, err
	}
}
