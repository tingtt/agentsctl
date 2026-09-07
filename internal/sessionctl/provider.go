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
