package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/tingtt/agentsctl/internal/agentview"
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

func TestRunAndRestartExecsOnlyAfterRunReturned(t *testing.T) {
	var steps []string
	run := func() (*agentview.Restart, error) {
		// Everything Run's deferred cleanup does -- terminal restore
		// included -- happens before this return.
		steps = append(steps, "run returned")
		return &agentview.Restart{Executable: "/go/bin/agentsctl"}, nil
	}
	var gotPath string
	var gotArgv, gotEnv []string
	execProcess := func(path string, argv, env []string) error {
		steps = append(steps, "exec")
		gotPath, gotArgv, gotEnv = path, argv, env
		return nil
	}
	args := []string{"/old/bin/agentsctl", "--flag", "value"}
	env := []string{"CODEX_EDITOR=nvim", "AGENTSCTL_STATE_DIR=/state", "PATH=/usr/bin"}
	if err := runAndRestart(run, execProcess, args, env); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(steps, []string{"run returned", "exec"}) {
		t.Fatalf("steps = %v", steps)
	}
	if gotPath != "/go/bin/agentsctl" || !reflect.DeepEqual(gotArgv, []string{"/go/bin/agentsctl", "--flag", "value"}) {
		t.Fatalf("exec path=%q argv=%q", gotPath, gotArgv)
	}
	if !reflect.DeepEqual(gotEnv, env) {
		t.Fatalf("env = %q, want the whole environment %q", gotEnv, env)
	}
}

func TestRunAndRestartDoesNotExecWithoutRestartRequest(t *testing.T) {
	execProcess := func(string, []string, []string) error {
		t.Fatal("exec must not run without a restart request")
		return nil
	}
	if err := runAndRestart(func() (*agentview.Restart, error) { return nil, nil }, execProcess, []string{"agentsctl"}, nil); err != nil {
		t.Fatal(err)
	}
}

func TestRunAndRestartDoesNotExecAfterRunError(t *testing.T) {
	want := errors.New("run failed")
	execProcess := func(string, []string, []string) error {
		t.Fatal("exec must not run after a failed Run")
		return nil
	}
	run := func() (*agentview.Restart, error) { return &agentview.Restart{Executable: "/x"}, want }
	if err := runAndRestart(run, execProcess, []string{"agentsctl"}, nil); !errors.Is(err, want) {
		t.Fatalf("err = %v", err)
	}
}

func TestRunAndRestartReportsExecFailure(t *testing.T) {
	execErr := errors.New("exec format error")
	run := func() (*agentview.Restart, error) { return &agentview.Restart{Executable: "/x"}, nil }
	execProcess := func(string, []string, []string) error { return execErr }
	if err := runAndRestart(run, execProcess, []string{"agentsctl"}, nil); !errors.Is(err, execErr) {
		t.Fatalf("err = %v", err)
	}
}
