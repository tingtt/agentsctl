package sessionctl

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tingtt/agentsctl/internal/session"
)

// waitForObserverUpdate blocks until updates delivers one ObserverUpdate
// or fails the test after a bounded timeout.
func waitForObserverUpdate(t *testing.T, updates <-chan ObserverUpdate) ObserverUpdate {
	t.Helper()
	select {
	case upd, ok := <-updates:
		if !ok {
			t.Fatal("Observe channel closed before publishing an update")
		}
		return upd
	case <-time.After(2 * time.Second):
		t.Fatal("no ObserverUpdate published within 2s")
		return ObserverUpdate{}
	}
}

// TestObserveAppliesActionsForToSuccessfulSessionsWithNoWarning fixes the
// plain success case: Sessions is actionsFor-narrowed and tagged with the
// provider, Err/Warning stay nil.
func TestObserveAppliesActionsForToSuccessfulSessionsWithNoWarning(t *testing.T) {
	src := &fakeObserverSource{fakeSource: fakeSource{id: session.ProviderChatGPT}, updates: make(chan ProviderUpdate, 1)}
	c := Controller{Providers: []Source{src}}
	updates := c.Observe(context.Background())

	row := session.Session{Key: session.Key{Provider: session.ProviderChatGPT, ID: "a"}, Actions: session.Actions{session.ActionOpen: {Available: true}}}
	src.updates <- ProviderUpdate{Sessions: []session.Session{row}}

	got := waitForObserverUpdate(t, updates)
	if got.Provider != session.ProviderChatGPT || got.Err != nil || got.Warning != nil {
		t.Fatalf("got=%+v", got)
	}
	if len(got.Sessions) != 1 || got.Sessions[0].Actions.Available(session.ActionOpen) {
		t.Fatalf("expected ActionOpen denied (fakeObserverSource does not implement Opener): %+v", got.Sessions)
	}
}

// TestObserveSuccessfulSessionsWithWarningPreservesBoth fixes that a
// non-fatal Warning alongside valid Sessions passes through Observe
// untouched -- Sessions still fully replaces the provider's rows, and
// Warning is neither dropped nor treated as a reason to discard Sessions.
func TestObserveSuccessfulSessionsWithWarningPreservesBoth(t *testing.T) {
	src := &fakeObserverSource{fakeSource: fakeSource{id: session.ProviderChatGPT}, updates: make(chan ProviderUpdate, 1)}
	c := Controller{Providers: []Source{src}}
	updates := c.Observe(context.Background())

	warning := errors.New("persist ChatGPT catalog: disk full")
	row := session.Session{Key: session.Key{Provider: session.ProviderChatGPT, ID: "b"}}
	src.updates <- ProviderUpdate{Sessions: []session.Session{row}, Warning: warning}

	got := waitForObserverUpdate(t, updates)
	if got.Err != nil {
		t.Fatalf("expected no Err alongside a Warning: %+v", got)
	}
	if len(got.Sessions) != 1 || got.Sessions[0].Key.ID != "b" {
		t.Fatalf("expected Sessions to replace the provider's rows despite the Warning: %+v", got.Sessions)
	}
	if !errors.Is(got.Warning, warning) && got.Warning.Error() != warning.Error() {
		t.Fatalf("Warning did not pass through: got=%v want=%v", got.Warning, warning)
	}
}

// TestObserveRefreshErrorCarriesNoSessions fixes the failure shape: Err
// set, Sessions nil -- a consumer must retain its own last-known rows.
func TestObserveRefreshErrorCarriesNoSessions(t *testing.T) {
	src := &fakeObserverSource{fakeSource: fakeSource{id: session.ProviderChatGPT}, updates: make(chan ProviderUpdate, 1)}
	c := Controller{Providers: []Source{src}}
	updates := c.Observe(context.Background())

	src.updates <- ProviderUpdate{Err: errors.New("timeout")}

	got := waitForObserverUpdate(t, updates)
	if got.Err == nil {
		t.Fatal("expected Err to be set")
	}
	if got.Sessions != nil {
		t.Fatalf("a failed refresh update must carry no Sessions: %+v", got)
	}
	if got.Warning != nil {
		t.Fatalf("a failed refresh update must carry no Warning: %+v", got)
	}
}
