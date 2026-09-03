//go:build darwin || linux

package supervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	processinfo "github.com/tingtt/agentsctl/internal/process"
	"github.com/tingtt/agentsctl/internal/protocol"
	"github.com/tingtt/agentsctl/internal/state"
	"golang.org/x/sys/unix"
)

type Request struct {
	Action    string   `json:"action"`
	RunID     string   `json:"runId,omitempty"`
	SessionID string   `json:"sessionId,omitempty"`
	Args      []string `json:"args,omitempty"`
	CWD       string   `json:"cwd,omitempty"`
	Provider  string   `json:"provider,omitempty"`
	Baseline  []string `json:"baseline,omitempty"`
}
type Response struct {
	OK               bool       `json:"ok"`
	Error            string     `json:"error,omitempty"`
	Run              *state.Run `json:"run,omitempty"`
	ProtocolVersion  int        `json:"protocolVersion,omitempty"`
	BuildVersion     string     `json:"buildVersion,omitempty"`
	DaemonPID        int        `json:"daemonPid,omitempty"`
	DaemonStartTime  uint64     `json:"daemonStartTime,omitempty"`
	DaemonUID        uint32     `json:"daemonUid,omitempty"`
	DaemonExecutable string     `json:"daemonExecutable,omitempty"`
	DaemonParentPID  int        `json:"daemonParentPid,omitempty"`
	DaemonCWD        string     `json:"daemonCwd,omitempty"`
	DaemonPATH       string     `json:"daemonPath,omitempty"`
	ResolvedPath     string     `json:"resolvedPath,omitempty"`
	Output           string     `json:"output,omitempty"`
}

const ProtocolVersion = 1
const BuildVersion = "mvp-2026-09-03"

type process struct {
	run         state.Run
	cmd         *exec.Cmd
	ptmx        *os.File
	mu          sync.Mutex
	subscribers map[chan []byte]struct{}
	done        chan struct{}
}
type Server struct {
	Socket            string
	Store             *state.Store
	ResolveExecutable func(string) (string, error)
	mu                sync.RWMutex
	runs              map[string]*process
}

func (s *Server) Serve(ctx context.Context) error {
	if s.runs == nil {
		s.runs = map[string]*process{}
	}
	if err := os.MkdirAll(filepath.Dir(s.Socket), 0o700); err != nil {
		return err
	}
	lock, err := os.OpenFile(s.Socket+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return errors.New("supervisor is already running")
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	if info, statErr := os.Lstat(s.Socket); statErr == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return errors.New("refusing to replace non-socket supervisor path")
		}
		if err := os.Remove(s.Socket); err != nil {
			return err
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}
	ln, err := net.Listen("unix", s.Socket)
	if err != nil {
		return err
	}
	defer ln.Close()
	if err := os.Chmod(s.Socket, 0o600); err != nil {
		return err
	}
	if err := s.markStale(); err != nil {
		return err
	}
	go func() { <-ctx.Done(); ln.Close() }()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			continue
		}
		go s.handle(c)
	}
}

func (s *Server) markStale() error {
	return s.Store.Update(func(d *state.Data) error {
		for id, r := range d.Runs {
			if r.State == "running" || r.State == "starting" {
				r.State = "stale"
				r.Error = "supervisor restarted; PTY cannot be recovered"
				d.Runs[id] = r
			}
		}
		return nil
	})
}

func (s *Server) handle(c net.Conn) {
	defer c.Close()
	kind, b, err := protocol.Read(c)
	if err != nil || kind != protocol.Request {
		return
	}
	var req Request
	if json.Unmarshal(b, &req) != nil {
		return
	}
	switch req.Action {
	case "start":
		s.start(c, req)
	case "attach":
		s.attach(c, req.RunID)
	case "stop":
		s.stop(c, req.RunID)
	case "ping":
		respond(c, s.handshake())
	case "preflight":
		s.preflight(c, req)
	default:
		respond(c, Response{Error: "unknown action"})
	}
}

func (s *Server) handshake() Response {
	executable, _ := os.Executable()
	cwd, _ := os.Getwd()
	identity, _ := processinfo.Observe(os.Getpid())
	return Response{
		OK: true, ProtocolVersion: ProtocolVersion, BuildVersion: BuildVersion,
		DaemonPID: os.Getpid(), DaemonStartTime: identity.StartTime, DaemonUID: identity.UID,
		DaemonExecutable: executable, DaemonParentPID: os.Getppid(), DaemonCWD: cwd, DaemonPATH: os.Getenv("PATH"),
	}
}

func (s *Server) resolve(provider string) (string, error) {
	name := provider
	if provider == "codex" {
		name = "codex"
	}
	if name == "" {
		return "", errors.New("provider executable is empty")
	}
	resolve := s.ResolveExecutable
	if resolve == nil {
		resolve = exec.LookPath
	}
	path, err := resolve(name)
	if err != nil {
		return "", fmt.Errorf("executable lookup for %s: %w", provider, err)
	}
	return path, nil
}

func (s *Server) preflight(c net.Conn, req Request) {
	path, err := s.resolve(req.Provider)
	if err != nil {
		respond(c, Response{Error: err.Error()})
		return
	}
	cmd := exec.Command(path, req.Args...)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Run(); err != nil {
		respond(c, Response{Error: fmt.Sprintf("direct exec %s: %v: %s", req.Provider, err, strings.TrimSpace(output.String())), ResolvedPath: path})
		return
	}
	respond(c, Response{OK: true, ResolvedPath: path, Output: output.String()})
}

func (s *Server) start(c net.Conn, req Request) {
	r := state.Run{ID: req.RunID, Provider: req.Provider, SessionID: req.SessionID, CWD: req.CWD, State: "starting", StartedAt: time.Now(), Baseline: append([]string(nil), req.Baseline...)}
	if err := s.Store.Update(func(d *state.Data) error {
		if _, ok := d.Runs[r.ID]; ok {
			return errors.New("run already exists")
		}
		d.Runs[r.ID] = r
		return nil
	}); err != nil {
		respond(c, Response{Error: err.Error()})
		return
	}
	path, err := s.resolve(req.Provider)
	if err != nil {
		_ = s.deleteRun(r.ID)
		respond(c, Response{Error: err.Error()})
		return
	}
	cmd := exec.Command(path, req.Args...)
	cmd.Dir = req.CWD
	cmd.Env = environmentWith(os.Environ(), "CODEX_TUI_DISABLE_KEYBOARD_ENHANCEMENT", "1")
	ptmx, tty, err := pty.Open()
	if err != nil {
		_ = s.deleteRun(r.ID)
		respond(c, Response{Error: "PTY setup: " + err.Error()})
		return
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = tty, tty, tty
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	if err := cmd.Start(); err != nil {
		_ = tty.Close()
		_ = ptmx.Close()
		_ = s.deleteRun(r.ID)
		respond(c, Response{Error: "PTY process spawn: " + err.Error(), ResolvedPath: path})
		return
	}
	_ = tty.Close()
	r.State = "running"
	r.PID = cmd.Process.Pid
	if identity, observeErr := processinfo.Observe(r.PID); observeErr == nil {
		r.StartTime, r.UID = identity.StartTime, identity.UID
	} else {
		r.Error = "process identity unavailable: " + observeErr.Error()
	}
	p := &process{run: r, cmd: cmd, ptmx: ptmx, subscribers: map[chan []byte]struct{}{}, done: make(chan struct{})}
	s.mu.Lock()
	s.runs[r.ID] = p
	s.mu.Unlock()
	_ = s.saveRun(r)
	go s.drain(p)
	respond(c, Response{OK: true, Run: &r})
}
func (s *Server) drain(p *process) {
	runID := p.run.ID
	buf := make([]byte, 32<<10)
	for {
		n, err := p.ptmx.Read(buf)
		if n > 0 {
			chunk := append([]byte(nil), buf[:n]...)
			p.mu.Lock()
			for ch := range p.subscribers {
				select {
				case ch <- chunk:
				default:
				}
			}
			p.mu.Unlock()
		}
		if err != nil {
			break
		}
	}
	_ = p.cmd.Wait()
	_ = p.ptmx.Close()
	p.mu.Lock()
	close(p.done)
	for ch := range p.subscribers {
		close(ch)
	}
	p.subscribers = map[chan []byte]struct{}{}
	p.mu.Unlock()
	var stopped state.Run
	_ = s.Store.Update(func(d *state.Data) error {
		run := d.Runs[runID]
		run.State = "stopped"
		run.PID = 0
		d.Runs[runID] = run
		stopped = run
		return nil
	})
	p.mu.Lock()
	p.run = stopped
	p.mu.Unlock()
	s.mu.Lock()
	delete(s.runs, runID)
	s.mu.Unlock()
}
func (s *Server) attach(c net.Conn, id string) {
	s.mu.RLock()
	p := s.runs[id]
	s.mu.RUnlock()
	if p == nil {
		respond(c, Response{Error: "managed run is not live"})
		return
	}
	p.mu.Lock()
	run := p.run
	p.mu.Unlock()
	respond(c, Response{OK: true, Run: &run})
	out := make(chan []byte, 64)
	p.mu.Lock()
	p.subscribers[out] = struct{}{}
	p.mu.Unlock()
	defer func() { p.mu.Lock(); delete(p.subscribers, out); p.mu.Unlock() }()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for b := range out {
			if protocol.Write(c, protocol.Output, b) != nil {
				return
			}
		}
	}()
	for {
		kind, b, err := protocol.Read(c)
		if err != nil {
			return
		}
		switch kind {
		case protocol.Input:
			_, _ = p.ptmx.Write(b)
		case protocol.Resize:
			var sz struct{ Rows, Cols uint16 }
			if json.Unmarshal(b, &sz) == nil {
				_ = pty.Setsize(p.ptmx, &pty.Winsize{Rows: sz.Rows, Cols: sz.Cols})
			}
		case protocol.Detach:
			return
		}
		select {
		case <-done:
			return
		default:
		}
	}
}
func (s *Server) stop(c net.Conn, id string) {
	s.mu.RLock()
	p := s.runs[id]
	s.mu.RUnlock()
	if p == nil {
		respond(c, Response{Error: "refusing to stop a run not owned by this supervisor"})
		return
	}
	p.mu.Lock()
	run := p.run
	p.mu.Unlock()
	if err := processinfo.Match(processinfo.Identity{PID: run.PID, StartTime: run.StartTime, UID: run.UID}); err != nil {
		respond(c, Response{Error: "refusing to signal changed process identity: " + err.Error()})
		return
	}
	if err := syscall.Kill(-p.cmd.Process.Pid, syscall.SIGTERM); err != nil {
		respond(c, Response{Error: err.Error()})
		return
	}
	select {
	case <-p.done:
		respond(c, Response{OK: true})
	case <-time.After(5 * time.Second):
		respond(c, Response{Error: "SIGTERM timed out; process was not force-killed"})
	}
}
func (s *Server) saveRun(r state.Run) error {
	return s.Store.Update(func(d *state.Data) error { d.Runs[r.ID] = r; return nil })
}
func (s *Server) deleteRun(id string) error {
	return s.Store.Update(func(d *state.Data) error { delete(d.Runs, id); return nil })
}
func respond(w io.Writer, r Response) {
	b, _ := json.Marshal(r)
	_ = protocol.Write(w, protocol.Response, b)
}

type Client struct {
	Socket     string
	DaemonPath string
	StatePath  string
}

func (c Client) Ensure(ctx context.Context) error {
	if conn, err := net.DialTimeout("unix", c.Socket, 100*time.Millisecond); err == nil {
		peer, peerErr := peerIdentity(conn)
		response, pingErr := call(conn, Request{Action: "ping"})
		_ = conn.Close()
		if pingErr == nil && response.ProtocolVersion == ProtocolVersion && response.BuildVersion == BuildVersion {
			return nil
		}
		if pingErr != nil {
			return fmt.Errorf("supervisor handshake: %w", pingErr)
		}
		var restartErr error
		if response.DaemonPID == 0 && response.DaemonStartTime == 0 && response.BuildVersion == "" {
			if peerErr != nil {
				restartErr = fmt.Errorf("legacy daemon peer identity: %w", peerErr)
			} else {
				restartErr = c.restartLegacyOwned(ctx, peer)
			}
		} else {
			restartErr = c.restartOwned(ctx, response)
		}
		if restartErr != nil {
			return fmt.Errorf("incompatible supervisor protocol/build: %w", restartErr)
		}
	}
	return c.start(ctx)
}

func (c Client) start(ctx context.Context) error {
	cmd := exec.Command(c.DaemonPath, "daemon", "--state", c.StatePath, "--socket", c.Socket)
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	_ = cmd.Process.Release()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if conn, err := net.DialTimeout("unix", c.Socket, 100*time.Millisecond); err == nil {
			_ = conn.Close()
			response, pingErr := c.Call(ctx, Request{Action: "ping"})
			if pingErr != nil {
				return fmt.Errorf("new supervisor handshake: %w", pingErr)
			}
			if response.ProtocolVersion != ProtocolVersion || response.BuildVersion != BuildVersion {
				return fmt.Errorf("new supervisor has incompatible protocol/build %d/%q", response.ProtocolVersion, response.BuildVersion)
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
	return errors.New("supervisor did not start")
}

func (c Client) restartOwned(ctx context.Context, response Response) error {
	if response.DaemonPID <= 0 || response.DaemonStartTime == 0 || response.DaemonUID != uint32(os.Getuid()) {
		return errors.New("existing daemon ownership is not verifiable")
	}
	if !sameExecutable(response.DaemonExecutable, c.DaemonPath) {
		return errors.New("existing daemon executable does not match this agentsctl binary")
	}
	if err := processinfo.Match(processinfo.Identity{PID: response.DaemonPID, StartTime: response.DaemonStartTime, UID: response.DaemonUID}); err != nil {
		return fmt.Errorf("existing daemon identity changed: %w", err)
	}
	if err := syscall.Kill(response.DaemonPID, syscall.SIGTERM); err != nil {
		return fmt.Errorf("stop owned daemon: %w", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := net.DialTimeout("unix", c.Socket, 50*time.Millisecond); err != nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
	return errors.New("owned daemon did not stop")
}

func (c Client) restartLegacyOwned(ctx context.Context, identity processinfo.Identity) error {
	if !identity.Valid() || identity.UID != uint32(os.Getuid()) {
		return errors.New("legacy daemon peer ownership is not verifiable")
	}
	if err := privateEndpoint(c.Socket, identity.UID); err != nil {
		return fmt.Errorf("legacy daemon endpoint: %w", err)
	}
	name, err := processinfo.Name(identity.PID)
	if err != nil {
		return fmt.Errorf("legacy daemon process name: %w", err)
	}
	if !processNameMatches(name, filepath.Base(c.DaemonPath)) {
		return fmt.Errorf("legacy daemon process %q does not match %q", name, filepath.Base(c.DaemonPath))
	}
	data, err := state.New(c.StatePath).Load()
	if err != nil {
		return fmt.Errorf("inspect legacy daemon state: %w", err)
	}
	for _, run := range data.Runs {
		if run.State == "running" || run.State == "starting" {
			return errors.New("legacy daemon still owns an active run; refusing automatic restart")
		}
	}
	if err := processinfo.Match(identity); err != nil {
		return fmt.Errorf("legacy daemon identity changed: %w", err)
	}
	if err := syscall.Kill(identity.PID, syscall.SIGTERM); err != nil {
		return fmt.Errorf("stop owned legacy daemon: %w", err)
	}
	return c.waitStopped(ctx, "owned legacy daemon")
}

func (c Client) waitStopped(ctx context.Context, label string) error {
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := net.DialTimeout("unix", c.Socket, 50*time.Millisecond); err != nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
	return fmt.Errorf("%s did not stop", label)
}

func privateEndpoint(socket string, uid uint32) error {
	directory, err := os.Stat(filepath.Dir(socket))
	if err != nil {
		return err
	}
	if !directory.IsDir() || directory.Mode().Perm()&0o077 != 0 || fileUID(directory) != uid {
		return errors.New("state directory is not private to the daemon user")
	}
	endpoint, err := os.Lstat(socket)
	if err != nil {
		return err
	}
	if endpoint.Mode()&os.ModeSocket == 0 || endpoint.Mode().Perm()&0o077 != 0 || fileUID(endpoint) != uid {
		return errors.New("socket is not private to the daemon user")
	}
	return nil
}

func fileUID(info os.FileInfo) uint32 {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return stat.Uid
	}
	return ^uint32(0)
}

func processNameMatches(actual, expected string) bool {
	if actual == expected {
		return true
	}
	// Linux and Darwin kernel process names are truncated to a small fixed field.
	return len(actual) >= 15 && strings.HasPrefix(expected, actual)
}

func sameExecutable(left, right string) bool {
	leftPath, leftErr := filepath.EvalSymlinks(left)
	rightPath, rightErr := filepath.EvalSymlinks(right)
	if leftErr == nil && rightErr == nil {
		leftInfo, leftStatErr := os.Stat(leftPath)
		rightInfo, rightStatErr := os.Stat(rightPath)
		if leftStatErr == nil && rightStatErr == nil {
			return os.SameFile(leftInfo, rightInfo)
		}
	}
	return filepath.Clean(left) == filepath.Clean(right)
}

func (c Client) Preflight(ctx context.Context, provider string, args ...string) (Response, error) {
	return c.Call(ctx, Request{Action: "preflight", Provider: provider, Args: append([]string(nil), args...)})
}
func (c Client) Call(ctx context.Context, req Request) (Response, error) {
	conn, err := net.Dial("unix", c.Socket)
	if err != nil {
		return Response{}, err
	}
	defer conn.Close()
	return call(conn, req)
}

func call(conn net.Conn, req Request) (Response, error) {
	b, _ := json.Marshal(req)
	if err := protocol.Write(conn, protocol.Request, b); err != nil {
		return Response{}, err
	}
	kind, b, err := protocol.Read(conn)
	if err != nil || kind != protocol.Response {
		return Response{}, fmt.Errorf("invalid supervisor response: %w", err)
	}
	var res Response
	if err := json.Unmarshal(b, &res); err != nil {
		return res, err
	}
	if !res.OK {
		return res, errors.New(res.Error)
	}
	return res, nil
}

func environmentWith(environment []string, key, value string) []string {
	prefix := key + "="
	result := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		if !strings.HasPrefix(entry, prefix) {
			result = append(result, entry)
		}
	}
	return append(result, prefix+value)
}
