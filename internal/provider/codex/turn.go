package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
)

const (
	turnStatusCompleted   = "completed"
	turnStatusInterrupted = "interrupted"
	turnStatusFailed      = "failed"
	turnStatusInProgress  = "inProgress"
)

var errTurnsListUnmaterialized = errors.New("codex thread turns are not materialized")

const turnsListUnmaterializedMessage = "thread/turns/list is unavailable before first user message"

type turnKey struct {
	threadID string
	turnID   string
}

// turnLifecycleEvents records the small set of notifications needed by a
// connection that controls a turn. Recording starts when the connection is
// created, so a notification arriving before its request response is not
// lost. observe never blocks the rpcConn reader.
type turnLifecycleEvents struct {
	mu        sync.Mutex
	started   map[turnKey]bool
	completed map[turnKey]string
	status    map[string]ThreadStatus
	changed   chan struct{}
}

func newTurnLifecycleEvents() *turnLifecycleEvents {
	return &turnLifecycleEvents{
		started:   map[turnKey]bool{},
		completed: map[turnKey]string{},
		status:    map[string]ThreadStatus{},
		changed:   make(chan struct{}),
	}
}

func (e *turnLifecycleEvents) observe(method string, params json.RawMessage) {
	e.mu.Lock()
	defer e.mu.Unlock()
	changed := false
	switch method {
	case "turn/started", "turn/completed":
		var p struct {
			ThreadID string `json:"threadId"`
			Turn     struct {
				ID     string `json:"id"`
				Status string `json:"status"`
			} `json:"turn"`
		}
		if json.Unmarshal(params, &p) != nil || p.ThreadID == "" || p.Turn.ID == "" {
			return
		}
		key := turnKey{threadID: p.ThreadID, turnID: p.Turn.ID}
		if method == "turn/started" {
			e.started[key] = true
		} else {
			e.completed[key] = p.Turn.Status
		}
		changed = true
	case notifyStatusChanged:
		var p struct {
			ThreadID string       `json:"threadId"`
			Status   ThreadStatus `json:"status"`
		}
		if json.Unmarshal(params, &p) != nil || p.ThreadID == "" || p.Status.Type == "" {
			return
		}
		e.status[p.ThreadID] = p.Status
		changed = true
	}
	if changed {
		close(e.changed)
		e.changed = make(chan struct{})
	}
}

func (e *turnLifecycleEvents) waitStarted(ctx context.Context, conn *rpcConn, threadID, turnID string) error {
	key := turnKey{threadID: threadID, turnID: turnID}
	return e.wait(ctx, conn, func() (bool, error) {
		return e.started[key], nil
	})
}

func (e *turnLifecycleEvents) waitCompleted(ctx context.Context, conn *rpcConn, threadID, turnID string) error {
	key := turnKey{threadID: threadID, turnID: turnID}
	return e.wait(ctx, conn, func() (bool, error) {
		status, ok := e.completed[key]
		if !ok {
			return false, nil
		}
		if status != turnStatusInterrupted {
			return false, fmt.Errorf("turn/completed status is %q, want %q", status, turnStatusInterrupted)
		}
		return true, nil
	})
}

func (e *turnLifecycleEvents) waitInactive(ctx context.Context, conn *rpcConn, threadID string) error {
	return e.wait(ctx, conn, func() (bool, error) {
		status, ok := e.status[threadID]
		return ok && status.Type != statusActive, nil
	})
}

func (e *turnLifecycleEvents) wait(ctx context.Context, conn *rpcConn, ready func() (bool, error)) error {
	for {
		e.mu.Lock()
		ok, err := ready()
		changed := e.changed
		e.mu.Unlock()
		if ok || err != nil {
			return err
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

// interruptTurn asks the shared daemon to interrupt exactly turnID. Callers
// resolve the turn differently (Dispatch already knows it; Stop looks it up
// at operation time) and own their respective completion policy.
func interruptTurn(ctx context.Context, conn *rpcConn, threadID, turnID string) error {
	return conn.call(ctx, "turn/interrupt", map[string]any{"threadId": threadID, "turnId": turnID}, nil)
}

type currentTurnState struct {
	status       ThreadStatus
	activeTurnID string
}

// readCurrentTurnState resolves the daemon's current active turn identity.
// thread/turns/list in Codex 0.157.1 merges the live in-progress turn into
// the newest page; a loaded active status without exactly one such turn is
// contradictory and is rejected.
func readCurrentTurnState(ctx context.Context, conn *rpcConn, threadID string) (currentTurnState, error) {
	t, err := readThread(ctx, conn, threadID)
	if err != nil {
		return currentTurnState{}, fmt.Errorf("thread/read %s: %w", threadID, err)
	}
	if t.ID != threadID || t.Status.Type == "" {
		return currentTurnState{}, fmt.Errorf("thread/read %s: malformed response", threadID)
	}
	state := currentTurnState{status: t.Status}
	switch t.Status.Type {
	case statusIdle, statusSystemError, statusNotLoaded:
		return state, nil
	case statusActive:
	default:
		return currentTurnState{}, fmt.Errorf("codex thread %s has unknown status %q", threadID, t.Status.Type)
	}

	var res struct {
		Data []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"data"`
	}
	params := map[string]any{"threadId": threadID, "limit": 100, "sortDirection": "desc", "itemsView": "notLoaded"}
	if err := conn.call(ctx, "thread/turns/list", params, &res); err != nil {
		if isTurnsListUnmaterialized(err) {
			return state, fmt.Errorf("%w: thread/turns/list %s: %v", errTurnsListUnmaterialized, threadID, err)
		}
		return currentTurnState{}, fmt.Errorf("thread/turns/list %s: %w", threadID, err)
	}
	for _, turn := range res.Data {
		if turn.Status != turnStatusInProgress {
			continue
		}
		if turn.ID == "" || state.activeTurnID != "" {
			return currentTurnState{}, fmt.Errorf("codex thread %s has contradictory active turn identity", threadID)
		}
		state.activeTurnID = turn.ID
	}
	if state.activeTurnID == "" {
		// The turn can finish between thread/read and thread/turns/list.
		// Re-read once to distinguish that race from a contradictory active
		// runtime; this is reconciliation, not a retry of the control action.
		latest, err := readThread(ctx, conn, threadID)
		if err != nil {
			return currentTurnState{}, fmt.Errorf("re-read thread %s after active-turn race: %w", threadID, err)
		}
		if latest.ID != threadID || latest.Status.Type == "" {
			return currentTurnState{}, fmt.Errorf("thread/read %s: malformed response", threadID)
		}
		switch latest.Status.Type {
		case statusIdle, statusSystemError, statusNotLoaded:
			return currentTurnState{status: latest.Status}, nil
		case statusActive:
			return currentTurnState{}, fmt.Errorf("codex thread %s is active but has no in-progress turn", threadID)
		default:
			return currentTurnState{}, fmt.Errorf("codex thread %s has unknown status %q", threadID, latest.Status.Type)
		}
	}
	return state, nil
}

// isTurnsListUnmaterialized classifies the Codex 0.157.1 InvalidRequest
// message. The protocol exposes no dedicated error code or structured
// reason for this pre-materialization condition.
func isTurnsListUnmaterialized(err error) bool {
	return err != nil && strings.Contains(err.Error(), turnsListUnmaterializedMessage)
}

func cleanupBootstrapTurn(ctx context.Context, conn *rpcConn, events *turnLifecycleEvents, threadID, turnID string) error {
	if err := interruptTurn(ctx, conn, threadID, turnID); err != nil {
		state, stateErr := readCurrentTurnState(ctx, conn, threadID)
		if stateErr != nil {
			return errors.Join(err, fmt.Errorf("confirm bootstrap turn state: %w", stateErr))
		}
		if state.activeTurnID == "" {
			return nil
		}
		if state.activeTurnID != turnID {
			return fmt.Errorf("interrupt bootstrap turn %s failed: %w; thread now has different active turn %s", turnID, err, state.activeTurnID)
		}
		return fmt.Errorf("interrupt bootstrap turn %s failed and it remains active: %w", turnID, err)
	}
	if err := events.waitCompleted(ctx, conn, threadID, turnID); err != nil {
		return fmt.Errorf("wait for interrupted turn/completed: %w", err)
	}
	return nil
}
