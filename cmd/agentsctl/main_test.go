package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/tingtt/agentsctl/internal/localstate"
	"github.com/tingtt/agentsctl/internal/session"
	"github.com/tingtt/agentsctl/internal/sessionctl"
)

func TestAppendChatGPTProviderIsOptIn(t *testing.T) {
	base := []sessionctl.Source{mainFakeSource{}}
	store := localstate.New(filepath.Join(t.TempDir(), "state.json"))
	providers, provider := appendChatGPTProvider(t.TempDir(), base, store)
	if provider != nil || len(providers) != len(base) {
		t.Fatalf("provider=%v providers=%d", provider, len(providers))
	}
}

func TestAppendChatGPTProviderRegistersValidAndInvalidExplicitConfig(t *testing.T) {
	for _, test := range []struct {
		name        string
		contents    string
		wantWarning bool
	}{
		{name: "valid", contents: "[chatgpt]\nproject_id = \"g-p-project\"\n"},
		{name: "invalid", contents: "[chatgpt]\n", wantWarning: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, ".agentsctl.toml"), []byte(test.contents), 0o600); err != nil {
				t.Fatal(err)
			}
			store := localstate.New(filepath.Join(t.TempDir(), "state.json"))
			providers, provider := appendChatGPTProvider(root, nil, store)
			if provider == nil || len(providers) != 1 || providers[0].ID() != session.ProviderChatGPT {
				t.Fatalf("provider=%v providers=%v", provider, providers)
			}
			if test.wantWarning {
				snapshot := (sessionctl.Controller{Providers: providers}).Load(context.Background(), session.Scope{Directory: session.ScopeAll})
				if snapshot.Warnings[session.ProviderChatGPT] == nil {
					t.Fatalf("warnings=%v", snapshot.Warnings)
				}
			}
			_ = provider.Close()
		})
	}
}

type mainFakeSource struct{}

func (mainFakeSource) ID() session.ProviderID { return session.ProviderClaude }
func (mainFakeSource) List(context.Context, bool) ([]session.Session, error) {
	return nil, nil
}
