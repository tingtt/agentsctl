//go:build linux

package supervisor

import (
	"os"
	"strconv"
	"strings"

	processinfo "github.com/tingtt/agentsctl/internal/process"
)

func directChildren(parentPID int) []processinfo.Identity {
	children, err := os.ReadFile("/proc/" + strconv.Itoa(parentPID) + "/task/" + strconv.Itoa(parentPID) + "/children")
	if err != nil {
		return nil
	}
	identities := make([]processinfo.Identity, 0, len(strings.Fields(string(children))))
	for _, field := range strings.Fields(string(children)) {
		pid, parseErr := strconv.Atoi(field)
		if parseErr != nil {
			continue
		}
		identity, observeErr := processinfo.Observe(pid)
		if observeErr == nil {
			identities = append(identities, identity)
		}
	}
	return identities
}
