package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	base "github.com/tingtt/agentsctl/internal/provider"
)

const maxDaemonErrorOutput = 4 << 10

// DaemonInfo identifies a shared app-server daemon that is ready for a
// connection. SocketPath is the endpoint resolved by the Codex CLI.
type DaemonInfo struct {
	Status           string `json:"status"`
	SocketPath       string `json:"socketPath"`
	AppServerVersion string `json:"appServerVersion"`
}

// DaemonLifecycle ensures that the shared app-server daemon is ready. It does
// not transfer ownership of the daemon process to the caller.
type DaemonLifecycle interface {
	Ensure(context.Context) (DaemonInfo, error)
}

// CommandDaemon ensures the shared daemon through the Codex CLI's idempotent
// app-server daemon start command. Concurrent calls share one in-flight
// command; a later call starts a new command and does not use a cached result.
type CommandDaemon struct {
	Path   string
	Runner base.Runner

	mu       sync.Mutex
	inFlight *daemonCall
}

type daemonCall struct {
	done chan struct{}
	info DaemonInfo
	err  error
}

// Ensure waits until Codex reports a ready shared daemon.
func (d *CommandDaemon) Ensure(ctx context.Context) (DaemonInfo, error) {
	d.mu.Lock()
	if call := d.inFlight; call != nil {
		d.mu.Unlock()
		select {
		case <-ctx.Done():
			return DaemonInfo{}, ctx.Err()
		case <-call.done:
			return call.info, call.err
		}
	}
	call := &daemonCall{done: make(chan struct{})}
	d.inFlight = call
	d.mu.Unlock()

	call.info, call.err = d.ensure(ctx)
	d.mu.Lock()
	d.inFlight = nil
	close(call.done)
	d.mu.Unlock()
	return call.info, call.err
}

func (d *CommandDaemon) ensure(ctx context.Context) (DaemonInfo, error) {
	path := d.Path
	if path == "" {
		path = "codex"
	}
	if d.Runner == nil {
		return DaemonInfo{}, errors.New("codex daemon runner is not configured")
	}
	result, err := d.Runner.Run(ctx, path, []string{"app-server", "daemon", "start"}, "")
	if err != nil {
		if stderr := boundedOutput(result.Stderr); stderr != "" {
			return DaemonInfo{}, fmt.Errorf("codex app-server daemon start: %w: %s", err, stderr)
		}
		return DaemonInfo{}, fmt.Errorf("codex app-server daemon start: %w", err)
	}
	info, err := decodeDaemonInfo(result.Stdout)
	if err != nil {
		return DaemonInfo{}, fmt.Errorf("codex app-server daemon start: %w", err)
	}
	return info, nil
}

func decodeDaemonInfo(stdout []byte) (DaemonInfo, error) {
	var info DaemonInfo
	dec := json.NewDecoder(bytes.NewReader(stdout))
	if err := dec.Decode(&info); err != nil {
		return DaemonInfo{}, fmt.Errorf("decode response: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return DaemonInfo{}, errors.New("decode response: multiple JSON values")
		}
		return DaemonInfo{}, fmt.Errorf("decode response trailing data: %w", err)
	}
	if info.Status != "started" && info.Status != "alreadyRunning" {
		return DaemonInfo{}, fmt.Errorf("unexpected status %q", info.Status)
	}
	if info.SocketPath == "" {
		return DaemonInfo{}, errors.New("response has empty socketPath")
	}
	return info, nil
}

func boundedOutput(stderr []byte) string {
	stderr = bytes.TrimSpace(stderr)
	if len(stderr) > maxDaemonErrorOutput {
		stderr = stderr[:maxDaemonErrorOutput]
		return strings.TrimSpace(string(stderr)) + "…"
	}
	return string(stderr)
}
