package sessionctl

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/tingtt/agentsctl/internal/session"
)

// listCounter counts List calls on top of a fakeSource.
type listCounter struct {
	fakeSource
	lists atomic.Int32
}

func (l *listCounter) List(ctx context.Context, archived bool) ([]session.Session, error) {
	l.lists.Add(1)
	return l.fakeSource.List(ctx, archived)
}

func TestLoadProviderListsOnlyThatProviderAndNarrowsActions(t *testing.T) {
	row := func(p session.ProviderID) session.Session {
		return session.Session{Key: session.Key{Provider: p, ID: "a"}, Actions: session.Actions{session.ActionOpen: {Available: true}}}
	}
	codex := &listCounter{fakeSource: fakeSource{id: session.ProviderCodex, rows: []session.Session{row(session.ProviderCodex)}}}
	claude := &listCounter{fakeSource: fakeSource{id: session.ProviderClaude, rows: []session.Session{row(session.ProviderClaude)}}}
	c := Controller{Providers: []Source{claude, codex}}

	got := c.LoadProvider(context.Background(), session.ProviderCodex)
	if got.Provider != session.ProviderCodex || got.Err != nil || len(got.Sessions) != 1 {
		t.Fatalf("got = %+v", got)
	}
	if codex.lists.Load() != 1 || claude.lists.Load() != 0 {
		t.Fatalf("lists: codex=%d claude=%d, want only codex Listed", codex.lists.Load(), claude.lists.Load())
	}
	if got.Sessions[0].Actions.Available(session.ActionOpen) {
		t.Fatalf("actionsFor must narrow like LoadStream does (fakeSource is not an Opener): %+v", got.Sessions[0].Actions)
	}
}

func TestLoadProviderMatchesLoadStreamSnapshot(t *testing.T) {
	src := &fakeObserverSource{fakeSource: fakeSource{id: session.ProviderChatGPT, rows: []session.Session{{Key: session.Key{Provider: session.ProviderChatGPT, ID: "a"}}}}}
	c := Controller{Providers: []Source{src}}
	var streamed ProviderSnapshot
	for ps := range c.LoadStream(context.Background()) {
		streamed = ps
	}
	got := c.LoadProvider(context.Background(), session.ProviderChatGPT)
	if len(got.Sessions) != len(streamed.Sessions) || got.Err != nil {
		t.Fatalf("LoadProvider = %+v, LoadStream = %+v, want the same snapshot", got, streamed)
	}
}

func TestLoadProviderReportsErrors(t *testing.T) {
	boom := errors.New("boom")
	c := Controller{Providers: []Source{fakeSource{id: session.ProviderCodex, err: boom}}}
	if got := c.LoadProvider(context.Background(), session.ProviderCodex); !errors.Is(got.Err, boom) || got.Sessions != nil {
		t.Fatalf("got = %+v, want the List error and no sessions", got)
	}
	if got := c.LoadProvider(context.Background(), session.ProviderClaude); got.Err == nil {
		t.Fatalf("an unconfigured provider must report an error: %+v", got)
	}
}
