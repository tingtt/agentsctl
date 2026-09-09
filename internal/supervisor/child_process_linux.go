//go:build linux

package supervisor

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	processinfo "github.com/tingtt/agentsctl/internal/process"
)

func directChildren(parentPID int) []processinfo.Identity {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var children []processinfo.Identity
	for _, entry := range entries {
		pid, parseErr := strconv.Atoi(entry.Name())
		if parseErr != nil || pid <= 0 {
			continue
		}
		if ppid, ok := linuxParentPID(pid); !ok || ppid != parentPID {
			continue
		}
		identity, observeErr := processinfo.Observe(pid)
		if observeErr != nil {
			continue
		}
		if ppid, ok := linuxParentPID(pid); !ok || ppid != parentPID {
			continue
		}
		if matchErr := processinfo.Match(identity); matchErr != nil {
			continue
		}
		children = append(children, identity)
	}
	return children
}

func linuxParentPID(pid int) (int, bool) {
	status, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "status"))
	if err != nil {
		return 0, false
	}
	return parseLinuxParentPID(status)
}

func parseLinuxParentPID(status []byte) (int, bool) {
	for _, line := range strings.Split(string(status), "\n") {
		if !strings.HasPrefix(line, "PPid:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[0] != "PPid:" {
			return 0, false
		}
		parentPID, parseErr := strconv.Atoi(fields[1])
		return parentPID, parseErr == nil && parentPID >= 0
	}
	return 0, false
}
