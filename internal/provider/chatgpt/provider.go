package chatgpt

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/tingtt/agentsctl/internal/localstate"
	"github.com/tingtt/agentsctl/internal/session"
	"github.com/tingtt/agentsctl/internal/sessionctl"
)

// CatalogStore persists ChatGPT's last-known-good catalog across process
// restarts, keyed by Project ID so catalogs from different configured
// Projects never cross (see (*localstate.Store).ChatGPTCatalog/
// SaveChatGPTCatalog, which satisfies this interface in production --
// tests use a fake). It is a small, consumer-side interface: Provider
// depends on this shape, never on localstate's own concrete Store or its
// private raw schema. A nil CatalogStore is valid and means Provider
// behaves as if persistence were simply absent -- nothing hydrated at
// construction, nothing saved after a refresh -- never a required
// dependency.
type CatalogStore interface {
	ChatGPTCatalog(projectID string) (localstate.ChatGPTCatalog, bool, error)
	SaveChatGPTCatalog(projectID string, catalog localstate.ChatGPTCatalog) error
}

// Provider lists and opens conversations from one configured ChatGPT
// Project. Browser authentication and undocumented endpoint behavior stay
// behind its private runtime.
//
// List is cheap: it serves catalogCache's last-known-good snapshot rather
// than performing a full browser cursor enumeration on the caller's
// critical path. The cache is replaced only by a COMPLETE background
// enumeration (see Refresh/runRefresh and catalogCache's doc comment) --
// scheduled, coalesced, and single-flighted by the refresh state machine
// below -- and published to any Observer subscriber as a full-replacement
// ProviderUpdate. The in-memory cache is additionally hydrated from, and
// kept durable through, CatalogStore -- see New/hydrate and runRefresh --
// so a restarted process can immediately serve last-known-good rows
// before any browser discovery even starts.
type Provider struct {
	config    Config
	browser   browser
	store     CatalogStore
	configErr error

	cache catalogCache

	// lifecycleCtx/lifecycleCancel bound every background refresh this
	// Provider ever starts and every Observe subscription's own watch
	// goroutine -- deliberately NOT derived from whatever ctx a caller
	// happens to pass into List/Refresh/Observe (typically Agent View's
	// per-reload-cycle context), since a ChatGPT refresh has independent
	// value beyond the Agent View reload generation that happened to
	// trigger it (see the DesignDoc's reload-generation vs. provider-
	// refresh lifetime separation). Only Close ends it. Lazily
	// initialized under mu so a Provider built by a test as a bare struct
	// literal (bypassing New) still works.
	lifecycleCtx    context.Context
	lifecycleCancel context.CancelFunc

	// mu guards the single-flight refresh state machine: refreshing is
	// true for the duration of exactly one in-flight runRefresh call;
	// pending records that another Refresh was requested while it ran, so
	// exactly one more refresh follows immediately after (coalescing --
	// see the DesignDoc's suggested refresh state machine). refreshCancel
	// cancels only the currently in-flight refresh (used by Close).
	mu            sync.Mutex
	refreshing    bool
	pending       bool
	refreshCancel context.CancelFunc

	// subMu guards subscribers, the set of live Observe channels.
	subMu       sync.Mutex
	subscribers map[chan sessionctl.ProviderUpdate]struct{}
}

var (
	_ sessionctl.Source    = (*Provider)(nil)
	_ sessionctl.Opener    = (*Provider)(nil)
	_ sessionctl.Observer  = (*Provider)(nil)
	_ sessionctl.Refresher = (*Provider)(nil)
)

// New returns a provider for config using the production browser runtime,
// hydrating its in-memory cache from store's persisted catalog (if any)
// for config.ProjectID before returning -- see hydrate. store may be nil.
func New(config Config, store CatalogStore) *Provider {
	p := &Provider{config: config, browser: newRuntime(), store: store}
	p.lifecycleCtx, p.lifecycleCancel = context.WithCancel(context.Background())
	p.hydrate()
	return p
}

// NewUnavailable returns a provider whose List reports configurationError.
// Registering it preserves provider-level failure isolation: Agent View can
// warn about the explicit ChatGPT configuration while healthy providers load.
func NewUnavailable(configurationError error) *Provider {
	p := &Provider{configErr: configurationError}
	p.lifecycleCtx, p.lifecycleCancel = context.WithCancel(context.Background())
	return p
}

// ID returns the stable ChatGPT provider identity.
func (*Provider) ID() session.ProviderID { return session.ProviderChatGPT }

// List is a pure cache read: it returns catalogCache's current snapshot
// immediately and never itself performs a full browser cursor enumeration
// or otherwise initiates remote work (see Provider's doc comment). If no
// successful enumeration has completed yet (and no persisted catalog was
// hydrated at construction -- see hydrate), it returns an empty,
// successful snapshot, never a remote-failure error: absence of a cache
// is not the same as a failed refresh.
//
// List deliberately does NOT trigger a background Refresh as a side
// effect. Initial-refresh ownership belongs entirely to
// sessionctl.Refresher: Agent View's own reload cycle already calls
// Refresh independently of List (see agentview.Runtime.requestReload), so
// a List-triggered Refresh here would only race/duplicate that call --
// harmless under single-flight, but it would still coalesce into one
// unnecessary extra enumeration immediately following the first. The
// eventual refresh result -- populated rows or a warning -- arrives
// through Observe.
func (p *Provider) List(ctx context.Context, archived bool) ([]session.Session, error) {
	if p.configErr != nil {
		return nil, p.configErr
	}
	if archived {
		return nil, nil
	}
	if p.browser == nil {
		return nil, fmt.Errorf("ChatGPT browser runtime is not configured")
	}
	sessions, hasSnapshot := p.cache.snapshot()
	if !hasSnapshot {
		return []session.Session{}, nil
	}
	return sessions, nil
}

// Refresh implements sessionctl.Refresher: it schedules a background
// catalog enumeration and returns immediately. Concurrent/rapid requests
// are single-flighted -- a refresh already running is left to finish, with
// at most one more queued immediately after it (see the mu/refreshing/
// pending fields' doc comment) -- so repeated Ctrl+L never spawns
// concurrent enumerations or tears down and restarts the discovery bridge.
func (p *Provider) Refresh(context.Context) {
	if p.configErr != nil || p.browser == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ensureLifecycleLocked()
	if p.lifecycleCtx.Err() != nil {
		return
	}
	if p.refreshing {
		p.pending = true
		return
	}
	p.startRefreshLocked()
}

// startRefreshLocked must be called with mu held and refreshing == false.
func (p *Provider) startRefreshLocked() {
	ctx, cancel := context.WithCancel(p.lifecycleCtx)
	p.refreshing = true
	p.refreshCancel = cancel
	go p.runRefresh(ctx)
}

// runRefresh performs one full browser cursor enumeration and, only if it
// completes successfully, atomically replaces catalogCache, persists the
// new catalog through CatalogStore (if configured), and publishes the new
// catalog to every Observer subscriber. Any failure (timeout, broken
// bridge, ambiguous request series, schema drift, cursor cycle, incomplete
// pagination, context cancellation -- see browser.List/enumerate) leaves
// the memory cache and persisted catalog exactly as they were and instead
// publishes (and records) the failure, so cached rows always survive a
// failed refresh. On completion it starts exactly one more refresh if
// Refresh was requested again while this one ran (coalescing).
//
// A persistence write failure (see persist) never un-does the memory
// replacement above it or suppresses the success publication: the fresh
// remote result is valid regardless of local durability, and is never
// discarded or hidden on account of a disk error. It is recorded on the
// cache (catalogCache.lastErr) for internal/future diagnostic use, but
// deliberately does not ride along on this cycle's Observer publication --
// sessionctl.ProviderUpdate's Sessions-xor-Err shape has no way to express
// "these sessions are valid AND there's also a durability warning" without
// widening that contract for every Observer provider, which durability
// alone does not justify.
func (p *Provider) runRefresh(ctx context.Context) {
	conversations, err := p.browser.List(ctx, p.config.ProjectID)
	if err != nil {
		p.cache.recordErr(err)
		p.publish(sessionctl.ProviderUpdate{Err: err})
	} else {
		sessions := toSessions(conversations, p.config.Root)
		refreshedAt := time.Now()
		p.cache.replaceAt(sessions, refreshedAt)
		p.persist(conversations, refreshedAt)
		p.publish(sessionctl.ProviderUpdate{Sessions: sessions})
	}

	p.mu.Lock()
	p.refreshing = false
	p.refreshCancel = nil
	again := p.pending && p.lifecycleCtx.Err() == nil
	p.pending = false
	if again {
		p.startRefreshLocked()
	}
	p.mu.Unlock()
}

// Observe implements sessionctl.Observer: the channel receives one
// ProviderUpdate for every completed cache replacement (Err == nil,
// Sessions == the new full cache) and one for every refresh failure (Err
// != nil, Sessions == nil -- see sessionctl.ProviderUpdate's doc comment
// on why a consumer must retain its own last-known Sessions in that case).
// It closes when ctx ends or the Provider is closed.
func (p *Provider) Observe(ctx context.Context) <-chan sessionctl.ProviderUpdate {
	// Buffered 1, not more: with latest-wins publish (see sendLatest),
	// this single slot always holds exactly the newest not-yet-consumed
	// update -- a bigger buffer would only let more stale history
	// accumulate behind the latest value, never anything useful.
	ch := make(chan sessionctl.ProviderUpdate, 1)

	p.mu.Lock()
	p.ensureLifecycleLocked()
	lifecycleCtx := p.lifecycleCtx
	p.mu.Unlock()

	if lifecycleCtx.Err() != nil {
		close(ch)
		return ch
	}

	p.subMu.Lock()
	if p.subscribers == nil {
		p.subscribers = make(map[chan sessionctl.ProviderUpdate]struct{})
	}
	p.subscribers[ch] = struct{}{}
	p.subMu.Unlock()

	go func() {
		select {
		case <-ctx.Done():
		case <-lifecycleCtx.Done():
		}
		p.subMu.Lock()
		if _, ok := p.subscribers[ch]; ok {
			delete(p.subscribers, ch)
			close(ch)
		}
		p.subMu.Unlock()
	}()
	return ch
}

// publish delivers update to every live subscriber using latest-wins
// semantics (see sendLatest): it never blocks cache replacement on a slow
// consumer, and a subscriber that falls behind converges on the newest
// provider state rather than working through a backlog of superseded
// ones. This matters concretely: an Observer publication is a full
// replacement of "the provider's current state", not an event in a log
// (see sessionctl.Observer's doc comment) -- so a failure queued behind a
// not-yet-delivered success (or vice versa) must never win over
// whichever actually reflects the provider's current cache once the
// subscriber catches up.
func (p *Provider) publish(update sessionctl.ProviderUpdate) {
	p.subMu.Lock()
	defer p.subMu.Unlock()
	for ch := range p.subscribers {
		sendLatest(ch, update)
	}
}

// sendLatest enqueues update as ch's one pending value, discarding
// whatever stale update (if any) is already queued and not yet consumed
// by dequeuing it first. The caller must hold a lock (subMu) that also
// guards ch's removal/close, so this can never race a concurrent close of
// ch, and must be the only writer to ch, so this loop is guaranteed to
// terminate: a concurrent reader can only ever make room, never take it
// away, between this function's two non-blocking selects.
func sendLatest(ch chan sessionctl.ProviderUpdate, update sessionctl.ProviderUpdate) {
	for {
		select {
		case ch <- update:
			return
		default:
		}
		select {
		case <-ch:
		default:
		}
	}
}

// ensureLifecycleLocked must be called with mu held. It lazily
// initializes lifecycleCtx/lifecycleCancel for a Provider built directly
// as a struct literal (as unit tests in this package do) instead of via
// New/NewUnavailable.
func (p *Provider) ensureLifecycleLocked() {
	if p.lifecycleCtx == nil {
		p.lifecycleCtx, p.lifecycleCancel = context.WithCancel(context.Background())
	}
}

// Open displays the official ChatGPT UI for s and returns when the browser
// view closes. Closing the view does not stop or mutate the cloud session.
// A cached session is always a valid Open target: Open never consults
// catalogCache or waits on any in-flight refresh (see browser.runtime's
// own List/Open locking, which keeps the two independent) -- if the
// conversation was deleted remotely after it entered the cache, the
// official ChatGPT UI determines that outcome, not this provider.
func (p *Provider) Open(ctx context.Context, s session.Session, in *os.File, out io.Writer) error {
	if p.configErr != nil {
		return p.configErr
	}
	if s.Key.Provider != session.ProviderChatGPT {
		return fmt.Errorf("cannot open non-ChatGPT session %s", s.Key)
	}
	if p.browser == nil {
		return fmt.Errorf("ChatGPT browser runtime is not configured")
	}
	return p.browser.Open(ctx, s.Key.ID, in, out)
}

// Close stops any in-flight/pending refresh, closes every Observer
// subscription, then stops the provider-owned discovery helper and
// removes its temporary bridge assets. It does not alter any ChatGPT
// conversation.
func (p *Provider) Close() error {
	p.mu.Lock()
	p.ensureLifecycleLocked()
	if p.refreshCancel != nil {
		p.refreshCancel()
	}
	p.pending = false
	p.lifecycleCancel()
	p.mu.Unlock()

	if p.browser == nil {
		return nil
	}
	return p.browser.Close()
}

// hydrate loads store's persisted catalog for p.config.ProjectID (if any)
// and installs it as catalogCache's initial snapshot, entirely from local
// state -- no browser process, network, or authentication is touched.
// Called once, synchronously, from New, before the Provider is ever
// registered or returns to its caller, so the very first List already
// has rows to serve (see the DesignDoc's "cached rows before remote
// refresh" startup guarantee) without waiting on the background refresh
// Agent View separately triggers via Refresh.
//
// A store read/decode error or an invalid persisted row (see
// sessionsFromPersisted) is recorded on the cache for diagnostics but
// does not prevent Provider from working: it simply starts with an empty
// cache, exactly as if nothing had ever been persisted, and the normal
// background refresh path takes over from there.
func (p *Provider) hydrate() {
	if p.store == nil {
		return
	}
	persisted, ok, err := p.store.ChatGPTCatalog(p.config.ProjectID)
	if err != nil {
		p.cache.recordErr(fmt.Errorf("load persisted ChatGPT catalog: %w", err))
		return
	}
	if !ok {
		return
	}
	sessions, err := sessionsFromPersisted(persisted, p.config.Root)
	if err != nil {
		p.cache.recordErr(fmt.Errorf("persisted ChatGPT catalog is invalid: %w", err))
		return
	}
	p.cache.replaceAt(sessions, persisted.RefreshedAt)
}

// persist saves conversations (from a just-COMPLETED enumeration -- see
// runRefresh, the only caller) to p.store under p.config.ProjectID. A
// nil store is a no-op (persistence is optional -- see CatalogStore's doc
// comment); a write failure is recorded on the cache but otherwise
// ignored here -- see runRefresh's doc comment on why it must never
// affect the memory cache or Observer publication already committed to
// for this refresh.
func (p *Provider) persist(conversations []conversation, refreshedAt time.Time) {
	if p.store == nil {
		return
	}
	if err := p.store.SaveChatGPTCatalog(p.config.ProjectID, toPersistedCatalog(conversations, refreshedAt)); err != nil {
		p.cache.recordErr(fmt.Errorf("persist ChatGPT catalog: %w", err))
	}
}

// sessionsFromPersisted validates and converts a persisted catalog into
// the provider-neutral session.Session shape, rooted at root -- root is
// always the CURRENT Config.Root, deliberately never anything persisted
// (see localstate.ChatGPTConversation's doc comment: only remote metadata
// is persisted, never a local CWD), so a moved checkout or a second local
// clone of the same Project hydrates with today's logical CWD, not a
// stale one.
//
// Persisted data is local state that may be stale or corrupt (hand-edited,
// written by a future/older agentsctl build, or otherwise malformed); a
// single invalid row fails the whole load rather than silently admitting
// a partially-corrupt catalog with e.g. an unparseable conversation ID
// into a session.Key.
func sessionsFromPersisted(catalog localstate.ChatGPTCatalog, root string) ([]session.Session, error) {
	conversations := make([]conversation, len(catalog.Conversations))
	for i, item := range catalog.Conversations {
		if !conversationIDPattern.MatchString(item.ID) {
			return nil, fmt.Errorf("persisted conversation %d has an invalid ID", i)
		}
		if strings.TrimSpace(item.Title) == "" {
			return nil, fmt.Errorf("persisted conversation %d has no title", i)
		}
		if item.CreatedAt.IsZero() || item.UpdatedAt.IsZero() {
			return nil, fmt.Errorf("persisted conversation %d has an invalid timestamp", i)
		}
		conversations[i] = conversation{ID: item.ID, Title: item.Title, CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt}
	}
	return toSessions(conversations, root), nil
}

// toPersistedCatalog converts a COMPLETE enumeration's conversations into
// CatalogStore's persisted shape -- remote metadata only (see
// localstate.ChatGPTConversation's doc comment), never local CWD/Actions/
// Pinned/Runtime/Activity, which are reconstructed by normal domain rules
// on every hydrate/List instead of being persisted.
func toPersistedCatalog(conversations []conversation, refreshedAt time.Time) localstate.ChatGPTCatalog {
	items := make([]localstate.ChatGPTConversation, len(conversations))
	for i, c := range conversations {
		items[i] = localstate.ChatGPTConversation{ID: c.ID, Title: c.Title, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt}
	}
	return localstate.ChatGPTCatalog{RefreshedAt: refreshedAt, Conversations: items}
}

// toSessions normalizes browser-enumerated conversations into the
// provider-neutral session.Session shape. ChatGPT remote star/pin fields
// never enter this representation; the controller overlays agentsctl's
// local pin store after provider listing.
func toSessions(conversations []conversation, root string) []session.Session {
	rows := make([]session.Session, 0, len(conversations))
	for _, item := range conversations {
		rows = append(rows, session.Session{
			Key:       session.Key{Provider: session.ProviderChatGPT, ID: item.ID},
			Name:      item.Title,
			CWD:       root,
			CreatedAt: item.CreatedAt,
			UpdatedAt: item.UpdatedAt,
			Activity:  session.ActivityUnknown,
			Runtime:   session.RuntimeNone,
			Archived:  false,
			Actions: session.Actions{
				session.ActionOpen: {Available: true},
			},
		})
	}
	return rows
}

// catalogCache is ChatGPT's last-known-good in-memory catalog: replaced
// only by a COMPLETE background enumeration (see Provider.runRefresh),
// never by a partial or failed one. It is memory-only for the life of one
// Provider/process -- nothing here persists to disk.
type catalogCache struct {
	mu          sync.RWMutex
	sessions    []session.Session
	hasSnapshot bool
	refreshedAt time.Time
	lastErr     error
}

// snapshot returns an independent copy of the current cache -- never a
// reference into the cache's own backing slice, so a caller mutating the
// result (or a later replace call) can never corrupt the other's view.
// The second return reports whether any successful enumeration has ever
// completed.
func (c *catalogCache) snapshot() ([]session.Session, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if !c.hasSnapshot {
		return nil, false
	}
	out := make([]session.Session, len(c.sessions))
	copy(out, c.sessions)
	return out, true
}

// replace is replaceAt(sessions, time.Now()) -- the common case, a fresh
// successful refresh. See replaceAt.
func (c *catalogCache) replace(sessions []session.Session) {
	c.replaceAt(sessions, time.Now())
}

// replaceAt atomically installs sessions as the new last-known-good
// catalog with an explicit refreshedAt (hydrate passes the persisted
// catalog's own original refresh time rather than time.Now(), since
// hydration itself is not a refresh), copying its input so the caller's
// own slice (and any earlier snapshot still held by another goroutine) is
// never shared with -- or later mutated through -- the cache's backing
// storage. It also clears any previously recorded error: a successful
// replacement (or valid hydration) supersedes it.
func (c *catalogCache) replaceAt(sessions []session.Session, refreshedAt time.Time) {
	cp := make([]session.Session, len(sessions))
	copy(cp, sessions)
	c.mu.Lock()
	c.sessions = cp
	c.hasSnapshot = true
	c.refreshedAt = refreshedAt
	c.lastErr = nil
	c.mu.Unlock()
}

// recordErr records a failed refresh's error without touching sessions or
// hasSnapshot -- the defining last-known-good guarantee: a failed refresh
// never evicts or overwrites whatever the cache already held.
func (c *catalogCache) recordErr(err error) {
	c.mu.Lock()
	c.lastErr = err
	c.mu.Unlock()
}
