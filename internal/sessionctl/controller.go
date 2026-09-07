package sessionctl

import (
	"context"
	"fmt"
	"sync"

	"github.com/tingtt/agentsctl/internal/session"
)

// PinStore persists provider-qualified session pin metadata. Implemented by
// internal/localstate; kept as a small consumer-side interface here (per
// the same capability-composition principle as Source/Dispatcher/...) so
// sessionctl never needs to know the persisted representation.
type PinStore interface {
	ListPinned() (map[string]bool, error)
	TogglePinned(key string) (bool, error)
}

// Controller is the provider-neutral session control boundary Agent View
// depends on. It never imports a concrete provider package or switches on
// session.ProviderID; every operation routes purely through the small
// capability interfaces in provider.go.
type Controller struct {
	Providers []Source
	Pins      PinStore
}

// Snapshot is one Load result: every visible session plus, per provider
// that failed, the error that made it unavailable -- a provider failure
// never discards sessions the other providers returned (see the
// DesignDoc's "provider catalog の partial failure を許容し...").
type Snapshot struct {
	Sessions []session.Session
	Warnings map[session.ProviderID]error
}

// Load fetches every provider's sessions concurrently (so one provider's
// latency is never added to another's -- see the DesignDoc's Catalog
// loading section), narrows each session's Actions to what the concrete
// provider actually implements (actionsFor), enriches with local pin
// state, applies scope filtering, and sorts into the canonical overview
// order. It performs no partial/incremental rendering: one Load call
// always waits for every provider before returning.
func (c Controller) Load(ctx context.Context, scope session.Scope) Snapshot {
	var wg sync.WaitGroup
	var mu sync.Mutex
	result := Snapshot{Warnings: make(map[session.ProviderID]error)}
	for _, p := range c.Providers {
		p := p
		wg.Add(1)
		go func() {
			defer wg.Done()
			rows, err := p.List(ctx, false)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				result.Warnings[p.ID()] = err
				return
			}
			for i := range rows {
				rows[i].Actions = actionsFor(p, rows[i])
			}
			result.Sessions = append(result.Sessions, rows...)
		}()
	}
	wg.Wait()
	pinned := map[string]bool{}
	if c.Pins != nil {
		if values, err := c.Pins.ListPinned(); err == nil {
			pinned = values
		}
	}
	for i := range result.Sessions {
		result.Sessions[i].Pinned = pinned[result.Sessions[i].Key.String()]
	}
	result.Sessions = session.Filter(result.Sessions, scope)
	session.SortOverview(result.Sessions)
	return result
}

// actionsFor narrows s.Actions -- as the provider itself reported them --
// to what the concrete provider p actually implements, then applies the
// provider-independent invariants (session.Normalize). This is what lets a
// partial provider (e.g. Source+Opener only) participate without ever
// implementing Stop/Rename/Archive as dummy methods: any action a
// provider's List reported as available but whose capability interface p
// does not satisfy is force-denied here as unsupported, so an
// unsupported capability always fails closed regardless of what a
// provider's own List happened to report.
func actionsFor(p Source, s session.Session) session.Actions {
	actions := make(session.Actions, len(s.Actions))
	for id, v := range s.Actions {
		actions[id] = v
	}
	deny := func(id session.ActionID, implemented bool) {
		if !implemented {
			actions[id] = session.Availability{Reason: "not supported by this provider"}
		}
	}
	_, isOpener := p.(Opener)
	_, isStopper := p.(Stopper)
	_, isRenamer := p.(Renamer)
	_, isArchiver := p.(Archiver)
	deny(session.ActionOpen, isOpener)
	deny(session.ActionStop, isStopper)
	deny(session.ActionRename, isRenamer)
	deny(session.ActionArchive, isArchiver)
	s.Actions = actions
	return session.Normalize(s)
}

// provider looks up the registered Source for id, the shared lookup every
// operation (Dispatch/Open/Stop/Rename/Archive) uses before asserting for
// its specific capability.
func (c Controller) provider(id session.ProviderID) (Source, error) {
	for _, p := range c.Providers {
		if p.ID() == id {
			return p, nil
		}
	}
	return nil, fmt.Errorf("provider %s is not configured", id)
}
