// Package sessionctl is the provider-neutral session control boundary
// between Agent View and each provider's implementation. It defines small,
// consumer-side capability interfaces (rather than one large Provider
// contract every provider must fully implement -- see the DesignDoc's
// "Replace one large Provider contract with capability composition") and a
// Controller that routes Agent View intents to whichever capability a
// concrete provider actually implements, failing closed for anything it
// doesn't.
package sessionctl

import (
	"context"
	"io"
	"os"

	"github.com/tingtt/agentsctl/internal/session"
)

// Source is the minimum capability every provider must implement: identity
// and the ability to list its own sessions, normalized into the common
// session.Session model. A provider implementing only Source (e.g. an
// initial List+Open-only integration -- see Issue #6) still participates
// fully in the unified catalog; Controller.Load fails closed on any other
// action such a provider does not also implement (see actionsFor).
type Source interface {
	ID() session.ProviderID
	List(ctx context.Context, archived bool) ([]session.Session, error)
}

// Dispatcher starts a new session for a provider from a composer prompt.
// Not every provider supports it -- Issue #6's initial ChatGPT integration
// does not create sessions from the shared composer -- so Controller.
// Dispatch fails closed for a Source that is not also a Dispatcher, rather
// than requiring every provider to implement a no-op Dispatch.
type Dispatcher interface {
	Dispatch(ctx context.Context, prompt, cwd string) (session.Session, error)
}

// Opener performs the common Agent View "Open selected session" intent
// (see the DesignDoc's "Open as common Agent View intent"): taking over
// the terminal for s using whatever transport the provider's runtime model
// requires (Claude: `claude attach`; Codex: supervisor PTY attach; a
// future browser-backed provider: launching a browser view). in/out are
// the real terminal file/writer Agent View owns. Open returns once control
// has come back to Agent View -- i.e. the interactive view ended -- not
// when the underlying session itself stops; the DesignDoc's Attach/Detach
// lifecycle separation (detaching never stops the session) is a provider-
// internal guarantee this interface does not itself encode.
type Opener interface {
	Open(ctx context.Context, s session.Session, in *os.File, out io.Writer) error
}

// Stopper terminates the running process/session backing key.
type Stopper interface {
	Stop(ctx context.Context, key session.Key) error
}

// Renamer changes a session's display name. A successful Rename must
// reflect a confirmed, provider-native rename (see provider/claude's
// native-catalog-confirmed rename) -- Controller.Rename applies the given
// name as an already-confirmed local Patch specifically because the
// capability's contract requires that confirmation before returning nil.
type Renamer interface {
	Rename(ctx context.Context, key session.Key, name string) error
}

// Archiver removes a session from the default (non-archived) view.
type Archiver interface {
	Archive(ctx context.Context, key session.Key) error
}

// ProviderUpdate is one Observer publication: either a provider's latest
// complete catalog snapshot (a full replacement, never a delta -- see
// Observer), optionally accompanied by a non-fatal Warning, or a refresh
// failure. Exactly one of two shapes is valid:
//
//   - Err != nil, Sessions == nil, Warning == nil: a refresh failed.
//     A consumer must keep whatever sessions it already has for this
//     provider rather than reading a nil/absent Sessions as "provider has
//     no sessions" (see the DesignDoc's last-known-good cache semantics).
//
//   - Err == nil, Sessions != nil: a refresh succeeded and Sessions is
//     that provider's entire current catalog -- a consumer replaces its
//     retained copy outright. Warning, if also set, does not change that:
//     Sessions is still fully valid and usable (selectable, actionable),
//     but some secondary/non-fatal problem exists alongside it -- e.g. the
//     catalog refreshed correctly yet a provider's own attempt to persist
//     it locally failed (see internal/provider/chatgpt's
//     durabilityWarning). A consumer surfaces Warning (e.g. as a footer
//     notice) without ever treating it as a reason to discard or hide
//     Sessions, and clears any previously-shown Warning for this provider
//     once an update arrives with Warning == nil.
//
// Err and Warning must never both be set on the same update -- they
// answer different questions ("did this refresh fail" vs. "did this
// otherwise-successful refresh's result end up less durable than
// intended") and a provider must pick the one that actually applies to
// this cycle's outcome rather than trying to report both at once (see the
// DesignDoc's "latest provider problem wins" policy).
type ProviderUpdate struct {
	Sessions []session.Session
	Err      error
	Warning  error
}

// Observer is an optional capability for a provider that maintains its own
// last-known-good catalog independent of the request/response List cycle
// (e.g. ChatGPT's browser-backed cache, replaced only by a complete
// background enumeration -- see internal/provider/chatgpt's catalogCache).
// Observe publishes a ProviderUpdate every time that catalog changes --
// full replacement, not incremental add/remove/update -- so a consumer
// (sessionctl.Controller.Observe, ultimately Agent View's provider
// snapshot store) can simply overwrite its retained copy of this
// provider's sessions rather than reconcile a delta.
//
// An Observer subscription is independent of any particular List/reload
// call: it belongs to the provider instance for as long as ctx lives, not
// to one Agent View reload generation (see the DesignDoc's "Observer
// generations"). The channel closes when ctx ends or the provider itself
// shuts down (see e.g. chatgpt.Provider.Close).
type Observer interface {
	Observe(ctx context.Context) <-chan ProviderUpdate
}

// Refresher is an optional capability: requests a background catalog
// refresh without blocking the caller for its result -- the eventual
// outcome (success or failure) arrives later through Observer, never as
// Refresh's own return value. A provider implementing Refresher is
// expected to coalesce concurrent/rapid Refresh requests into at most one
// extra refresh after the one already running (see the DesignDoc's
// single-flight refresh state machine) rather than starting one
// enumeration per call.
type Refresher interface {
	Refresh(ctx context.Context)
}
