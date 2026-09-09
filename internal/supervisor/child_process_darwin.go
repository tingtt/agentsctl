//go:build darwin

package supervisor

import (
	processinfo "github.com/tingtt/agentsctl/internal/process"
	"golang.org/x/sys/unix"
)

func directChildren(parentPID int) []processinfo.Identity {
	processes, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil
	}
	var children []processinfo.Identity
	for _, candidate := range processes {
		if candidate.Eproc.Ppid == int32(parentPID) {
			started := candidate.Proc.P_starttime
			children = append(children, processinfo.Identity{
				PID:       int(candidate.Proc.P_pid),
				StartTime: uint64(started.Sec)*1_000_000_000 + uint64(started.Usec)*1_000,
				UID:       candidate.Eproc.Ucred.Uid,
			})
		}
	}
	return children
}
