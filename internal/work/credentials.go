package work

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
)

// CommandRunner runs a credential helper without a shell.
type CommandRunner interface {
	Output(context.Context, string, ...string) ([]byte, error)
}

// CredentialSource follows GitHub CLI precedence: GH_TOKEN, GITHUB_TOKEN,
// then gh auth token. Credentials are never persisted by this package.
type CredentialSource struct {
	LookupEnv func(string) (string, bool)
	Runner    CommandRunner
}

// Token resolves a GitHub credential and returns only redacted errors.
func (s CredentialSource) Token(ctx context.Context) (string, error) {
	lookup := s.LookupEnv
	if lookup == nil {
		lookup = os.LookupEnv
	}
	for _, name := range []string{"GH_TOKEN", "GITHUB_TOKEN"} {
		if value, ok := lookup(name); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value), nil
		}
	}
	runner := s.Runner
	if runner == nil {
		runner = execCommandRunner{}
	}
	output, err := runner.Output(ctx, "gh", "auth", "token")
	if err != nil || strings.TrimSpace(string(output)) == "" {
		return "", errors.New("GitHub credential unavailable")
	}
	return strings.TrimSpace(string(output)), nil
}

type execCommandRunner struct{}

func (execCommandRunner) Output(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}
