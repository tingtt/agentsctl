//go:build darwin || linux

package claude

import (
	"context"
	"time"
)

// NewNativeRenamer returns the real NativeRenamer -- the transient-attach
// transport in rename_transport_unix.go -- for callers (main.go) to wire
// into Provider.Renamer. Only available on the platforms this package's
// PTY primitives actually support.
func NewNativeRenamer() NativeRenamer { return ptyRenamer{} }

type ptyRenamer struct{}

func (ptyRenamer) Send(ctx context.Context, path, id, name string) (func(context.Context, time.Duration) error, error) {
	return sendClaudeRename(ctx, path, id, name)
}
