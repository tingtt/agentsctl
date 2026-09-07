//go:build darwin || linux

package claude

import (
	"context"
	"io"
	"os"
	"os/exec"
	"time"

	creackpty "github.com/creack/pty"
	"github.com/tingtt/agentsctl/internal/session"
	"github.com/tingtt/agentsctl/internal/terminal"
)

// openDetachTimeout bounds how long Open waits for the transient `claude
// attach` client to exit after sending its own detach byte/signals (see
// detachClaudeClient) before giving up -- short, since a slow detach here
// is directly visible to a waiting user (contrast renameCleanupTimeout in
// provider.go, which has no one watching it in real time).
const openDetachTimeout = 2 * time.Second

// Open implements sessionctl.Opener: it starts `claude attach <id>` on a
// PTY agentsctl owns and forwards the real terminal's input/output to it
// until the outer terminal's detach key (see internal/terminal) is seen or
// the client exits on its own -- the DesignDoc's Claude Attach/Detach
// transport. Detaching only ends this transient attach client; Claude's
// own native background session is never signaled.
func (p *Provider) Open(ctx context.Context, s session.Session, in *os.File, out io.Writer) error {
	cmd := exec.CommandContext(ctx, p.path(), "attach", s.Key.ID)
	child, err := startClaudeAttachRaw(cmd)
	if err != nil {
		return err
	}
	defer child.Close()
	restore, err := terminal.Raw(in)
	if err != nil {
		return err
	}
	defer restore()
	stopResize := terminal.WatchResize(in, func() { _ = creackpty.InheritSize(in, child) })
	defer stopResize()
	go io.Copy(out, child)
	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()
	buf := make([]byte, 4096)
	var scanner terminal.DetachScanner
	for {
		select {
		case err := <-wait:
			return normalizeExit(err)
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		ready, pollErr := terminal.PollInput(in, 100*time.Millisecond)
		if pollErr != nil {
			return pollErr
		}
		if !ready {
			continue
		}
		n, readErr := in.Read(buf)
		if n > 0 {
			before, detach := scanner.Feed(buf[:n])
			if len(before) > 0 {
				_, _ = child.Write(before)
			}
			if detach {
				return detachClaudeClient(ctx, cmd, child, wait, openDetachTimeout)
			}
		}
		if readErr != nil {
			select {
			case err := <-wait:
				return normalizeExit(err)
			default:
				return readErr
			}
		}
		select {
		case err := <-wait:
			return normalizeExit(err)
		default:
		}
	}
}
