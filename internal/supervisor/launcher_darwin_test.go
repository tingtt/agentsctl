//go:build darwin

package supervisor

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"

	"github.com/creack/pty"
)

func TestLegacyPTYAttributesReproduceOperationNotPermitted(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(executable, "-test.run=^$")
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	terminal, err := pty.Start(command)
	if terminal != nil {
		_ = terminal.Close()
	}
	if command.Process != nil {
		_ = command.Process.Kill()
		_, _ = command.Process.Wait()
	}
	if !errors.Is(err, syscall.EPERM) {
		t.Fatalf("legacy Setpgid + PTY start error=%v, want EPERM", err)
	}
}
