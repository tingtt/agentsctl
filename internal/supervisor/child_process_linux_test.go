//go:build linux

package supervisor

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"testing"

	processinfo "github.com/tingtt/agentsctl/internal/process"
)

func TestDirectChildrenDiscoversProcessRelationships(t *testing.T) {
	existing := startLinuxChildHelper(t)
	existingIdentity, err := processinfo.Observe(existing.command.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	assertDirectChild(t, os.Getpid(), existingIdentity, true)

	newChild := startLinuxChildHelper(t)
	newIdentity, err := processinfo.Observe(newChild.command.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	children := directChildren(os.Getpid())
	if !containsIdentity(children, existingIdentity) || !containsIdentity(children, newIdentity) {
		t.Fatalf("direct children=%v, want existing=%v and new=%v", children, existingIdentity, newIdentity)
	}
	self, err := processinfo.Observe(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if containsIdentity(children, self) {
		t.Fatalf("direct children unexpectedly include unrelated process %v", self)
	}
	for _, want := range []processinfo.Identity{existingIdentity, newIdentity} {
		if got, observeErr := processinfo.Observe(want.PID); observeErr != nil || got != want {
			t.Fatalf("child identity=%v, observed=%v, err=%v", want, got, observeErr)
		}
	}

	stopLinuxChildHelper(t, newChild)
	assertDirectChild(t, os.Getpid(), newIdentity, false)
	stopLinuxChildHelper(t, existing)
	assertDirectChild(t, os.Getpid(), existingIdentity, false)
}

func TestParseLinuxParentPIDRejectsMalformedStatus(t *testing.T) {
	tests := []struct {
		name   string
		status string
		want   int
		ok     bool
	}{
		{name: "valid", status: "Name:\ttest\nPPid:\t42\n", want: 42, ok: true},
		{name: "missing", status: "Name:\ttest\n"},
		{name: "not numeric", status: "PPid:\tparent\n"},
		{name: "extra field", status: "PPid:\t42 extra\n"},
		{name: "lookalike key", status: "PPidExtra:\t42\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := parseLinuxParentPID([]byte(test.status))
			if got != test.want || ok != test.ok {
				t.Fatalf("parent PID=(%d, %v), want (%d, %v)", got, ok, test.want, test.ok)
			}
		})
	}
}

func TestLinuxChildHelper(t *testing.T) {
	if os.Getenv("AGENTSCTL_TEST_LINUX_CHILD") != "1" {
		return
	}
	_, _ = fmt.Fprintln(os.Stdout, "ready")
	_, _ = io.Copy(io.Discard, os.Stdin)
}

type linuxChildHelper struct {
	command *exec.Cmd
	stdin   io.WriteCloser
}

func startLinuxChildHelper(t *testing.T) *linuxChildHelper {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(executable, "-test.run=^TestLinuxChildHelper$")
	command.Env = append(os.Environ(), "AGENTSCTL_TEST_LINUX_CHILD=1")
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	helper := &linuxChildHelper{command: command, stdin: stdin}
	t.Cleanup(func() {
		if command.ProcessState == nil {
			_ = stdin.Close()
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	})
	ready, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || ready != "ready\n" {
		t.Fatalf("child readiness=%q, err=%v", ready, err)
	}
	return helper
}

func stopLinuxChildHelper(t *testing.T, helper *linuxChildHelper) {
	t.Helper()
	if err := helper.stdin.Close(); err != nil {
		t.Fatal(err)
	}
	if err := helper.command.Wait(); err != nil {
		t.Fatal(err)
	}
}

func assertDirectChild(t *testing.T, parentPID int, identity processinfo.Identity, want bool) {
	t.Helper()
	if got := containsIdentity(directChildren(parentPID), identity); got != want {
		t.Fatalf("contains child %v=%v, want %v", identity, got, want)
	}
}

func containsIdentity(identities []processinfo.Identity, want processinfo.Identity) bool {
	for _, identity := range identities {
		if identity == want {
			return true
		}
	}
	return false
}
