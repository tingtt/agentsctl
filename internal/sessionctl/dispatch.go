package sessionctl

import (
	"context"
	"fmt"

	"github.com/tingtt/agentsctl/internal/session"
)

// Dispatch starts a new session on the named provider. It fails closed
// (with no call into the provider) if the provider is not configured or
// does not implement Dispatcher.
func (c Controller) Dispatch(ctx context.Context, id session.ProviderID, prompt, cwd string) (session.Session, Result, error) {
	p, err := c.provider(id)
	if err != nil {
		return session.Session{}, Result{}, err
	}
	d, ok := p.(Dispatcher)
	if !ok {
		return session.Session{}, Result{}, fmt.Errorf("cannot dispatch: %s does not support starting new sessions", id)
	}
	s, err := d.Dispatch(ctx, prompt, cwd)
	if err != nil {
		return session.Session{}, Result{}, err
	}
	return s, Result{Reload: true}, nil
}

// Dispatchable reports whether id both exists and implements Dispatcher,
// so a caller (Agent View's composer) can decide whether the provider is
// eligible for the composer's provider cycle without attempting a dispatch
// and without switching on session.ProviderID itself.
func (c Controller) Dispatchable(id session.ProviderID) bool {
	p, err := c.provider(id)
	if err != nil {
		return false
	}
	_, ok := p.(Dispatcher)
	return ok
}
