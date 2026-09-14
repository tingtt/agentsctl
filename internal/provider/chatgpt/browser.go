package chatgpt

import (
	"context"
	"crypto/rand"
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
)

const (
	defaultBrowserPath = "terminal-browser"
	defaultPartition   = "agentsctl-chatgpt"
	chatGPTOrigin      = "https://chatgpt.com"
	socketPlaceholder  = `"__AGENTSCTL_CHATGPT_SOCKET_PATH__"`
	ownerPlaceholder   = `"__AGENTSCTL_CHATGPT_OWNER_TOKEN__"`
)

type browser interface {
	List(context.Context, string) ([]conversation, error)
	Open(context.Context, string, *os.File, io.Writer) error
	Close() error
}

type processHandle interface {
	Done() <-chan struct{}
	Err() error
	Stop() error
}

type browserExecutor interface {
	StartBackground(context.Context, string, []string) (processHandle, error)
	RunForeground(context.Context, string, []string, *os.File, io.Writer) error
}

type runtime struct {
	mu sync.Mutex

	path      string
	partition string
	executor  browserExecutor
	dial      func(context.Context, string) (net.Conn, error)

	assetDir    string
	mainPath    string
	preloadPath string
	socketPath  string
	ownerToken  string
	helper      processHandle
	bridge      discoveryBridge
}

func newRuntime() *runtime {
	return &runtime{
		path:      defaultBrowserPath,
		partition: defaultPartition,
		executor:  commandExecutor{},
		dial:      dialBridge,
	}
}

func (r *runtime) List(ctx context.Context, projectID string) ([]conversation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.ensureDiscoveryLocked(ctx); err != nil {
		return nil, err
	}
	return enumerate(ctx, r.bridge, projectID)
}

func (r *runtime) Open(ctx context.Context, conversationID string, in *os.File, out io.Writer) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !conversationIDPattern.MatchString(conversationID) {
		return fmt.Errorf("invalid ChatGPT conversation ID")
	}
	if err := r.ensureDiscoveryLocked(ctx); err != nil {
		return err
	}
	url := chatGPTOrigin + "/c/" + conversationID
	args := []string{
		"open", url,
		"--app-mode",
		"--app-name=ChatGPT",
		"--app-id=agentsctl-chatgpt",
		"--no-merge",
		"--partition=" + r.partition,
		"--preload=" + r.preloadPath,
	}
	if err := r.executor.RunForeground(ctx, r.path, args, in, out); err != nil {
		return fmt.Errorf("open ChatGPT browser view: %w", err)
	}
	return nil
}

func (r *runtime) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	var errs []error
	if r.bridge != nil {
		errs = append(errs, r.bridge.Close())
		r.bridge = nil
	}
	if r.helper != nil {
		errs = append(errs, r.helper.Stop())
		r.helper = nil
	}
	if r.assetDir != "" {
		errs = append(errs, os.RemoveAll(r.assetDir))
		r.assetDir = ""
	}
	return errors.Join(errs...)
}

func (r *runtime) ensureDiscoveryLocked(ctx context.Context) error {
	if r.helper != nil && r.bridge != nil {
		select {
		case <-r.helper.Done():
			_ = r.bridge.Close()
			_ = r.helper.Stop()
			r.bridge, r.helper = nil, nil
		default:
			return nil
		}
	}
	if err := r.materializeAssetsLocked(); err != nil {
		return err
	}
	args := []string{
		"open", chatGPTOrigin + "/#agentsctl-discovery=" + r.ownerToken,
		"--no-merge",
		"--partition=" + r.partition,
		"--main-script=" + r.mainPath,
		"--preload=" + r.preloadPath,
	}
	helper, err := r.executor.StartBackground(ctx, r.path, args)
	if err != nil {
		return fmt.Errorf("start ChatGPT discovery helper: %w", err)
	}
	conn, err := r.dial(ctx, r.socketPath)
	if err != nil {
		_ = helper.Stop()
		return err
	}
	bridge := newBridgeClient(conn)
	if err := bridge.Ping(ctx); err != nil {
		_ = bridge.Close()
		_ = helper.Stop()
		return err
	}
	r.helper, r.bridge = helper, bridge
	return nil
}

func (r *runtime) materializeAssetsLocked() error {
	if r.assetDir != "" {
		return nil
	}
	dir, err := os.MkdirTemp("", "agentsctl-chatgpt-")
	if err != nil {
		return fmt.Errorf("create ChatGPT runtime directory: %w", err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(dir)
		}
	}()
	socketPath := filepath.Join(dir, "bridge.sock")
	quotedSocket, err := json.Marshal(socketPath)
	if err != nil {
		return err
	}
	ownerToken, err := newOwnerToken()
	if err != nil {
		return fmt.Errorf("create ChatGPT discovery ownership token: %w", err)
	}
	quotedOwner, err := json.Marshal(ownerToken)
	if err != nil {
		return err
	}
	if strings.Count(mainScriptTemplate, socketPlaceholder) != 1 {
		return fmt.Errorf("embedded ChatGPT bridge has an invalid socket placeholder")
	}
	if strings.Count(mainScriptTemplate, ownerPlaceholder) != 1 || strings.Count(preloadScriptTemplate, ownerPlaceholder) != 1 {
		return fmt.Errorf("embedded ChatGPT bridge has an invalid ownership placeholder")
	}
	mainScript := strings.Replace(mainScriptTemplate, socketPlaceholder, string(quotedSocket), 1)
	mainScript = strings.Replace(mainScript, ownerPlaceholder, string(quotedOwner), 1)
	preloadScript := strings.Replace(preloadScriptTemplate, ownerPlaceholder, string(quotedOwner), 1)
	mainPath := filepath.Join(dir, "main.js")
	preloadPath := filepath.Join(dir, "preload.js")
	ownershipPath := filepath.Join(dir, "ownership.js")
	capturePath := filepath.Join(dir, "capture.js")
	if err := os.WriteFile(mainPath, []byte(mainScript), 0o600); err != nil {
		return fmt.Errorf("write ChatGPT main script: %w", err)
	}
	if err := os.WriteFile(preloadPath, []byte(preloadScript), 0o600); err != nil {
		return fmt.Errorf("write ChatGPT preload: %w", err)
	}
	if err := os.WriteFile(ownershipPath, ownershipScript, 0o600); err != nil {
		return fmt.Errorf("write ChatGPT ownership helper: %w", err)
	}
	if err := os.WriteFile(capturePath, captureScript, 0o600); err != nil {
		return fmt.Errorf("write ChatGPT capture helper: %w", err)
	}
	r.assetDir, r.mainPath, r.preloadPath, r.socketPath, r.ownerToken = dir, mainPath, preloadPath, socketPath, ownerToken
	cleanup = false
	return nil
}

func newOwnerToken() (string, error) {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", token), nil
}

func dialBridge(ctx context.Context, path string) (net.Conn, error) {
	deadline := time.Now().Add(20 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		conn, err := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "unix", path)
		if err == nil {
			return conn, nil
		}
		lastErr = err
		if err := waitContext(ctx, 100*time.Millisecond); err != nil {
			return nil, err
		}
	}
	return nil, fmt.Errorf("ChatGPT browser bridge unavailable: %w", lastErr)
}

type commandExecutor struct{}

func (commandExecutor) StartBackground(ctx context.Context, path string, args []string) (processHandle, error) {
	cmd := gracefulCommand(ctx, path, args...)
	cmd.Env = append(os.Environ(), "TERMINAL_BROWSER_SKIP_GRAPHICS_CHECK=1")
	ptmx, err := pty.Start(cmd)
	if err != nil {
		return nil, err
	}
	process := &commandProcess{cmd: cmd, ptmx: ptmx, done: make(chan struct{})}
	go func() { _, _ = io.Copy(io.Discard, ptmx) }()
	go func() {
		err := cmd.Wait()
		_ = ptmx.Close()
		process.mu.Lock()
		process.waitErr = err
		process.mu.Unlock()
		close(process.done)
	}()
	return process, nil
}

func (commandExecutor) RunForeground(ctx context.Context, path string, args []string, in *os.File, out io.Writer) error {
	cmd := gracefulCommand(ctx, path, args...)
	cmd.Stdin = in
	cmd.Stdout = out
	cmd.Stderr = out
	return cmd.Run()
}

func gracefulCommand(ctx context.Context, path string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		err := cmd.Process.Signal(syscall.SIGTERM)
		if errors.Is(err, os.ErrProcessDone) {
			return nil
		}
		return err
	}
	cmd.WaitDelay = 5 * time.Second
	return cmd
}

type commandProcess struct {
	cmd  *exec.Cmd
	ptmx *os.File
	done chan struct{}

	mu      sync.Mutex
	waitErr error
}

func (p *commandProcess) Done() <-chan struct{} { return p.done }

func (p *commandProcess) Err() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.waitErr
}

func (p *commandProcess) Stop() error {
	select {
	case <-p.done:
		return p.Err()
	default:
	}
	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	select {
	case <-p.done:
		return nil
	case <-time.After(5 * time.Second):
	}
	if err := p.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	<-p.done
	return nil
}
