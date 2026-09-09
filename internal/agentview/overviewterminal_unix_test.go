//go:build darwin || linux

package agentview

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/creack/pty"
)

func terminalMode(t *testing.T, terminal *os.File) string {
	t.Helper()
	cmd := exec.Command("stty", "-g")
	cmd.Stdin = terminal
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Run(); err != nil {
		t.Fatalf("stty -g: %v: %s", err, output.String())
	}
	return strings.TrimSpace(output.String())
}

func TestOverviewTerminalSuspendsAndResumesOwnership(t *testing.T) {
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	defer slave.Close()

	original := terminalMode(t, slave)
	var output bytes.Buffer
	terminal := &overviewTerminal{input: slave, output: &output}
	if err := terminal.start(); err != nil {
		t.Fatal(err)
	}
	raw := terminalMode(t, slave)
	if raw == original {
		t.Fatal("overview start did not put the PTY into raw mode")
	}
	if err := terminal.suspend(); err != nil {
		t.Fatal(err)
	}
	if got := terminalMode(t, slave); got != original {
		t.Fatalf("suspended mode=%q, want original %q", got, original)
	}
	if err := terminal.resume(); err != nil {
		t.Fatal(err)
	}
	if got := terminalMode(t, slave); got == original {
		t.Fatal("overview resume did not restore raw mode")
	}
	if err := terminal.close(); err != nil {
		t.Fatal(err)
	}
	if got := terminalMode(t, slave); got != original {
		t.Fatalf("closed mode=%q, want original %q", got, original)
	}

	want := "\x1b[?1049h\x1b[?25l" +
		"\x1b[0m\x1b[?25h\x1b[?1049l" +
		"\x1b[?1049h\x1b[?25l" +
		"\x1b[0m\x1b[?25h\x1b[?1049l"
	if got := output.String(); got != want {
		t.Fatalf("terminal sequence order=%q, want %q", got, want)
	}
}
