//go:build darwin || linux

package agentview

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/tingtt/agentsctl/internal/session"
	"github.com/tingtt/agentsctl/internal/sessionctl"
)

var errOverviewTerminalOwnership = errors.New("agent view terminal ownership could not be restored")

type promptEditorRunner func(context.Context, string, *os.File, io.Writer) error

func runVim(ctx context.Context, path string, input *os.File, output io.Writer) error {
	cmd := exec.CommandContext(ctx, "vim", path)
	cmd.Stdin = input
	cmd.Stdout = output
	cmd.Stderr = output
	return cmd.Run()
}

func encodePromptFile(prompt string) string {
	return prompt + "\n"
}

func decodePromptFile(content string) string {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	content = strings.ReplaceAll(content, "\r", "\n")
	return strings.TrimSuffix(content, "\n")
}

// withForegroundTerminal runs run as the terminal's foreground owner: the
// overview is suspended first (releasing every overview-owned terminal mode)
// and resumed afterwards, whether or not run failed. If the overview cannot be
// suspended, run never starts; if it cannot be resumed, the overview's
// ownership of the terminal is no longer trustworthy and the error wraps
// errOverviewTerminalOwnership -- joined with run's own error, so neither is
// lost. The caller decides what run does; this knows nothing about providers
// or editors.
func (r *Runtime) withForegroundTerminal(run func() error) error {
	if r.terminal == nil {
		return fmt.Errorf("%w: terminal lifecycle is unavailable", errOverviewTerminalOwnership)
	}
	if suspendErr := r.terminal.suspend(); suspendErr != nil {
		return fmt.Errorf("%w: suspend overview: %w", errOverviewTerminalOwnership, suspendErr)
	}
	runErr := run()
	if resumeErr := r.terminal.resume(); resumeErr != nil {
		return errors.Join(
			fmt.Errorf("%w: resume overview: %w", errOverviewTerminalOwnership, resumeErr),
			runErr,
		)
	}
	return runErr
}

// openForeground hands the terminal to the provider's Open (through the
// overview suspend/resume handoff) and returns Open's result. The provider is
// opaque here: this is the same handoff for every provider.
func (r *Runtime) openForeground(ctx context.Context, row session.Session) (result sessionctl.Result, err error) {
	err = r.withForegroundTerminal(func() error {
		var openErr error
		result, openErr = r.Controller.Open(ctx, row, r.Input, r.Output)
		return openErr
	})
	return result, err
}

// editPrompt snapshots the composer, lets Vim edit only that prompt through a
// private temporary file, and applies saved content after terminal ownership
// has been restored. Any failure before the final read leaves the snapshot in
// place.
func (r *Runtime) editPrompt(ctx context.Context) (err error) {
	original := r.State.Composer.Prompt
	file, err := os.CreateTemp("", "agentsctl-prompt-*.txt")
	if err != nil {
		return fmt.Errorf("create temporary prompt file: %w", err)
	}
	path := file.Name()
	defer func() {
		if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			err = errors.Join(err, fmt.Errorf("remove temporary prompt file: %w", removeErr))
		}
	}()

	if _, writeErr := io.WriteString(file, encodePromptFile(original)); writeErr != nil {
		return errors.Join(fmt.Errorf("write temporary prompt file: %w", writeErr), file.Close())
	}
	if closeErr := file.Close(); closeErr != nil {
		return fmt.Errorf("close temporary prompt file: %w", closeErr)
	}
	runner := r.runPromptEditor
	if runner == nil {
		runner = runVim
	}
	if err := r.withForegroundTerminal(func() error {
		if editorErr := runner(ctx, path, r.Input, r.Output); editorErr != nil {
			return fmt.Errorf("vim: %w", editorErr)
		}
		return nil
	}); err != nil {
		return err
	}

	content, readErr := os.ReadFile(path)
	if readErr != nil {
		return fmt.Errorf("read temporary prompt file: %w", readErr)
	}
	updated := decodePromptFile(string(content))
	if updated != original {
		r.State.Composer.ReplacePrompt(updated)
	}
	return nil
}
