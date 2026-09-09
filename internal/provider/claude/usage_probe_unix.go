//go:build darwin || linux

package claude

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/tingtt/agentsctl/internal/provider/claude/probestate"
	"github.com/tingtt/agentsctl/internal/session"
)

// usageProbeTTL bounds how long a cached Claude usage snapshot is served
// before Usage() triggers a fresh refresh through the owned probe session
// (see Probe.Usage) -- #14's "1〜5分程度" cache freshness guidance. Also
// the freshness window refreshCoordinator.Refresh re-checks the persisted
// snapshot against after acquiring the cross-process refresh lock -- the
// same TTL policy (probestate.SnapshotFresh), not a second one (see
// refreshCoordinator.Refresh's own doc comment).
const usageProbeTTL = 3 * time.Minute

// usageProbeRefreshLockPollInterval is the retry cadence refreshCoordinator.lock
// polls at while waiting for another process's refresh transaction to
// release the cross-process refresh lock. Unlike probestate.IdentityStore's
// plain blocking unix.Flock (an identity transaction is always short --
// one local read plus one atomic write), a refresh transaction can hold
// this lock for as long as a full Claude round trip takes, so a
// context-cancellable wait matters here: LOCK_EX|LOCK_NB polled at this
// interval, rather than a single blocking LOCK_EX, so Usage(ctx)'s own
// cancellation semantics are never made worse by this lock's existence.
const usageProbeRefreshLockPollInterval = 50 * time.Millisecond

// usageProbePrompt is the minimal message probeAttemptRunner.Run sends into the
// probe session to elicit one real API response (the only way Claude Code
// populates rate_limits for its statusLine -- see UsageProbeSource's doc
// comment). It deliberately asks for no tool use: this session has no
// human watching its PTY to approve a permission prompt, and any it
// spawned would simply hang until usageProbeSendTimeout.
const usageProbePrompt = "Reply with just the word OK. Do not use any tools."

// usageProbeSendTimeout bounds how long probeAttemptRunner.Run waits for the
// collector to observe a fresh statusLine snapshot (ObservedAt after the
// prompt was sent) before giving up on this refresh.
const usageProbeSendTimeout = 30 * time.Second

// usageProbePollInterval is the polling cadence for waitForFreshSnapshot.
const usageProbePollInterval = 250 * time.Millisecond

// usageProbeSettleDelay is a deliberate wait between starting the probe
// session's process and writing anything to it -- unlike sendClaudeRename's
// write-immediately design (verified, by real-CLI measurement, safe
// against `claude attach <id>`: attaching to an already-running
// background session), this starts a brand-new interactive session from
// cold (auth, model/session setup, and only then its own raw-mode
// terminal setup). Verified against the installed CLI: writing
// immediately after start is unreliable here (input can arrive before the
// session's own terminal-mode setup and be silently dropped), while
// waiting this long consistently reaches an input-ready state.
const usageProbeSettleDelay = 3 * time.Second

// usageProbeTrustDialogAccept is the raw byte sequence sent to accept
// Claude Code's workspace-trust confirmation dialog -- shown the first
// time any interactive session runs in a directory Claude hasn't seen
// before (this probe's own dedicated app-data directory, on its very
// first-ever launch): Down arrow moves the selection off its own default
// ("No, exit") onto "Yes, I trust this folder", and Enter confirms it.
// Verified against the installed CLI: sending this once, then a real
// prompt, completes a real chat turn, and Claude Code durably remembers
// the directory as trusted in its own local state from then on (see
// probestate.Identity.TrustAccepted's doc comment for why this is sent at
// most once per probe identity).
const usageProbeTrustDialogAccept = "\x1b[B\r"

// usageProbeTrustSettleDelay is a second, shorter settle wait after
// answering the trust dialog and before sending the real prompt -- the
// dialog's answer needs to be processed and the session's chat composer
// needs to actually be ready before the prompt is treated as this
// session's first turn (see waitForFreshSnapshot's use of sentAt).
const usageProbeTrustSettleDelay = 2 * time.Second

// usageProbeDetachSettleDelay is a wait, after the probe's own turn has
// completed (successfully or not), before asking the session to exit.
// Verified against the installed CLI: a SIGINT sent immediately after a
// turn completes does not reliably exit the process within a few
// seconds, while one sent to an otherwise-idle session does -- this gives
// the session's own TUI a moment to settle back to idle first.
const usageProbeDetachSettleDelay = 2 * time.Second

// usageProbeDetachTimeout bounds how long probeAttemptRunner.detach waits
// for a SIGINT-requested clean exit before escalating to SIGKILL.
const usageProbeDetachTimeout = 5 * time.Second

// Probe is agentsctl's one owned Claude usage-probe session: an
// interactive `claude` process, run under a PTY this package owns
// transiently for each refresh (never left running in the background --
// see usage_attempt.go's probeAttemptRunner.Run), configured via a dedicated --settings file whose
// statusLine points back at this same executable's UsageCollectorCommand.
// It implements UsageProbeSource for Provider.
//
// A Probe is safe for concurrent use: Usage single-flights refreshes (see
// refreshShared) so concurrent callers within this process never spawn
// more than one probe process or send more than one prompt at a time.
// That guarantee extends across every agentsctl process sharing this
// probe's Dir too, not just within one -- see refreshCoordinator.
type Probe struct {
	// Path is the `claude` binary path; empty means "claude" (resolved via
	// PATH, matching Provider.path()).
	Path string
	// Dir is this probe's dedicated app-data directory (never a user
	// project directory): it holds the probe's identity, its dedicated
	// Claude settings, and the persisted usage snapshot.
	Dir string
	// ExePath overrides how refresh resolves "this agentsctl executable"
	// for the generated statusLine command; empty uses os.Executable().
	// Exposed for tests, which run as a `go test` binary rather than the
	// real one.
	ExePath string
	// SettleDelay, TrustSettleDelay, and DetachSettleDelay override
	// refresh's real-CLI-measured settle waits (see the usageProbe*Delay
	// constants); zero uses the documented production default for each.
	// Exposed so a test driving a fake CLI (whose own startup/settle
	// timing is trivial, unlike the real `claude` binary this package was
	// tuned against) isn't forced to wait out real-CLI-scaled delays.
	SettleDelay       time.Duration
	TrustSettleDelay  time.Duration
	DetachSettleDelay time.Duration
	// SendTimeout overrides usageProbeSendTimeout, the bound
	// waitForProbeOutcome waits for either a genuine fresh snapshot or a
	// classified usage-limit indication before giving up with a plain
	// (non-limit) failure; zero uses the production default. Exposed so a
	// test exercising that plain-failure path (see Issue #19's "limit 以外
	// の timeout" regression test) isn't forced to wait out the real
	// production timeout.
	SendTimeout time.Duration
	// Clock overrides "now" for reset-boundary decisions (see now,
	// toSessionUsage, exhaustedSnapshot); nil uses time.Now. Exposed so a
	// test can deterministically cross a cached window's own Reset time
	// without sleeping (see Issue #19's reset-boundary regression tests).
	// TTL freshness (cachedFresh/refreshCoordinator's persisted re-check) does NOT go
	// through Clock -- see the DesignDoc's "Clock consistency" note: TTL
	// freshness is a real-wall-clock comparison against when a refresh
	// actually happened, not a domain decision a test needs to control
	// independently of wall time.
	Clock func() time.Time

	mu          sync.Mutex
	snapshot    probestate.Snapshot
	hasSnapshot bool
	snapshotAt  time.Time
	refreshCh   chan struct{} // non-nil while a refresh is in flight (single-flight)
}

// NewProbe returns a Probe backed by dir (created lazily on first refresh).
func NewProbe(path, dir string) *Probe {
	return &Probe{Path: path, Dir: dir}
}

func (pr *Probe) claudePath() string {
	if pr.Path != "" {
		return pr.Path
	}
	return "claude"
}
func (pr *Probe) settleDelay() time.Duration {
	if pr.SettleDelay > 0 {
		return pr.SettleDelay
	}
	return usageProbeSettleDelay
}
func (pr *Probe) trustSettleDelay() time.Duration {
	if pr.TrustSettleDelay > 0 {
		return pr.TrustSettleDelay
	}
	return usageProbeTrustSettleDelay
}
func (pr *Probe) detachSettleDelay() time.Duration {
	if pr.DetachSettleDelay > 0 {
		return pr.DetachSettleDelay
	}
	return usageProbeDetachSettleDelay
}
func (pr *Probe) sendTimeout() time.Duration {
	if pr.SendTimeout > 0 {
		return pr.SendTimeout
	}
	return usageProbeSendTimeout
}
func (pr *Probe) now() time.Time {
	if pr.Clock != nil {
		return pr.Clock()
	}
	return time.Now()
}
func (pr *Probe) settingsPath() string { return filepath.Join(pr.Dir, "settings.json") }
func (pr *Probe) snapshotPath() string { return filepath.Join(pr.Dir, "usage.json") }
func (pr *Probe) identityPath() string { return filepath.Join(pr.Dir, "probe.json") }

// identityStore returns the Root Owner of this probe's identity
// (probe.json) -- cheap to construct (just wraps a path), so a fresh one
// is built on demand rather than cached as a field.
func (pr *Probe) identityStore() *probestate.IdentityStore {
	return probestate.NewIdentityStore(pr.identityPath())
}

// snapshotStore returns the Root Owner of this probe's persisted usage
// snapshot (usage.json) -- see identityStore's own doc comment.
func (pr *Probe) snapshotStore() *probestate.SnapshotStore {
	return probestate.NewSnapshotStore(pr.snapshotPath())
}

// newRefreshCoordinator builds this probe's machine-global refresh
// coordinator (see refreshCoordinator's own doc comment) from this
// Probe's current configuration -- cheap to construct, so a fresh one is
// built per refresh rather than cached as a field (matching
// identityStore/snapshotStore's own reasoning).
func (pr *Probe) newRefreshCoordinator() *refreshCoordinator {
	return &refreshCoordinator{
		dir:      pr.Dir,
		identity: pr.identityStore(),
		snapshot: pr.snapshotStore(),
		ttl:      usageProbeTTL,
		now:      pr.now,
		attempt:  pr.newAttemptRunner().Run,
	}
}

// newAttemptRunner builds this probe's single-attempt runner (see
// probeAttemptRunner's own doc comment) from this Probe's current
// configuration -- cheap to construct, so a fresh one is built per attempt
// rather than cached as a field.
func (pr *Probe) newAttemptRunner() *probeAttemptRunner {
	return &probeAttemptRunner{
		dir:               pr.Dir,
		claudePath:        pr.claudePath(),
		exePath:           pr.exePath,
		settingsPath:      pr.settingsPath(),
		snapshotPath:      pr.snapshotPath(),
		settleDelay:       pr.settleDelay(),
		trustSettleDelay:  pr.trustSettleDelay(),
		detachSettleDelay: pr.detachSettleDelay(),
		sendTimeout:       pr.sendTimeout(),
		identity:          pr.identityStore(),
	}
}

func (pr *Probe) exePath() (string, error) {
	if pr.ExePath != "" {
		return pr.ExePath, nil
	}
	return os.Executable()
}

// KnownSessionIDs implements UsageProbeSource: a pure local-file read (no
// process spawned, no catalog call), so Provider.List can cheaply exclude
// every row this probe has ever owned -- its current SessionID and every
// RetiredSessionIDs entry a rotation has left behind (see
// probestate.Identity's own doc comment) -- on every load. An identity
// that has never been created, or that can't be read, reports an
// empty/nil slice -- List then excludes nothing, never guessing (see
// Provider.List's doc comment on this call site).
func (pr *Probe) KnownSessionIDs() []string {
	return pr.identityStore().KnownSessionIDs()
}

// Usage implements UsageProbeSource. A fresh cached snapshot (within
// usageProbeTTL) is returned immediately with no process work at all --
// including one loaded lazily from this probe's own persisted usage.json
// (see loadPersistedSnapshotOnce), so a freshly-restarted agentsctl
// process reuses a still-fresh snapshot from a previous run instead of
// forcing an immediate cold probe refresh. A stale or absent cache
// triggers refreshShared; if that fails but a stale snapshot still
// exists, the stale snapshot is returned rather than failing the whole
// call -- Codex's usage row, and the session catalog itself, must never
// be taken down by a Claude-side hiccup (see the DesignDoc's Claude usage
// cache/failure policy). Only a refresh failure with no prior snapshot at
// all surfaces as an error, which sessionctl.Controller.Usage already
// treats as "omit this provider from the usage line" -- never a fake 0%.
//
// Every returned snapshot passes through toSessionUsage with the same
// "now" (see pr.now()), which independently normalizes each window
// against its own cached Reset time via session.Usage.At -- a window
// whose reset boundary has passed since it was cached renders as
// session.UsageUnknown here regardless of which of the three paths below
// produced the underlying snapshot (see Issue #19's reset-boundary
// handling). Agent View applies the exact same session.Usage.At rule
// again at its own read/render time, so a value already normalized here
// is never shown differently on the other side of that boundary.
func (pr *Probe) Usage(ctx context.Context) (session.Usage, error) {
	pr.loadPersistedSnapshotOnce()
	now := pr.now()
	if snap, ok := pr.cachedFresh(); ok {
		return toSessionUsage(snap, now), nil
	}
	snap, err := pr.refreshShared(ctx)
	if err == nil {
		return toSessionUsage(snap, now), nil
	}
	if stale, ok := pr.cachedAny(); ok {
		return toSessionUsage(stale, now), nil
	}
	return session.Usage{}, err
}

// loadPersistedSnapshotOnce lazily loads this probe's persisted
// usage.json into the in-memory cache the first time it's consulted -- a
// freshly-constructed Probe (e.g. right after an agentsctl restart)
// starts with hasSnapshot=false in memory even though a recent snapshot
// from a previous process may already be sitting on disk. It is a no-op
// once hasSnapshot is already true (whether from an earlier call to this
// method or a real refresh), so it can never clobber a fresher in-memory
// result with an older on-disk one, and a missing or unreadable
// usage.json is silently ignored (the normal cold-start case).
func (pr *Probe) loadPersistedSnapshotOnce() {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	if pr.hasSnapshot {
		return
	}
	if snap, ok, err := pr.snapshotStore().Load(); err == nil && ok {
		pr.snapshot, pr.hasSnapshot, pr.snapshotAt = snap, true, snap.ObservedAt
	}
}

// cachedFresh and cachedAny apply probestate.SnapshotFresh -- the single
// TTL freshness policy -- against the real wall clock (time.Now(), not
// pr.now()/pr.Clock): TTL freshness is "how long ago did a refresh
// actually complete", not a reset-boundary domain decision a test needs to
// control independently of wall time (see Probe.Clock's own doc comment).
func (pr *Probe) cachedFresh() (probestate.Snapshot, bool) {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	if pr.hasSnapshot && probestate.SnapshotFresh(pr.snapshotAt, time.Now(), usageProbeTTL) {
		return pr.snapshot, true
	}
	return probestate.Snapshot{}, false
}
func (pr *Probe) cachedAny() (probestate.Snapshot, bool) {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	return pr.snapshot, pr.hasSnapshot
}

// refreshShared single-flights refresh WITHIN THIS PROCESS: concurrent
// callers arriving while a refresh is already in progress wait for it
// (bounded by ctx) and reuse its result instead of starting a second
// probe process or prompt of their own, guaranteeing at most one refresh
// attempt in flight per process at a time, regardless of how many
// goroutines call Usage concurrently. It delegates the actual work to a
// fresh refreshCoordinator (see newRefreshCoordinator), which extends
// that same guarantee across every agentsctl process sharing this probe's
// Dir too -- see its own doc comment; this function's own responsibility
// ends at the process boundary, exactly like localstate.Store's
// in-process mu paired with its own cross-process flock. This function
// never itself acquires the refresh lock, checks persisted freshness, or
// persists a result -- see refreshCoordinator.Refresh's own doc comment
// for why that whole sequence is that type's alone to run.
func (pr *Probe) refreshShared(ctx context.Context) (probestate.Snapshot, error) {
	pr.mu.Lock()
	if ch := pr.refreshCh; ch != nil {
		pr.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return probestate.Snapshot{}, ctx.Err()
		}
		if snap, ok := pr.cachedFresh(); ok {
			return snap, nil
		}
		return probestate.Snapshot{}, errors.New("concurrent claude usage refresh did not produce a fresh snapshot")
	}
	ch := make(chan struct{})
	pr.refreshCh = ch
	pr.mu.Unlock()

	// prev feeds refreshCoordinator.resolveOutcome's exhausted-snapshot
	// merge if this refresh's own attempt detects a usage limit (see its
	// own doc comment) -- Probe's process-local cache is the only place
	// that knowledge lives, so it's read here and handed down rather than
	// the coordinator reaching back into Probe for it.
	prev, _ := pr.cachedAny()
	snap, err := pr.newRefreshCoordinator().Refresh(ctx, prev)

	pr.mu.Lock()
	if err == nil {
		pr.snapshot, pr.hasSnapshot, pr.snapshotAt = snap, true, time.Now()
	}
	pr.refreshCh = nil
	pr.mu.Unlock()
	close(ch)
	return snap, err
}

// toSessionUsage converts this package's own Claude-specific snapshot into
// the provider-neutral session.Usage -- the boundary past which no
// statusLine-shaped detail leaks (see UsageProbeSource's doc comment).
// The reset-boundary rule itself (Issue #19: a cached window's reading is
// only valid while now is still before its own Reset) is NOT duplicated
// here -- it is applied uniformly via session.Usage.At, the same
// provider-neutral method Agent View applies again at its own read/render
// time (see footer.go's usageWindowText), so a value already normalized
// here can never drift from how a later read of the same session.Usage
// treats it (see the DesignDoc's usage contract).
func toSessionUsage(snap probestate.Snapshot, now time.Time) session.Usage {
	u := session.Usage{
		Provider: session.ProviderClaude,
		FiveHour: toSessionUsageWindow(snap.FiveHour),
		Weekly:   toSessionUsageWindow(snap.Weekly),
	}
	return u.At(now)
}

// toSessionUsageWindow converts one cached window's shape into the
// provider-neutral session.UsageWindow, with no reset-boundary judgment
// of its own -- see toSessionUsage, which applies session.Usage.At right
// after calling this.
func toSessionUsageWindow(w probestate.WindowSnapshot) session.UsageWindow {
	return session.UsageWindow{State: w.State, Percent: w.Percent, Reset: w.ResetAt}
}
