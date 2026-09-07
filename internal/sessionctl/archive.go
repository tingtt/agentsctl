package sessionctl

import (
	"context"
	"fmt"

	"github.com/tingtt/agentsctl/internal/session"
)

// Archive removes key's session from the default (non-archived) view. It
// fails closed if the provider is not configured or does not implement
// Archiver.
func (c Controller) Archive(ctx context.Context, key session.Key) (Result, error) {
	p, err := c.provider(key.Provider)
	if err != nil {
		return Result{}, err
	}
	a, ok := p.(Archiver)
	if !ok {
		return Result{}, fmt.Errorf("cannot archive: %s does not support archiving sessions", key.Provider)
	}
	if err := a.Archive(ctx, key); err != nil {
		return Result{}, err
	}
	return Result{Reload: true}, nil
}
