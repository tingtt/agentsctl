package sessionctl

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/tingtt/agentsctl/internal/session"
)

// Open performs the common Agent View intent (see Opener's doc comment),
// routing to s.Key.Provider's Opener implementation. It fails closed if
// the provider is not configured or does not implement Opener -- Agent
// View never branches on session.ProviderID or a concrete provider type to
// decide how to open a session (see the DesignDoc's "TUI must not know
// provider implementation details").
func (c Controller) Open(ctx context.Context, s session.Session, in *os.File, out io.Writer) (Result, error) {
	p, err := c.provider(s.Key.Provider)
	if err != nil {
		return Result{}, err
	}
	o, ok := p.(Opener)
	if !ok {
		return Result{}, fmt.Errorf("cannot open: %s does not support opening a session", s.Key.Provider)
	}
	if err := o.Open(ctx, s, in, out); err != nil {
		return Result{}, err
	}
	return Result{Reload: true}, nil
}
