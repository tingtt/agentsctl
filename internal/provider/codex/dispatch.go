package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/tingtt/agentsctl/internal/session"
)

// dispatchCleanupTimeout bounds each step Dispatch takes after its commit
// point (see Dispatch). Those steps no longer decide whether the session
// exists, so an unresponsive daemon must not keep an already-started
// Dispatch from returning. A variable only so tests can shorten it.
var dispatchCleanupTimeout = 10 * time.Second

// Dispatch starts a new Codex thread on the shared app-server daemon and
// submits prompt as its first turn, over a connection of its own:
//
//	initialize -> thread/start -> turn/start -> thread/unsubscribe -> close
//
// A rename-only input also sets the name, between turn/start and
// thread/unsubscribe, once the turn's turn/started arrived.
//
// thread/start returns the canonical thread ID; a successful turn/start
// response is the commit point, after which the turn may already be
// running. Any failure before it is a Dispatch failure. Everything after it
// is cleanup: thread/start subscribed this connection to the thread, and
// thread/unsubscribe releases that subscription best effort, with closing
// the connection as the fallback that always runs. A cleanup failure never
// turns the committed Dispatch into an error a retry would duplicate.
//
// The returned session carries only the canonical key and the requested
// CWD. Dispatch creates no local run, row or provisional identity: the
// native catalog (List and the Observer) publishes the thread. Server
// requests (approval, user input) that reach this connection before it is
// unsubscribed stay unanswered (see rpcConn); they remain pending on the
// thread for a later foreground client.
//
// A composer input that is only `/rename <name>` is never forwarded to
// Codex: as an initial prompt it would reach the model as plain text. The
// model gets the fixed renameBootstrapPrompt instead, and the name is set
// natively on the same connection once that bootstrap turn has started --
// its turn/started, not the accepted response, says the thread's rollout
// exists (see turnStarts) -- and before the connection unsubscribes (see
// setThreadName). An ordinary Dispatch waits for no turn/started. A rename failure
// after the commit point fails Dispatch with the thread's ID, and leaves
// the thread and its turn alone.
func (p *Provider) Dispatch(ctx context.Context, prompt, cwd string) (session.Session, error) {
	var rename string
	if name, isRename := parseRenameOnly(prompt); isRename {
		if name == "" {
			return session.Session{}, errors.New("name must not be empty")
		}
		prompt, rename = renameBootstrapPrompt, name
	}
	socket, err := p.readySocket(ctx)
	if err != nil {
		return session.Session{}, err
	}
	// Recorded from the moment the connection exists: a turn/started
	// notification may arrive before the turn/start response it matches.
	started := newTurnStarts()
	var threadID string
	err = connectDaemon(ctx, socket, started.observe, func(conn *rpcConn) error {
		var err error
		if threadID, err = startThread(ctx, conn, cwd); err != nil {
			return fmt.Errorf("codex thread/start: %w", err)
		}
		turnID, err := startTurn(ctx, conn, threadID, prompt)
		if err != nil {
			return fmt.Errorf("codex turn/start on thread %s: %w", threadID, err)
		}

		// Committed: the turn has started. What follows must neither undo
		// it nor report it as failed, and the caller giving up no longer
		// matters. The rename runs while the connection is still
		// subscribed; unsubscribing is attempted whatever it did.
		var renameErr error
		if rename != "" {
			renameErr = cleanupStep(ctx, func(ctx context.Context) error {
				// The accepted response does not mean the thread's rollout
				// is persisted yet, and the rename needs it. The daemon
				// sends turn/started only after persisting the turn start.
				if err := started.wait(ctx, conn, threadID, turnID); err != nil {
					return fmt.Errorf("wait for turn/started: %w", err)
				}
				return setThreadName(ctx, conn, threadID, rename)
			})
		}
		_ = cleanupStep(ctx, func(ctx context.Context) error { return unsubscribeThread(ctx, conn, threadID) })
		if renameErr != nil {
			return fmt.Errorf("codex thread %s started, but rename to %q failed: %w", threadID, rename, renameErr)
		}
		return nil
	})
	if err != nil {
		return session.Session{}, err
	}
	return session.Session{Key: session.Key{Provider: session.ProviderCodex, ID: threadID}, CWD: cwd}, nil
}

// cleanupStep runs one post-commit step under its own bound, detached from
// ctx's cancellation.
func cleanupStep(ctx context.Context, step func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), dispatchCleanupTimeout)
	defer cancel()
	return step(ctx)
}

// startThread creates a durable, user-sourced thread in cwd and returns its
// ID. It overrides nothing else: model, approval, sandbox and environment
// resolve from the daemon's Codex configuration for cwd.
func startThread(ctx context.Context, conn *rpcConn, cwd string) (string, error) {
	var res struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	params := map[string]any{"cwd": cwd, "ephemeral": false, "threadSource": "user"}
	if err := conn.call(ctx, "thread/start", params, &res); err != nil {
		return "", err
	}
	if res.Thread.ID == "" {
		return "", errors.New("malformed response: empty thread id")
	}
	return res.Thread.ID, nil
}

// startTurn submits prompt as one text input on threadID, with the
// settings thread/start established, and returns the turn's ID once the
// daemon accepted it. Acceptance is not persistence: see turnStarts.
func startTurn(ctx context.Context, conn *rpcConn, threadID, prompt string) (string, error) {
	var res struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	params := map[string]any{
		"threadId":    threadID,
		"turnTrigger": "user",
		"input":       []any{map[string]any{"type": "text", "text": prompt, "textElements": []any{}}},
	}
	if err := conn.call(ctx, "turn/start", params, &res); err != nil {
		return "", err
	}
	if res.Turn.ID == "" {
		return "", errors.New("malformed response: empty turn id")
	}
	return res.Turn.ID, nil
}

// turnStarts records the turn/started notifications one connection
// receives. The daemon persists a turn's start to the thread's rollout
// before it sends turn/started, while the turn/start response only says the
// turn was accepted; so a matching turn/started is the point from which the
// thread's rollout exists for metadata updates such as a rename.
type turnStarts struct {
	mu   sync.Mutex
	seen map[[2]string]bool // {thread ID, turn ID}
	// changed is closed, and replaced, whenever a turn start is recorded.
	changed chan struct{}
}

func newTurnStarts() *turnStarts {
	return &turnStarts{seen: map[[2]string]bool{}, changed: make(chan struct{})}
}

// observe is the connection's notification callback. It never blocks.
func (s *turnStarts) observe(method string, params json.RawMessage) {
	if method != "turn/started" {
		return
	}
	var p struct {
		ThreadID string `json:"threadId"`
		Turn     struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if json.Unmarshal(params, &p) != nil || p.ThreadID == "" || p.Turn.ID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen[[2]string{p.ThreadID, p.Turn.ID}] = true
	close(s.changed)
	s.changed = make(chan struct{})
}

// wait returns once turnID's turn/started for threadID was recorded, whether
// before or after wait was called. It fails when ctx ends or conn closes.
func (s *turnStarts) wait(ctx context.Context, conn *rpcConn, threadID, turnID string) error {
	for {
		s.mu.Lock()
		seen, changed := s.seen[[2]string{threadID, turnID}], s.changed
		s.mu.Unlock()
		if seen {
			return nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		case <-conn.Done():
			return conn.Err()
		}
	}
}

// unsubscribeThread releases this connection's subscription to threadID.
// unsubscribed, notSubscribed and notLoaded all mean no subscription is
// left; any other status is reported as an anomaly.
func unsubscribeThread(ctx context.Context, conn *rpcConn, threadID string) error {
	var res struct {
		Status string `json:"status"`
	}
	if err := conn.call(ctx, "thread/unsubscribe", map[string]any{"threadId": threadID}, &res); err != nil {
		return err
	}
	switch res.Status {
	case "unsubscribed", "notSubscribed", "notLoaded":
		return nil
	}
	return fmt.Errorf("unexpected thread/unsubscribe status %q", res.Status)
}

// setThreadName renames threadID natively.
func setThreadName(ctx context.Context, conn *rpcConn, threadID, name string) error {
	return conn.call(ctx, "thread/name/set", map[string]any{"threadId": threadID, "name": name}, nil)
}
