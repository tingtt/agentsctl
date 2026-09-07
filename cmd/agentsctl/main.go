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
	"github.com/tingtt/agentsctl/internal/provider/claude"
	"github.com/tingtt/agentsctl/internal/provider/codex"
	"github.com/tingtt/agentsctl/internal/sessionctl"
	"github.com/tingtt/agentsctl/internal/supervisor"
	"github.com/tingtt/agentsctl/internal/workspace"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "agentsctl:", err)
		os.Exit(1)
	}
}
func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if len(os.Args) > 1 && os.Args[1] == "daemon" {
		fs := flag.NewFlagSet("daemon", flag.ContinueOnError)
		statePath := fs.String("state", "", "state path")
		socket := fs.String("socket", "", "socket path")
		if err := fs.Parse(os.Args[2:]); err != nil {
			return err
		}
		return (&supervisor.Server{Socket: *socket, Store: localstate.New(*statePath)}).Serve(ctx)
	}
	// claude.UsageCollectorCommand is the Claude usage probe's own
	// statusLine command target (see claude.NewProbe/writeUsageSettings):
	// this same executable, re-invoked as a hidden subcommand, the same
	// pattern "daemon" above uses for the Codex supervisor's own re-exec.
	if len(os.Args) > 1 && os.Args[1] == claude.UsageCollectorCommand {
		return claude.RunUsageCollector(os.Args[2:], os.Stdin)
	}
	dir, err := configDir()
	if err != nil {
		return err
	}
	statePath := filepath.Join(dir, "state.json")
	socket := filepath.Join(dir, "supervisor.sock")
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	client := supervisor.Client{Socket: socket, DaemonPath: exe, StatePath: statePath}
	if err := client.Ensure(ctx); err != nil {
		return err
	}
	store := localstate.New(statePath)
	runner := base.ExecRunner{}
	api := &codex.CommandAppServer{Path: "codex"}
	dispatch := supervisor.Dispatcher{Client: client}
	usageProbe := claude.NewProbe("claude", filepath.Join(dir, "claude-usage"))
	controller := sessionctl.Controller{
		Providers: []sessionctl.Source{
			&claude.Provider{Path: "claude", Runner: runner, Store: store, Renamer: claude.NewNativeRenamer(), UsageProbe: usageProbe},
			&codex.Provider{Path: "codex", API: api, Runner: runner, Store: store, Runtime: dispatch},
		},
		Pins: store,
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	rt := agentview.Runtime{Controller: controller, State: agentview.NewState(), CWD: cwd, Worktrees: workspace.Worktrees}
	return rt.Run(ctx)
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
