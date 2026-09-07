package sessionctl

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/tingtt/agentsctl/internal/session"
)

// Rename changes key's display name. It fails closed if the provider is
// not configured or does not implement Renamer. A Renamer implementation's
// contract requires it not return nil until the new name is confirmed by
// the provider's own native state (see provider/claude's native-catalog
// confirmation) -- Rename relies on that contract to apply name as an
// already-confirmed local Patch, without a provider List round-trip just
// to learn back a value already known to be current.
func (c Controller) Rename(ctx context.Context, key session.Key, name string) (Result, error) {
	if strings.TrimSpace(name) == "" {
		return Result{}, errors.New("name must not be empty")
	}
	p, err := c.provider(key.Provider)
	if err != nil {
		return Result{}, err
	}
	r, ok := p.(Renamer)
	if !ok {
		return Result{}, fmt.Errorf("cannot rename: %s does not support renaming sessions", key.Provider)
	}
	if err := r.Rename(ctx, key, name); err != nil {
		return Result{}, err
	}
	return Result{Patch: &Patch{Key: key, Name: &name}}, nil
}
