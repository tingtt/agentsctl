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
	// MigratePinned atomically carries a pin from one key to another (see
	// session.Session.PreviousKeys); a no-op if from is not pinned.
	MigratePinned(from, to string) error
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

// ProviderSnapshot is one provider's incremental Load contribution --
// LoadStream's per-arrival unit, mirroring UsageStream's UsageUpdate for
// the catalog side. Sessions is already actionsFor-narrowed, matching
// what Load's own per-provider fetch does; Err is set instead when that
// provider's List call failed (never both).
//
// ListOwnsStatus reports whether a successful List (Err == nil) is
// authoritative for this provider's refresh/availability status -- true
// for an ordinary Source-only provider (a successful List means it
// recovered from any previous failure, exactly like before this field
// existed), false for a provider that also implements Observer. For the
// latter, List may simply be serving a last-known-good cache (see e.g.
// provider/chatgpt): a successful cached read says nothing about
// whether background refresh/durability actually recovered, so it must
// not silently clear a warning only Observer is entitled to replace or
// clear (see the DesignDoc's "Catalog validity vs. local durability" /
// this field's rationale). A List failure (Err != nil) is always
// surfaced as this provider's warning regardless of ListOwnsStatus -- a
// real Source.List error (misconfiguration, browser unavailable, ...) is
// never hidden just because the provider also has an Observer.
type ProviderSnapshot struct {
	Provider       session.ProviderID
	Sessions       []session.Session
	Err            error
	ListOwnsStatus bool
}

// LoadStream is Load's incremental counterpart: the same concurrent,
// per-provider List call, but delivered as each provider's own call
// completes rather than collected into one batch behind the slowest
// provider (see Load's doc comment). This is what lets a caller (Agent
// View) show and act on a fast provider's rows (e.g. Claude, Codex)
// without waiting on a slow one (e.g. ChatGPT's multi-page browser-backed
// discovery walk) -- the catalog-loading counterpart to UsageStream's
// existing latency isolation.
//
// Unlike Load, LoadStream does not apply pin state, scope filtering, or
// overview ordering: those operate over the accumulated set of every
// provider heard from so far, which only a caller ranging over the
// channel can know at any given moment (see MergeSessions).
//
// The channel is closed once every provider has sent its ProviderSnapshot
// (immediately, already closed, if c.Providers is empty).
func (c Controller) LoadStream(ctx context.Context) <-chan ProviderSnapshot {
	out := make(chan ProviderSnapshot)
	var wg sync.WaitGroup
	for _, p := range c.Providers {
		p := p
		_, observes := p.(Observer)
		listOwnsStatus := !observes
		wg.Add(1)
		go func() {
			defer wg.Done()
			rows, err := p.List(ctx, false)
			if err != nil {
				out <- ProviderSnapshot{Provider: p.ID(), Err: err, ListOwnsStatus: listOwnsStatus}
				return
			}
			// A fresh slice, never rows itself: a provider whose List
			// returns a live reference into its own mutable state (a
			// test double, typically) must never be mutated here.
			enriched := make([]session.Session, len(rows))
			for i := range rows {
				enriched[i] = rows[i]
				enriched[i].Actions = actionsFor(p, rows[i])
			}
			out <- ProviderSnapshot{Provider: p.ID(), Sessions: enriched, ListOwnsStatus: listOwnsStatus}
		}()
	}
	go func() {
		wg.Wait()
		close(out)
	}()
	return out
}

// ObserverUpdate is one Observer-sourced provider update, mirroring
// ProviderSnapshot for the Observe side: the same provider-tagged,
// actionsFor-narrowed shape LoadStream's ProviderSnapshot uses, so a
// consumer (Agent View's provider snapshot store) can apply either
// through one code path. Sessions is nil and Err is set on a refresh
// failure, exactly like ProviderUpdate -- Observe narrows/tags the
// underlying provider.Observer publication but preserves that contract,
// Warning (a non-fatal problem alongside otherwise-valid Sessions --
// see ProviderUpdate's doc comment) included.
type ObserverUpdate struct {
	Provider session.ProviderID
	Sessions []session.Session
	Err      error
	Warning  error
}

// Observe subscribes once to every configured provider that implements
// Observer, merging their independent publication streams into one
// channel, each tagged with its provider and actionsFor-narrowed the same
// way LoadStream's ProviderSnapshot is (see actionsFor). Unlike
// LoadStream, this is not one reload cycle: it is a long-lived
// subscription a caller (Agent View) establishes once and drains for as
// long as ctx lives (see the DesignDoc's "Observer generations" -- a
// completed provider refresh is authoritative independent of any Agent
// View reload generation). The returned channel closes once every
// Observer provider's own channel has closed (immediately, already
// closed, if no configured provider implements Observer).
func (c Controller) Observe(ctx context.Context) <-chan ObserverUpdate {
	out := make(chan ObserverUpdate)
	var wg sync.WaitGroup
	for _, p := range c.Providers {
		obs, ok := p.(Observer)
		if !ok {
			continue
		}
		p, obs := p, obs
		wg.Add(1)
		go func() {
			defer wg.Done()
			for upd := range obs.Observe(ctx) {
				if upd.Err != nil {
					out <- ObserverUpdate{Provider: p.ID(), Err: upd.Err}
					continue
				}
				enriched := make([]session.Session, len(upd.Sessions))
				for i := range upd.Sessions {
					enriched[i] = upd.Sessions[i]
					enriched[i].Actions = actionsFor(p, upd.Sessions[i])
				}
				out <- ObserverUpdate{Provider: p.ID(), Sessions: enriched, Warning: upd.Warning}
			}
		}()
	}
	go func() {
		wg.Wait()
		close(out)
	}()
	return out
}

// MergeSessions applies Load's pin-enrichment (including migrating a pin
// held under a session's PreviousKeys to its current Key, see
// migratePins), scope filtering, and overview ordering to an arbitrary collection of already actionsFor-
// narrowed sessions -- the same post-processing Load itself applies to
// its one complete batch, factored out so a caller accumulating
// LoadStream's incremental ProviderSnapshots can re-run it over its own
// running total after each arrival, without duplicating pin-store/Filter/
// SortOverview wiring outside this package.
func (c Controller) MergeSessions(sessions []session.Session, scope session.Scope) []session.Session {
	pinned := map[string]bool{}
	if c.Pins != nil {
		if values, err := c.Pins.ListPinned(); err == nil {
			pinned = values
			c.migratePins(sessions, pinned)
		}
	}
	merged := make([]session.Session, len(sessions))
	copy(merged, sessions)
	for i := range merged {
		merged[i].Pinned = pinned[merged[i].Key.String()]
	}
	merged = session.Filter(merged, scope)
	session.SortOverview(merged)
	return merged
}

// migratePins follows each session's provider-stated identity continuity
// (session.Session.PreviousKeys) in the pin store: a pin left on a
// provisional key -- pinned while the session was still Starting, or pinned
// on a stale view after its binding -- moves to the session's current Key,
// so no provisional pin metadata outlives the transition. It is applied on
// every merge rather than as a one-shot event, which makes the outcome
// independent of how pin operations and catalog arrivals interleave: the
// store move is atomic and idempotent, and once the provisional pin is gone
// there is nothing left to do. pinned is the caller's ListPinned snapshot
// and is updated in place to reflect the moves. If the store cannot persist
// a move the pin is still shown on the current Key (the persisted
// provisional pin is kept, so the next merge retries) rather than
// disappearing from the UI.
func (c Controller) migratePins(sessions []session.Session, pinned map[string]bool) {
	for _, s := range sessions {
		to := s.Key.String()
		for _, prev := range s.PreviousKeys {
			from := prev.String()
			if from == to || !pinned[from] {
				continue
			}
			if err := c.Pins.MigratePinned(from, to); err == nil {
				delete(pinned, from)
			}
			pinned[to] = true
		}
	}
}

// Load fetches every provider's sessions concurrently (so one provider's
// latency is never added to another's -- see the DesignDoc's Catalog
// loading section), narrows each session's Actions to what the concrete
// provider actually implements, enriches with local pin state, applies
// scope filtering, and sorts into the canonical overview order (see
// LoadStream/MergeSessions, which this is built from). One Load call
// always waits for every provider before returning its one complete,
// merged Snapshot; a caller that instead wants each provider's rows as
// soon as they're ready uses LoadStream directly (see Agent View's
// Runtime.requestReload).
func (c Controller) Load(ctx context.Context, scope session.Scope) Snapshot {
	result := Snapshot{Warnings: make(map[session.ProviderID]error)}
	var sessions []session.Session
	for ps := range c.LoadStream(ctx) {
		if ps.Err != nil {
			result.Warnings[ps.Provider] = ps.Err
			continue
		}
		sessions = append(sessions, ps.Sessions...)
	}
	result.Sessions = c.MergeSessions(sessions, scope)
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
