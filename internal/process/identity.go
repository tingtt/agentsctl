package process

import "errors"

var ErrNotFound = errors.New("process not found")

type Identity struct {
	PID       int
	StartTime uint64
	UID       uint32
}

func (i Identity) Valid() bool { return i.PID > 0 && i.StartTime > 0 }
func Match(expected Identity) error {
	got, err := Observe(expected.PID)
	if err != nil {
		return err
	}
	if got != expected {
		return errors.New("process identity changed")
	}
	return nil
}

// Name returns the kernel-recorded executable name for pid. It is deliberately
// narrower than an argv lookup: callers use it only as an additional identity
// check before signalling a process.
func Name(pid int) (string, error) { return name(pid) }
