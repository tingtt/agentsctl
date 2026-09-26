package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/tingtt/agentsctl/internal/agentview"
	"github.com/tingtt/agentsctl/internal/localstate"
	base "github.com/tingtt/agentsctl/internal/provider"
	"github.com/tingtt/agentsctl/internal/provider/chatgpt"
	"github.com/tingtt/agentsctl/internal/provider/claude"
	"github.com/tingtt/agentsctl/internal/provider/codex"
	"github.com/tingtt/agentsctl/internal/selfupdate"
	"github.com/tingtt/agentsctl/internal/sessionctl"
	"github.com/tingtt/agentsctl/internal/supervisor"
	"github.com/tingtt/agentsctl/internal/terminal"
	"github.com/tingtt/agentsctl/internal/version"
	"github.com/tingtt/agentsctl/internal/workspace"
)

func main() {
	if err := runAndRestart(run, syscall.Exec, os.Args, os.Environ()); err != nil {
		fmt.Fprintln(os.Stderr, "agentsctl:", err)
		os.Exit(1)
	}
}

// execFunc replaces the current process image (syscall.Exec).
type execFunc func(path string, argv, env []string) error

// runAndRestart runs the application and, when it ended because an update was
// installed, replaces the process with the new executable. The replacement
// happens only after run has returned: Agent View has by then restored the
// terminal, and run's own deferred cleanup (provider close, signal handling)
// has executed -- none of which would happen across an exec. The working
// directory is inherited by exec; arguments and the whole environment
// (CODEX_EDITOR, AGENTSCTL_STATE_DIR, ...) are carried over from args/env.
func runAndRestart(run func() (*agentview.Restart, error), execProcess execFunc, args, env []string) error {
	restart, err := run()
	if err != nil {
		return err
	}
	if restart == nil {
		return nil
	}
	argv := []string{restart.Executable}
	if len(args) > 1 {
		argv = append(argv, args[1:]...)
	}
	if err := execProcess(restart.Executable, argv, env); err != nil {
		return fmt.Errorf("restart %s: %w", restart.Executable, err)
	}
	return nil
}

func run() (*agentview.Restart, error) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if len(os.Args) > 1 && os.Args[1] == "daemon" {
		fs := flag.NewFlagSet("daemon", flag.ContinueOnError)
		statePath := fs.String("state", "", "state path")
		socket := fs.String("socket", "", "socket path")
		if err := fs.Parse(os.Args[2:]); err != nil {
			return nil, err
		}
		return nil, (&supervisor.Server{Socket: *socket, Store: localstate.New(*statePath)}).Serve(ctx)
	}
	// claude.UsageCollectorCommand is the Claude usage probe's own
	// statusLine command target (see claude.NewProbe/writeUsageSettings):
	// this same executable, re-invoked as a hidden subcommand, the same
	// pattern "daemon" above uses for the Codex supervisor's own re-exec.
	if len(os.Args) > 1 && os.Args[1] == claude.UsageCollectorCommand {
		return nil, claude.RunUsageCollector(os.Args[2:], os.Stdin)
	}
	dir, err := configDir()
	if err != nil {
		return nil, err
	}
	statePath := filepath.Join(dir, "state.json")
	socket := filepath.Join(dir, "supervisor.sock")
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	client := supervisor.Client{Socket: socket, DaemonPath: exe, StatePath: statePath}
	if err := client.Ensure(ctx); err != nil {
		return nil, err
	}
	store := localstate.New(statePath)
	runner := base.ExecRunner{}
	api := &codex.CommandAppServer{Path: "codex"}
	daemon := &codex.CommandDaemon{Path: "codex", Runner: runner}
	dispatch := supervisor.Dispatcher{Client: client}
	usageProbe := claude.NewProbe("claude", filepath.Join(dir, "claude-usage"))
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	providers := []sessionctl.Source{
		&claude.Provider{Path: "claude", Runner: runner, Store: store, Renamer: claude.NewNativeRenamer(), UsageProbe: usageProbe},
		&codex.Provider{Path: "codex", API: api, Runner: runner, Store: store, Runtime: dispatch, Daemon: daemon, Foreground: terminal.ForegroundPTY{}},
	}
	providers, chatGPTProvider := appendChatGPTProvider(cwd, providers, store)
	if chatGPTProvider != nil {
		defer func() { _ = chatGPTProvider.Close() }()
	}
	controller := sessionctl.Controller{
		Providers: providers,
		Pins:      store,
	}
	rt := agentview.Runtime{Controller: controller, State: agentview.NewState(), CWD: cwd, Worktrees: workspace.Worktrees}
	// A development build (no injected version) gets no Updater, so it
	// neither checks for updates nor offers /update.
	if updater := selfupdate.New(version.Version); updater != nil {
		rt.Updater = updater
	}
	if err := rt.Run(ctx); err != nil {
		return nil, err
	}
	if restart, ok := rt.Restart(); ok {
		return &restart, nil
	}
	return nil, nil
}

func appendChatGPTProvider(cwd string, providers []sessionctl.Source, store *localstate.Store) ([]sessionctl.Source, *chatgpt.Provider) {
	config, configured, err := chatgpt.Discover(cwd)
	if !configured {
		return providers, nil
	}
	var provider *chatgpt.Provider
	if err != nil {
		provider = chatgpt.NewUnavailable(err)
	} else {
		provider = chatgpt.New(config, store)
	}
	return append(providers, provider), provider
}

func configDir() (string, error) {
	if v := os.Getenv("AGENTSCTL_STATE_DIR"); v != "" {
		return v, nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "agentsctl"), nil
}
