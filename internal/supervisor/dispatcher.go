package supervisor

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"

	"github.com/tingtt/agentsctl/internal/state"
)

type Dispatcher struct {
	Client Client
}

func (d Dispatcher) Dispatch(ctx context.Context, prompt, cwd string, baseline []string) (state.Run, error) {
	id, err := newID()
	if err != nil {
		return state.Run{}, err
	}
	res, err := d.Client.Call(ctx, Request{Action: "start", RunID: id, Args: []string{prompt}, CWD: cwd, Provider: "codex", Baseline: baseline})
	if err != nil {
		return state.Run{}, err
	}
	if res.Run == nil {
		return state.Run{}, errors.New("supervisor returned no run")
	}
	return *res.Run, nil
}
func (d Dispatcher) Resume(ctx context.Context, runID, threadID, cwd string) (state.Run, error) {
	res, err := d.Client.Call(ctx, Request{Action: "start", RunID: runID, SessionID: threadID, Args: []string{"resume", threadID}, CWD: cwd, Provider: "codex"})
	if err != nil {
		return state.Run{}, err
	}
	return *res.Run, nil
}
func (d Dispatcher) ResumeExisting(ctx context.Context, threadID, cwd string) (state.Run, error) {
	id, err := newID()
	if err != nil {
		return state.Run{}, err
	}
	return d.Resume(ctx, id, threadID, cwd)
}
func (d Dispatcher) Stop(ctx context.Context, id string) error {
	_, err := d.Client.Call(ctx, Request{Action: "stop", RunID: id})
	return err
}
func newID() (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "run-" + hex.EncodeToString(b), nil
}
