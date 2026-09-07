package sessionctl

import (
	"context"
	"fmt"

	"github.com/tingtt/agentsctl/internal/session"
)

// Stop terminates the process/session backing key. It fails closed if the
// provider is not configured or does not implement Stopper. The actual
// ownership/identity re-verification before any signal is sent is a
// provider/supervisor-internal safety boundary (see the DesignDoc's
// process-ownership invariants), not something Controller re-implements.
func (c Controller) Stop(ctx context.Context, key session.Key) (Result, error) {
	p, err := c.provider(key.Provider)
	if err != nil {
		return Result{}, err
	}
	s, ok := p.(Stopper)
	if !ok {
		return Result{}, fmt.Errorf("cannot stop: %s does not support stopping sessions", key.Provider)
	}
	if err := s.Stop(ctx, key); err != nil {
		return Result{}, err
	}
	return Result{Reload: true}, nil
}
