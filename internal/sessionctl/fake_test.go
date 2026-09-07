package sessionctl

import (
	"context"
	"errors"
	"io"
	"os"

	"github.com/tingtt/agentsctl/internal/session"
)

// fakeSource implements only Source -- modeling a minimal, List+nothing-
// else provider (the architecture acceptance shape for a future partial
// provider such as an initial ChatGPT integration, see Issue #6).
type fakeSource struct {
	id   session.ProviderID
	rows []session.Session
	err  error
}

func (f fakeSource) ID() session.ProviderID { return f.id }
func (f fakeSource) List(context.Context, bool) ([]session.Session, error) {
	return f.rows, f.err
}

// fakeOpenerSource is a partial provider implementing Source+Opener only
// -- the architecture-acceptance shape from Issue #15 (List+Open without
// Stop/Rename/Archive/Dispatch).
type fakeOpenerSource struct {
	fakeSource
	openErr error
	opened  []session.Key
}

func (f *fakeOpenerSource) Open(_ context.Context, s session.Session, _ *os.File, _ io.Writer) error {
	if f.openErr != nil {
		return f.openErr
	}
	f.opened = append(f.opened, s.Key)
	return nil
}

// fakeFullProvider implements every capability interface, modeling
// Claude/Codex-shaped providers.
type fakeFullProvider struct {
	fakeSource
	dispatchResult session.Session
	dispatchErr    error
	dispatchedArgs []string // prompt, cwd

	openErr error
	opened  []session.Key

	stopErr    error
	stopped    []session.Key
	renameErr  error
	renamed    map[session.Key]string
	archiveErr error
	archived   []session.Key
}

func (f *fakeFullProvider) Dispatch(_ context.Context, prompt, cwd string) (session.Session, error) {
	f.dispatchedArgs = []string{prompt, cwd}
	return f.dispatchResult, f.dispatchErr
}
func (f *fakeFullProvider) Open(_ context.Context, s session.Session, _ *os.File, _ io.Writer) error {
	if f.openErr != nil {
		return f.openErr
	}
	f.opened = append(f.opened, s.Key)
	return nil
}
func (f *fakeFullProvider) Stop(_ context.Context, key session.Key) error {
	if f.stopErr != nil {
		return f.stopErr
	}
	f.stopped = append(f.stopped, key)
	return nil
}
func (f *fakeFullProvider) Rename(_ context.Context, key session.Key, name string) error {
	if f.renameErr != nil {
		return f.renameErr
	}
	if f.renamed == nil {
		f.renamed = map[session.Key]string{}
	}
	f.renamed[key] = name
	return nil
}
func (f *fakeFullProvider) Archive(_ context.Context, key session.Key) error {
	if f.archiveErr != nil {
		return f.archiveErr
	}
	f.archived = append(f.archived, key)
	return nil
}

type fakePinStore struct {
	pinned map[string]bool
	listErr,
	toggleErr error
}

func (p *fakePinStore) ListPinned() (map[string]bool, error) {
	if p.listErr != nil {
		return nil, p.listErr
	}
	out := make(map[string]bool, len(p.pinned))
	for k, v := range p.pinned {
		out[k] = v
	}
	return out, nil
}
func (p *fakePinStore) TogglePinned(key string) (bool, error) {
	if p.toggleErr != nil {
		return false, p.toggleErr
	}
	if p.pinned == nil {
		p.pinned = map[string]bool{}
	}
	next := !p.pinned[key]
	if next {
		p.pinned[key] = true
	} else {
		delete(p.pinned, key)
	}
	return next, nil
}

var errBoom = errors.New("boom")
