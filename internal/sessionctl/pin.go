package sessionctl

import (
	"errors"

	"github.com/tingtt/agentsctl/internal/session"
)

// TogglePin changes and returns the pinned state for key. Pin/unpin never
// touches provider (remote/native) state -- see the DesignDoc's "Pin /
// Unpin 操作は即時に表示へ反映するため、provider の catalog を再取得せ
// ず..." -- so it always returns a local Patch, never Reload.
func (c Controller) TogglePin(key session.Key) (Result, error) {
	if c.Pins == nil {
		return Result{}, errors.New("pin metadata store is not configured")
	}
	pinned, err := c.Pins.TogglePinned(key.String())
	if err != nil {
		return Result{}, err
	}
	return Result{Patch: &Patch{Key: key, Pinned: &pinned}}, nil
}
