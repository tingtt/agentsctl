package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/tingtt/agentsctl/internal/localstate"
	base "github.com/tingtt/agentsctl/internal/provider"
	"github.com/tingtt/agentsctl/internal/provider/claude"
	"github.com/tingtt/agentsctl/internal/provider/codex"
	"github.com/tingtt/agentsctl/internal/session"
	"github.com/tingtt/agentsctl/internal/state"
	"github.com/tingtt/agentsctl/internal/supervisor"
	"github.com/tingtt/agentsctl/internal/tui"
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
		return (&supervisor.Server{Socket: *socket, Store: state.New(*statePath)}).Serve(ctx)
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
	store := state.New(statePath)
	// claudeStore is a second, transitional handle on the same state file:
	// provider/claude has migrated to localstate's domain operations while
	// provider/codex and the supervisor still read/write through the old
	// state.Store (both types take/release an flock per operation against
	// the same path, so the two handles interleave safely). This goes away
	// once the supervisor/codex side migrates too.
	claudeStore := localstate.New(statePath)
	runner := base.ExecRunner{}
	api := &codex.CommandAppServer{Path: "codex"}
	dispatch := supervisor.Dispatcher{Client: client}
	providers := []session.Provider{&claude.Provider{Path: "claude", Runner: runner, Store: claudeStore, Renamer: claude.NewNativeRenamer()}, &codex.Provider{Path: "codex", API: api, Runner: runner, Store: store, Runtime: dispatch}}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	app := tui.App{Catalog: session.Catalog{Providers: providers, Pins: store}, Model: tui.NewModel(), Input: os.Stdin, Output: os.Stdout, CWD: cwd, ClaudePath: "claude", Socket: socket}
	return app.Run(ctx)
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
