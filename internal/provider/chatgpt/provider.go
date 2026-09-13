package chatgpt

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/tingtt/agentsctl/internal/session"
	"github.com/tingtt/agentsctl/internal/sessionctl"
)

// Provider lists and opens conversations from one configured ChatGPT
// Project. Browser authentication and undocumented endpoint behavior stay
// behind its private runtime.
type Provider struct {
	config    Config
	browser   browser
	configErr error
}

var (
	_ sessionctl.Source = (*Provider)(nil)
	_ sessionctl.Opener = (*Provider)(nil)
)

// New returns a provider for config using the production browser runtime.
func New(config Config) *Provider {
	return &Provider{config: config, browser: newRuntime()}
}

// NewUnavailable returns a provider whose List reports configurationError.
// Registering it preserves provider-level failure isolation: Agent View can
// warn about the explicit ChatGPT configuration while healthy providers load.
func NewUnavailable(configurationError error) *Provider {
	return &Provider{configErr: configurationError}
}

// ID returns the stable ChatGPT provider identity.
func (*Provider) ID() session.ProviderID { return session.ProviderChatGPT }

// List returns active conversations from the configured ChatGPT Project.
// ChatGPT remote star/pin fields never enter this representation; the
// controller overlays agentsctl's local pin store after provider listing.
func (p *Provider) List(ctx context.Context, archived bool) ([]session.Session, error) {
	if p.configErr != nil {
		return nil, p.configErr
	}
	if archived {
		return nil, nil
	}
	if p.browser == nil {
		return nil, fmt.Errorf("ChatGPT browser runtime is not configured")
	}
	conversations, err := p.browser.List(ctx, p.config.ProjectID)
	if err != nil {
		return nil, err
	}
	rows := make([]session.Session, 0, len(conversations))
	for _, item := range conversations {
		rows = append(rows, session.Session{
			Key:       session.Key{Provider: session.ProviderChatGPT, ID: item.ID},
			Name:      item.Title,
			CWD:       p.config.Root,
			CreatedAt: item.CreatedAt,
			UpdatedAt: item.UpdatedAt,
			Activity:  session.ActivityUnknown,
			Runtime:   session.RuntimeNone,
			Archived:  false,
			Actions: session.Actions{
				session.ActionOpen: {Available: true},
			},
		})
	}
	return rows, nil
}

// Open displays the official ChatGPT UI for s and returns when the browser
// view closes. Closing the view does not stop or mutate the cloud session.
func (p *Provider) Open(ctx context.Context, s session.Session, in *os.File, out io.Writer) error {
	if p.configErr != nil {
		return p.configErr
	}
	if s.Key.Provider != session.ProviderChatGPT {
		return fmt.Errorf("cannot open non-ChatGPT session %s", s.Key)
	}
	if p.browser == nil {
		return fmt.Errorf("ChatGPT browser runtime is not configured")
	}
	return p.browser.Open(ctx, s.Key.ID, in, out)
}

// Close stops the provider-owned discovery helper and removes its temporary
// bridge assets. It does not alter any ChatGPT conversation.
func (p *Provider) Close() error {
	if p.browser == nil {
		return nil
	}
	return p.browser.Close()
}
