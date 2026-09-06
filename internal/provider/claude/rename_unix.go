//go:build darwin || linux

package claude

import (
	"context"
	"time"

	"github.com/tingtt/agentsctl/internal/pty"
)

// NewNativeRenamer returns the real NativeRenamer -- internal/pty's
// transient-attach transport -- for callers (main.go) to wire into
// Provider.Renamer. Only available on the platforms internal/pty's PTY
// primitives actually support.
func NewNativeRenamer() NativeRenamer { return ptyRenamer{} }

type ptyRenamer struct{}

func (ptyRenamer) Send(ctx context.Context, path, id, name string) (func(context.Context, time.Duration) error, error) {
	return pty.SendClaudeRename(ctx, path, id, name)
}
