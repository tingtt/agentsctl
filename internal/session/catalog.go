package session

import (
	"context"
	"errors"
	"sync"
)

type Provider interface {
	ID() ProviderID
	Available() error
	List(context.Context, bool) ([]Session, error)
	Dispatch(context.Context, string, string) (Session, error)
	Stop(context.Context, Key) error
	Archive(context.Context, Key) error
	Unarchive(context.Context, Key) error
	Rename(context.Context, Key, string) error
}

type Snapshot struct {
	Sessions []Session
	Warnings map[ProviderID]error
}

// PinStore persists provider-qualified session pin metadata.
type PinStore interface {
	ListPinned() (map[string]bool, error)
	TogglePinned(string) (bool, error)
}

type Catalog struct {
	Providers []Provider
	Pins      PinStore
}

func (c Catalog) Load(ctx context.Context, scope Scope) Snapshot {
	var wg sync.WaitGroup
	var mu sync.Mutex
	result := Snapshot{Warnings: make(map[ProviderID]error)}
	for _, p := range c.Providers {
		p := p
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := p.Available(); err != nil {
				mu.Lock()
				result.Warnings[p.ID()] = err
				mu.Unlock()
				return
			}
			rows, err := p.List(ctx, false)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				result.Warnings[p.ID()] = err
				return
			}
			for i := range rows {
				rows[i].Capabilities = CapabilitiesFor(rows[i])
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
	result.Sessions = Filter(result.Sessions, scope)
	SortOverview(result.Sessions)
	return result
}

// TogglePin changes and returns the pinned state for key.
func (c Catalog) TogglePin(key Key) (bool, error) {
	if c.Pins == nil {
		return false, errors.New("pin metadata store is not configured")
	}
	return c.Pins.TogglePinned(key.String())
}

func (c Catalog) Provider(id ProviderID) (Provider, error) {
	for _, p := range c.Providers {
		if p.ID() == id {
			return p, nil
		}
	}
	return nil, errors.New("provider is not configured")
}
