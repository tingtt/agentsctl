package codex

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tingtt/agentsctl/internal/localstate"
	base "github.com/tingtt/agentsctl/internal/provider"
)

type daemonRunner struct {
	mu     sync.Mutex
	result base.Result
	err    error
	run    func(context.Context) (base.Result, error)
	path   string
	args   []string
	cwd    string
	calls  int
}

func (r *daemonRunner) Run(ctx context.Context, path string, args []string, cwd string) (base.Result, error) {
	r.mu.Lock()
	r.path, r.args, r.cwd = path, append([]string(nil), args...), cwd
	r.calls++
	r.mu.Unlock()
	if r.run != nil {
		return r.run(ctx)
	}
	return r.result, r.err
}

func (r *daemonRunner) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func TestCommandDaemonAcceptsReadyResponses(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		wantPath string
		stdout   string
		stderr   string
		want     DaemonInfo
	}{
		{
			name:     "started",
			path:     "/opt/codex",
			wantPath: "/opt/codex",
			stdout:   `{"status":"started","socketPath":"/tmp/codex.sock","appServerVersion":"0.156.1"}`,
			want:     DaemonInfo{Status: "started", SocketPath: "/tmp/codex.sock", AppServerVersion: "0.156.1"},
		},
		{
			name:     "already running with installation warning and unknown fields",
			wantPath: "codex",
			stdout:   `{"status":"alreadyRunning","socketPath":"/tmp/codex.sock","future":true}`,
			stderr:   "installed daemon package\nwarning: first run",
			want:     DaemonInfo{Status: "alreadyRunning", SocketPath: "/tmp/codex.sock"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			runner := &daemonRunner{result: base.Result{Stdout: []byte(tc.stdout), Stderr: []byte(tc.stderr)}}
			daemon := &CommandDaemon{Path: tc.path, Runner: runner}
			got, err := daemon.Ensure(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("info=%+v, want %+v", got, tc.want)
			}
			if runner.path != tc.wantPath || !reflect.DeepEqual(runner.args, []string{"app-server", "daemon", "start"}) || runner.cwd != "" {
				t.Fatalf("command=%q %v cwd=%q", runner.path, runner.args, runner.cwd)
			}
		})
	}
}

func TestCommandDaemonRejectsFailedOrInvalidResponses(t *testing.T) {
	tests := []struct {
		name   string
		result base.Result
		err    error
		want   string
	}{
		{name: "command failure", result: base.Result{Stderr: []byte("daemon failed")}, err: errors.New("exit status 1"), want: "daemon failed"},
		{name: "malformed JSON", result: base.Result{Stdout: []byte(`{"status":`)}},
		{name: "missing socket", result: base.Result{Stdout: []byte(`{"status":"started","socketPath":""}`)}},
		{name: "unexpected status", result: base.Result{Stdout: []byte(`{"status":"stopped","socketPath":"/tmp/codex.sock"}`)}},
		{name: "multiple JSON values", result: base.Result{Stdout: []byte(`{"status":"started","socketPath":"/tmp/a"} {"status":"started","socketPath":"/tmp/b"}`)}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			daemon := &CommandDaemon{Runner: &daemonRunner{result: tc.result, err: tc.err}}
			_, err := daemon.Ensure(context.Background())
			if err == nil {
				t.Fatal("Ensure accepted an invalid daemon response")
			}
			if tc.want != "" && !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error=%q, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestListDoesNotEnsureDaemon(t *testing.T) {
	lifecycle := &scriptedLifecycle{results: []lifecycleResult{{err: errors.New("must not be called")}}}
	p := &Provider{API: &fakeAPI{}, Store: localstate.New(t.TempDir() + "/state.json"), Daemon: lifecycle}
	if _, err := p.List(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if lifecycle.calls != 0 {
		t.Fatalf("List called Ensure %d times", lifecycle.calls)
	}
}

func TestCommandDaemonConcurrentCallsKeepIndependentContextsAndAreNotCached(t *testing.T) {
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	result := base.Result{Stdout: []byte(`{"status":"started","socketPath":"/tmp/codex.sock"}`)}
	runner := &daemonRunner{
		run: func(ctx context.Context) (base.Result, error) {
			started <- struct{}{}
			select {
			case <-ctx.Done():
				return base.Result{}, ctx.Err()
			case <-release:
				return result, nil
			}
		},
	}
	daemon := &CommandDaemon{Runner: runner}

	ctxA, cancelA := context.WithCancel(context.Background())
	defer cancelA()
	resultA := make(chan error, 1)
	resultB := make(chan error, 1)
	go func() {
		_, err := daemon.Ensure(ctxA)
		resultA <- err
	}()
	go func() {
		_, err := daemon.Ensure(context.Background())
		resultB <- err
	}()
	for range 2 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("each concurrent Ensure must start its own command")
		}
	}

	cancelA()
	select {
	case err := <-resultA:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled caller error=%v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled caller did not return")
	}
	close(release)
	select {
	case err := <-resultB:
		if err != nil {
			t.Fatalf("unrelated caller inherited cancellation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("unrelated caller did not complete")
	}
	if got := runner.callCount(); got != 2 {
		t.Fatalf("concurrent command calls=%d, want 2", got)
	}

	if _, err := daemon.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := runner.callCount(); got != 3 {
		t.Fatalf("later Ensure command calls=%d, want 3", got)
	}
}
