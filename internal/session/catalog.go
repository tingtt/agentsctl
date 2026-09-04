package session

import (
	"context"
	"errors"
	"path/filepath"
	"sort"
	"strings"
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

// DirectoryScope selects which sessions' CWDs the overview shows, relative
// to the directory agentsctl was started in.
type DirectoryScope int

const (
	// ScopeCWD shows only sessions whose CWD is exactly the directory
	// agentsctl was started in.
	ScopeCWD DirectoryScope = iota
	// ScopeSubtree shows sessions whose CWD is the starting directory
	// itself or any descendant of it (an inclusive recursive subtree).
	ScopeSubtree
	// ScopeAll shows every session regardless of CWD.
	ScopeAll
)

type Scope struct {
	CurrentDirectory string
	Directory        DirectoryScope
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
	switch scope.Directory {
	case ScopeAll:
		// No directory filter.
	case ScopeSubtree:
		current := normalizeDirectory(scope.CurrentDirectory)
		filtered := result.Sessions[:0]
		for _, row := range result.Sessions {
			if isWithinSubtree(current, row.CWD) {
				filtered = append(filtered, row)
			}
		}
		result.Sessions = filtered
	default: // ScopeCWD
		current := normalizeDirectory(scope.CurrentDirectory)
		filtered := result.Sessions[:0]
		for _, row := range result.Sessions {
			if normalizeDirectory(row.CWD) == current {
				filtered = append(filtered, row)
			}
		}
		result.Sessions = filtered
	}
	SortOverview(result.Sessions)
	return result
}

// SortOverview sorts sessions into the canonical overview order: pinned
// sessions first, each group ordered by CreatedAt descending, ties broken
// by session key. This is the single ordering implementation shared by
// Catalog.Load and any in-memory update (e.g. a local pin toggle) that
// must reproduce the same order without a full provider refresh.
func SortOverview(sessions []Session) {
	sort.SliceStable(sessions, func(i, j int) bool {
		a, b := sessions[i], sessions[j]
		if a.Pinned != b.Pinned {
			return a.Pinned
		}
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.After(b.CreatedAt)
		}
		return a.Key.String() < b.Key.String()
	})
}

// TogglePin changes and returns the pinned state for key.
func (c Catalog) TogglePin(key Key) (bool, error) {
	if c.Pins == nil {
		return false, errors.New("pin metadata store is not configured")
	}
	return c.Pins.TogglePinned(key.String())
}

// normalizeDirectory intentionally does not resolve symlinks. codex-agents
// compares filepath-cleaned logical paths and agentsctl keeps that UX contract.
func normalizeDirectory(path string) string { return filepath.Clean(path) }

// isWithinSubtree reports whether candidate is root itself or a descendant
// of root, using filepath.Rel on cleaned logical paths (no symlink
// resolution, matching normalizeDirectory) rather than strings.HasPrefix —
// a prefix match alone cannot tell "/foo/bar" apart from the sibling
// "/foo/bar-other", since the latter starts with the former as a string
// but not as a path.
func isWithinSubtree(root, candidate string) bool {
	root = normalizeDirectory(root)
	candidate = normalizeDirectory(candidate)
	if root == candidate {
		return true
	}
	rel, err := filepath.Rel(root, candidate)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func (c Catalog) Provider(id ProviderID) (Provider, error) {
	for _, p := range c.Providers {
		if p.ID() == id {
			return p, nil
		}
	}
	return nil, errors.New("provider is not configured")
}
