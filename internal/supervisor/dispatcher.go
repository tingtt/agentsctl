package supervisor

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"

	"github.com/tingtt/agentsctl/internal/localstate"
)

type Dispatcher struct {
	Client Client
}

func (d Dispatcher) Dispatch(ctx context.Context, prompt, cwd string, baseline []string, environment map[string]string) (localstate.Run, error) {
	id, err := newID()
	if err != nil {
		return localstate.Run{}, err
	}
	res, err := d.Client.Call(ctx, Request{Action: "start", RunID: id, Args: []string{prompt}, CWD: cwd, Provider: "codex", Baseline: baseline, Environment: environment})
	if err != nil {
		return localstate.Run{}, err
	}
	if res.Run == nil {
		return localstate.Run{}, errors.New("supervisor returned no run")
	}
	return *res.Run, nil
}
func (d Dispatcher) Resume(ctx context.Context, runID, threadID, cwd string, environment map[string]string) (localstate.Run, error) {
	res, err := d.Client.Call(ctx, Request{Action: "start", RunID: runID, SessionID: threadID, Args: []string{"resume", threadID}, CWD: cwd, Provider: "codex", Environment: environment})
	if err != nil {
		return localstate.Run{}, err
	}
	return *res.Run, nil
}
func (d Dispatcher) ResumeExisting(ctx context.Context, threadID, cwd string, environment map[string]string) (localstate.Run, error) {
	id, err := newID()
	if err != nil {
		return localstate.Run{}, err
	}
	return d.Resume(ctx, id, threadID, cwd, environment)
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
