//go:build darwin || linux

package claude

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/tingtt/agentsctl/internal/session"
)

// usageProbeTTL bounds how long a cached Claude usage snapshot is served
// before Usage() triggers a fresh refresh through the owned probe session
// (see Probe.Usage) -- #14's "1〜5分程度" cache freshness guidance. Also
// the freshness window refreshCrossProcess re-checks the persisted
// snapshot against after acquiring the cross-process refresh lock -- the
// same TTL policy, not a second one (see refreshCrossProcess's own doc
// comment).
const usageProbeTTL = 3 * time.Minute

// usageProbeRefreshLockPollInterval is the retry cadence Probe.lockRefresh
// polls at while waiting for another process's refresh transaction to
// release the cross-process refresh lock. Unlike lockProbeIdentityFile's
// plain blocking unix.Flock (an identity transaction is always short --
// one local read plus one atomic write), a refresh transaction can hold
// this lock for as long as a full Claude round trip takes, so a
// context-cancellable wait matters here: LOCK_EX|LOCK_NB polled at this
// interval, rather than a single blocking LOCK_EX, so Usage(ctx)'s own
// cancellation semantics are never made worse by this lock's existence.
const usageProbeRefreshLockPollInterval = 50 * time.Millisecond

// usageProbePrompt is the minimal message Probe.refreshOnce sends into the
// probe session to elicit one real API response (the only way Claude Code
// populates rate_limits for its statusLine -- see UsageProbeSource's doc
// comment). It deliberately asks for no tool use: this session has no
// human watching its PTY to approve a permission prompt, and any it
// spawned would simply hang until usageProbeSendTimeout.
const usageProbePrompt = "Reply with just the word OK. Do not use any tools."

// usageProbeSendTimeout bounds how long Probe.refreshOnce waits for the
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
// probeIdentity.TrustAccepted's doc comment for why this is sent at most
// once per probe identity).
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

// usageProbeDetachTimeout bounds how long detachProbeSession waits for a
// SIGINT-requested clean exit before escalating to SIGKILL.
const usageProbeDetachTimeout = 5 * time.Second

// Probe is agentsctl's one owned Claude usage-probe session: an
// interactive `claude` process, run under a PTY this package owns
// transiently for each refresh (never left running in the background --
// see refreshOnce), configured via a dedicated --settings file whose
// statusLine points back at this same executable's UsageCollectorCommand.
// It implements UsageProbeSource for Provider.
//
// A Probe is safe for concurrent use: Usage single-flights refreshes (see
// refreshShared) so concurrent callers within this process never spawn
// more than one probe process or send more than one prompt at a time.
// That guarantee extends across every agentsctl process sharing this
// probe's Dir too, not just within one -- see refreshCrossProcess.
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
	// Clock overrides "now" for cache-freshness and reset-boundary
	// decisions (see now, toSessionUsage, exhaustedSnapshot); nil uses
	// time.Now. Exposed so a test can deterministically cross a cached
	// window's own Reset time without sleeping (see Issue #19's reset-
	// boundary regression tests) -- time.Now is never called directly
	// anywhere else in this file for that purpose (see the DesignDoc's
	// usage contract).
	Clock func() time.Time

	mu          sync.Mutex
	snapshot    usageSnapshot
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

// refreshLockPath is the dedicated cross-process refresh lock (see
// refreshCrossProcess) -- deliberately its own file, never probe.json.lock
// itself: refreshOnce calls markTrustAccepted, which acquires
// probe.json.lock as its own short-lived identity transaction, so holding
// probe.json.lock across an entire refresh attempt (this lock's actual
// scope, potentially a full Claude round trip) would make that inner
// acquisition either deadlock against itself or require reentrant
// locking. Keeping the two locks -- and the two files backing them --
// entirely separate keeps the ordering strictly one-directional (refresh
// lock held first, identity lock acquired and released many times inside
// it, never the reverse) with no risk of lock-order inversion.
func (pr *Probe) refreshLockPath() string { return filepath.Join(pr.Dir, "refresh.lock") }

func (pr *Probe) exePath() (string, error) {
	if pr.ExePath != "" {
		return pr.ExePath, nil
	}
	return os.Executable()
}

// KnownSessionIDs implements UsageProbeSource: a pure local-file read (no
// process spawned, no catalog call), so Provider.List can cheaply exclude
// every row this probe has ever owned -- its current SessionID and every
// RetiredSessionIDs entry a rotation has left behind (see probeIdentity's
// own doc comment) -- on every load. An identity that has never been
// created, or that can't be read, reports an empty/nil slice -- List then
// excludes nothing, never guessing (see Provider.List's doc comment on
// this call site).
func (pr *Probe) KnownSessionIDs() []string {
	id, ok, err := readProbeIdentityIfExists(pr.identityPath())
	if err != nil || !ok {
		return nil
	}
	return id.allSessionIDs()
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
	if snap, ok, err := readUsageSnapshot(pr.snapshotPath()); err == nil && ok {
		pr.snapshot, pr.hasSnapshot, pr.snapshotAt = snap, true, snap.ObservedAt
	}
}

func (pr *Probe) cachedFresh() (usageSnapshot, bool) {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	if pr.hasSnapshot && time.Since(pr.snapshotAt) < usageProbeTTL {
		return pr.snapshot, true
	}
	return usageSnapshot{}, false
}
func (pr *Probe) cachedAny() (usageSnapshot, bool) {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	return pr.snapshot, pr.hasSnapshot
}

// refreshShared single-flights refresh WITHIN THIS PROCESS: concurrent
// callers arriving while a refresh is already in progress wait for it
// (bounded by ctx) and reuse its result instead of starting a second
// probe process or prompt of their own, guaranteeing at most one refresh
// attempt in flight per process at a time, regardless of how many
// goroutines call Usage concurrently. It delegates the actual work to
// refreshCrossProcess, which extends that same guarantee across every
// agentsctl process sharing this probe's Dir -- see its own doc comment;
// this function's own responsibility ends at the process boundary,
// exactly like localstate.Store's in-process mu paired with its own
// cross-process flock.
func (pr *Probe) refreshShared(ctx context.Context) (usageSnapshot, error) {
	pr.mu.Lock()
	if ch := pr.refreshCh; ch != nil {
		pr.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return usageSnapshot{}, ctx.Err()
		}
		if snap, ok := pr.cachedFresh(); ok {
			return snap, nil
		}
		return usageSnapshot{}, errors.New("concurrent claude usage refresh did not produce a fresh snapshot")
	}
	ch := make(chan struct{})
	pr.refreshCh = ch
	pr.mu.Unlock()

	snap, err := pr.refreshCrossProcess(ctx)

	pr.mu.Lock()
	if err == nil {
		pr.snapshot, pr.hasSnapshot, pr.snapshotAt = snap, true, time.Now()
	}
	pr.refreshCh = nil
	pr.mu.Unlock()
	close(ch)
	return snap, err
}

// refreshCrossProcess is refreshShared's cross-process counterpart:
// exactly one agentsctl process at a time, across every process sharing
// this probe's Dir, is ever inside refreshWithRecovery for this probe --
// enforced by holding the exclusive cross-process refresh lock (see
// lockRefresh) for this call's entire duration, from before the very
// first freshness check through the final persist. Without that lock,
// two processes racing a stale cache could each open their own
// `--session-id A` interactive session concurrently; the earlier rounds'
// own session-conflict-recovery machinery exists to survive Claude Code
// rejecting one of those two sessions, but it was never meant to be the
// steady-state outcome of agentsctl's own normal, non-adversarial
// operation -- an entirely avoidable self-inflicted conflict, an
// unnecessary duplicate live API call (consuming quota twice for what
// should be one refresh), and duplicate statusLine/collector work, all
// for a result the second process didn't actually need.
//
// Once the lock is held, the persisted usage.json is re-read and checked
// against the exact same usageProbeTTL freshness window Probe.cachedFresh
// already applies to the in-memory cache -- no separate or looser
// freshness rule. This is what turns the lock from "just serialize
// refreshes" into "actually skip the second one": a caller that had to
// wait for the lock, arriving here right after another process's own
// refreshCrossProcess call already persisted a fresh result, returns that
// persisted snapshot directly, never touching Claude at all. A caller
// that acquires the lock uncontended, or whose wait ends before anyone
// else's refresh has produced anything fresh, falls through to a real
// refreshWithRecovery attempt exactly as before.
//
// The persist-on-success step (writeUsageSnapshotAtomic) deliberately
// stays inside the locked section too, not after it: releasing the lock
// before persisting would reopen the exact window this function exists to
// close, letting a second process's own freshness re-check run against a
// still-stale usage.json and duplicate the just-completed refresh. A
// write failure here is NOT escalated into this call's own error --
// matching the DesignDoc's existing policy that an otherwise-valid
// refresh result must survive a persistence-layer hiccup -- but it does
// mean this specific dedupe guarantee is lost for whichever other
// process's own wait happens to end before some later successful write
// finally lands: that process will see a still-stale (or absent)
// usage.json under the lock and, correctly by its own local information,
// perform its own redundant refresh. This is a narrow, honestly-documented
// gap, not a silently-assumed one -- session ID conflict recovery (see
// refreshWithRecovery) remains available to both processes regardless.
func (pr *Probe) refreshCrossProcess(ctx context.Context) (usageSnapshot, error) {
	unlock, err := pr.lockRefresh(ctx)
	if err != nil {
		return usageSnapshot{}, fmt.Errorf("acquire claude usage probe refresh lock: %w", err)
	}
	defer unlock()

	if snap, ok := pr.persistedFresh(); ok {
		return snap, nil
	}

	snap, err := pr.refreshWithRecovery(ctx)
	if err != nil {
		return usageSnapshot{}, err
	}
	_ = writeUsageSnapshotAtomic(pr.snapshotPath(), snap) // see this function's own doc comment for why a write failure here is deliberately not returned as this call's error
	return snap, nil
}

// persistedFresh reads this probe's persisted usage.json and reports it
// (via ok) only if it's within usageProbeTTL of now -- the exact same
// freshness rule Probe.cachedFresh applies to the in-memory cache, read
// here from disk instead so refreshCrossProcess can tell whether another
// process's refresh, completed while this call was waiting for the lock,
// already makes a fresh Claude call unnecessary.
func (pr *Probe) persistedFresh() (usageSnapshot, bool) {
	snap, ok, err := readUsageSnapshot(pr.snapshotPath())
	if err != nil || !ok {
		return usageSnapshot{}, false
	}
	if time.Since(snap.ObservedAt) >= usageProbeTTL {
		return usageSnapshot{}, false
	}
	return snap, true
}

// lockRefresh acquires the exclusive cross-process refresh lock guarding
// refreshCrossProcess (see its own doc comment and refreshLockPath's),
// honoring ctx cancellation while waiting -- unlike
// lockProbeIdentityFile's single blocking unix.Flock call (safe there
// because an identity transaction never holds its lock for more than one
// local read plus one atomic write), the caller here can hold this lock
// for as long as a full Claude round trip takes, so Usage(ctx)'s own
// cancellation contract has to keep working while waiting for it: a
// non-blocking LOCK_EX|LOCK_NB attempt, polled at
// usageProbeRefreshLockPollInterval, rather than one call that could
// block past ctx's own deadline with no way to interrupt it.
func (pr *Probe) lockRefresh(ctx context.Context) (func(), error) {
	if err := os.MkdirAll(pr.Dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(pr.refreshLockPath(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		if err := ctx.Err(); err != nil {
			f.Close()
			return nil, err
		}
		err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return func() { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN); _ = f.Close() }, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			f.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(usageProbeRefreshLockPollInterval):
		}
	}
}

// refreshWithRecovery runs refreshOnce, self-healing exactly one specific
// failure mode -- errProbeSessionConflict -- within this single call,
// entirely inside the Claude provider boundary: Agent View, sessionctl,
// and every other caller of Probe.Usage just see one refresh that either
// succeeded or failed, never Claude-specific recovery orchestration (see
// Issue #19's follow-up: refresh's own background fetch is not a polling
// loop, so recovering only "on the next refresh" could otherwise leave a
// user staring at a stale/"?%" reading until whatever next triggers a
// fetch).
//
// At most one rotation/recovery attempt and one retry ever happen --
// never a loop: a second errProbeSessionConflict (or any other error)
// from the retry is returned exactly as refreshOnce reported it, falling
// through to refreshShared's/Usage's existing stale-cache-or-error
// handling unchanged. If rotateProbeIdentity itself fails (a persistence
// error, not a conflict), the original conflict error is returned rather
// than attempting a retry with no valid identity to use. This bound holds
// regardless of which of rotateProbeIdentity's two outcomes occurs below
// (see its own doc comment) -- a cross-process recovery that mints no new
// identity still counts as this call's one allowed retry, never another
// round of its own.
//
// rotateProbeIdentity is deliberately passed id.SessionID -- the specific
// identity THIS attempt just saw rejected -- rather than being called with
// no argument of its own, and re-reads the persisted file itself rather
// than rotating from the `id` this function loaded before calling
// refreshOnce. Two races this guards against, both requiring the current
// on-disk record rather than this function's own copy:
//
//   - refreshOnce can itself durably persist a TrustAccepted:true partway
//     through the very attempt that goes on to hit a conflict (a conflict
//     surfacing right after the workspace-trust dialog was just
//     answered); rotating from this function's own now-possibly-stale
//     `id` copy instead of the current on-disk record would silently lose
//     that update.
//   - Multiple agentsctl processes can share the same probe.json. Another
//     process may have already rotated this exact rejected ID away before
//     this call gets here; rotateProbeIdentity detects that by comparing
//     id.SessionID against what it reads as current from inside its own
//     withProbeIdentityLock transaction -- not just "fresher than this
//     function's copy" but genuinely race-free, since no other process's
//     own rotation transaction can be interleaved with this comparison --
//     and hands that already-rotated identity back instead of minting a
//     redundant one of its own (see its own doc comment's "cross-process
//     race").
//
// See rotateProbeIdentity's own doc comment for the full detail on both.
func (pr *Probe) refreshWithRecovery(ctx context.Context) (usageSnapshot, error) {
	id, err := loadOrCreateProbeIdentity(pr.identityPath())
	if err != nil {
		return usageSnapshot{}, fmt.Errorf("load claude usage probe identity: %w", err)
	}
	snap, err := pr.refreshOnce(ctx, id)
	if err == nil || !errors.Is(err, errProbeSessionConflict) {
		return snap, err
	}
	recovery, rerr := rotateProbeIdentity(pr.identityPath(), id.SessionID)
	if rerr != nil {
		return usageSnapshot{}, err
	}
	return pr.refreshOnce(ctx, recovery.Identity)
}

// refreshOnce does the actual probe-session work for a single attempt,
// addressing Claude via id's SessionID (supplied by the caller --
// refreshWithRecovery -- rather than loaded here, so a post-conflict
// retry can pass a freshly-rotated identity without this function
// needing any recovery logic of its own): it starts the one owned Claude
// session under a PTY this call owns transiently, answers the
// workspace-trust dialog if id.TrustAccepted is false, sends
// usageProbePrompt to elicit a real API response (consuming a small
// amount of quota -- see usageProbePrompt's doc comment), waits for
// either the collector to observe a snapshot that actually reflects a
// completed response to this prompt or Claude Code's own terminal output
// to show a usage-limit or session-conflict indication (see
// waitForProbeOutcome/classifyProbeOutput/checkSessionConflict), then
// detaches -- the process is never left running across refreshes (see
// Probe's doc comment): each attempt runs only one transient probe
// process, addressing exactly one --session-id. That does NOT mean
// Claude's own native catalog only ever shows one probe row, though: a
// rotation (see rotateProbeIdentity) leaves the rejected SessionID's row
// sitting in that catalog indefinitely, since Claude Code never removes
// it on its own. Provider.List instead excludes every session ID this
// probe has ever owned -- current and retired alike (see
// UsageProbeSource.KnownSessionIDs) -- rather than relying on the
// catalog ever containing only one such row.
//
// A detected limit is returned as a valid (err == nil) exhausted
// usageSnapshot, not a failure -- see exhaustedSnapshot and Issue #19's
// "limit 到達を...単なる refresh failure として扱わない". A detected session
// conflict is returned as an error wrapping errProbeSessionConflict (see
// checkSessionConflict), which only refreshWithRecovery ever interprets
// specially. Any other, genuinely unrelated failure (timeout, parse
// failure, process launch failure) returns a plain error, preserving
// #14's existing stale-cache fallback policy for those cases unchanged
// (see Usage's own doc comment) -- refreshWithRecovery does not rotate or
// retry for these.
func (pr *Probe) refreshOnce(ctx context.Context, id probeIdentity) (usageSnapshot, error) {
	if err := os.MkdirAll(pr.Dir, 0o700); err != nil {
		return usageSnapshot{}, err
	}
	exe, err := pr.exePath()
	if err != nil {
		return usageSnapshot{}, fmt.Errorf("resolve agentsctl executable: %w", err)
	}
	if err := writeUsageSettings(pr.settingsPath(), exe, pr.snapshotPath()); err != nil {
		return usageSnapshot{}, fmt.Errorf("write claude usage probe settings: %w", err)
	}

	args := []string{"--session-id", id.SessionID, "--settings", pr.settingsPath()}
	cmd := exec.CommandContext(ctx, pr.claudePath(), args...)
	cmd.Dir = pr.Dir
	child, err := startClaudeAttachRaw(cmd)
	if err != nil {
		return usageSnapshot{}, fmt.Errorf("start claude usage probe session: %w", err)
	}
	defer child.Close()
	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()
	// No real terminal is watching this session's output; capture it
	// (rather than plain drainUntilClosed's discard, as sendClaudeRename
	// uses) both to keep the pty buffer draining -- an unread buffer would
	// otherwise make the child block on its own writes -- and so a usage-
	// limit indication in that output can be classified below.
	capture := &probeOutputCapture{}
	go captureUntilClosed(child, capture)

	// See usageProbeSettleDelay's doc comment for why this waits, unlike
	// sendClaudeRename's immediate write.
	if err := probeSettle(ctx, pr.settleDelay()); err != nil {
		return usageSnapshot{}, err
	}

	// A rejected --session-id shows up here, within the settle window,
	// well before any prompt is sent (see probeSessionConflict's doc
	// comment) -- checked before the trust-dialog logic below so the
	// common case never misreports it as a confusing, unrelated pty write
	// failure once the (already-dead) child's slave end is written to.
	if err, ok := pr.checkSessionConflict(capture, id); ok {
		return usageSnapshot{}, err
	}

	if !id.TrustAccepted {
		if _, err := child.Write([]byte(usageProbeTrustDialogAccept)); err != nil {
			if cerr, ok := pr.checkSessionConflict(capture, id); ok {
				return usageSnapshot{}, cerr
			}
			return usageSnapshot{}, fmt.Errorf("accept claude workspace trust dialog: %w", err)
		}
		// Persisted before the prompt below, not after refresh succeeds:
		// this dialog is answered at most once ever, regardless of
		// whether the rest of this particular refresh goes on to
		// succeed or fail (see probeIdentity.TrustAccepted). Marks
		// whatever identity is LATEST persisted at this instant, not
		// necessarily this attempt's own `id` (see markTrustAccepted's
		// own doc comment) -- workspace trust is directory-level, so
		// that's the correct target even if another process rotated
		// concurrently with this very attempt.
		//
		// Its error is no longer silently discarded: this is now a
		// meaningful identity-lock transaction (see markTrustAccepted's
		// own doc comment), not a fire-and-forget write, and continuing
		// on to send the real prompt below on the unverified assumption
		// that it durably persisted would leave a LATER refresh's own
		// `!id.TrustAccepted` check still true even though Claude Code's
		// own state already considers this directory trusted -- exactly
		// the blind-resend hazard TrustAccepted exists to avoid (see
		// probeIdentity's own doc comment). Failing this attempt instead
		// falls through to Usage's existing stale-cache fallback, same as
		// any other refreshOnce failure.
		//
		// This alone doesn't fully close that hazard: refreshCrossProcess
		// (see its own doc comment) is this package's actual primary
		// defense, serializing first-trust handling across every
		// agentsctl process sharing this probe directory so the
		// interleaving this guards against essentially can't occur in
		// normal operation; a persistence failure occurring right here is
		// a narrow residual window even that can't close by itself (a
		// later refresh, still seeing TrustAccepted=false on disk, would
		// still attempt to answer a dialog Claude Code may no longer be
		// showing). Deliberately NOT added on top of this: a text-based
		// "is the trust dialog actually still showing" classifier,
		// checked alongside checkSessionConflict's kind of output
		// classification. Unlike probeSessionConflictPhrase or
		// classifyProbeOutput's limit wording -- both confirmed against
		// real, captured, quoted installed-CLI terminal output in earlier
		// rounds -- no real Claude Code workspace-trust dialog text has
		// ever actually been captured and verified against the installed
		// CLI for this probe, and this round's fix is required to close
		// without any further live-quota-consuming Claude calls to go
		// verify one. A guessed pattern risks exactly the fail-unsafe
		// outcome this whole mechanism exists to prevent (a false
		// negative sending real navigation/Enter keystrokes into a live
		// chat composer), so this residual window is accepted and
		// documented rather than closed with an unverified classifier.
		if err := markTrustAccepted(pr.identityPath()); err != nil {
			return usageSnapshot{}, fmt.Errorf("persist claude workspace trust acceptance: %w", err)
		}
		if err := probeSettle(ctx, pr.trustSettleDelay()); err != nil {
			return usageSnapshot{}, err
		}
	}

	sentAt := time.Now()
	if _, err := child.Write([]byte(usageProbePrompt + "\r")); err != nil {
		// A conflict that arrived just after the settle-time check above
		// (a narrow race, not observed but not impossible under heavy
		// system load) would otherwise surface here only as a generic,
		// unhelpful pty write error with no recovery action taken -- this
		// re-check is the same identity-discarding recovery as the
		// settle-time one above, just as a fallback rather than the
		// common path.
		if cerr, ok := pr.checkSessionConflict(capture, id); ok {
			return usageSnapshot{}, cerr
		}
		return usageSnapshot{}, fmt.Errorf("send claude usage probe prompt: %w", err)
	}

	snap, sig, waitErr := waitForProbeOutcome(ctx, pr.snapshotPath(), capture, sentAt, pr.sendTimeout())

	pr.detachProbeSession(ctx, cmd, wait)

	if sig.any() {
		return pr.exhaustedSnapshot(sig, pr.now()), nil
	}
	if waitErr != nil {
		return usageSnapshot{}, waitErr
	}
	return snap, nil
}

// checkSessionConflict reports (via ok) whether capture shows Claude Code
// rejecting id.SessionID as already in use (see probeSessionConflict). It
// is a pure classifier with no side effect on persisted state -- deciding
// what to do about a conflict (rotate the identity and retry once) is
// refreshWithRecovery's responsibility, not refreshOnce's; this only
// needs to produce a distinguishable error (wrapping
// errProbeSessionConflict, checked via errors.Is, never by re-parsing a
// message string). Called at more than one point in refreshOnce (see its
// own call sites) so a conflict is caught promptly in the common case but
// never missed just because it showed up a moment later than the first
// check ran.
func (pr *Probe) checkSessionConflict(capture *probeOutputCapture, id probeIdentity) (err error, ok bool) {
	if !probeSessionConflict(capture.String()) {
		return nil, false
	}
	return fmt.Errorf("claude usage probe: session id %s rejected as already in use: %w", id.SessionID, errProbeSessionConflict), true
}

// probeSettle waits for d, or returns ctx's error if ctx is cancelled
// first.
func probeSettle(ctx context.Context, d time.Duration) error {
	select {
	case <-time.After(d):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// waitForProbeOutcome polls both path (the collector's persisted snapshot)
// and capture (the probe session's own raw terminal output) until one of
// three outcomes is reached: (1) path holds a snapshot whose ObservedAt is
// after `after` AND whose ResponseObserved is true -- proving it reflects
// a completed response to this refresh's own prompt, not a periodic
// pre-response or stale-prior-turn re-tick (see parseStatusLinePayload's
// doc comment) -- returned as a normal fresh snapshot; (2) capture's
// accumulated output classifies as a usage-limit indication (see
// classifyProbeOutput) -- returned as a limit signal, snapshot zero,
// error nil, since this is itself a valid outcome, not a failure; or (3)
// timeout elapses with neither -- a genuine non-limit failure (see
// refresh's own doc comment for how each outcome is handled).
func waitForProbeOutcome(ctx context.Context, path string, capture *probeOutputCapture, after time.Time, timeout time.Duration) (usageSnapshot, probeLimitSignal, error) {
	deadline := time.Now().Add(timeout)
	for {
		if snap, ok, err := readUsageSnapshot(path); err == nil && ok && snap.ObservedAt.After(after) && snap.ResponseObserved {
			return snap, probeLimitSignal{}, nil
		}
		if sig := classifyProbeOutput(capture.String()); sig.any() {
			return usageSnapshot{}, sig, nil
		}
		if !time.Now().Before(deadline) {
			return usageSnapshot{}, probeLimitSignal{}, fmt.Errorf("claude usage probe: no fresh statusLine snapshot within %s", timeout)
		}
		select {
		case <-ctx.Done():
			return usageSnapshot{}, probeLimitSignal{}, ctx.Err()
		case <-time.After(usageProbePollInterval):
		}
	}
}

// exhaustedSnapshot builds the usageSnapshot for a refresh that detected a
// Claude usage limit (see classifyProbeOutput) instead of obtaining a
// trustworthy fresh statusLine snapshot. Each window sig marks exhausted
// becomes session.UsageExhausted at 100%, carrying forward that same
// window's own prior cached Reset time when one is known (the server-set
// reset boundary doesn't change just because this refresh couldn't
// re-confirm it) -- never inventing one. A window sig does NOT mark is
// left exactly as it was previously cached, so a limit confirmed for one
// window never destroys the other's still-valid snapshot (see Issue #19's
// "片方だけ exhausted の場合...もう片方の有効な snapshot を不必要に失わない"); that
// carried-forward window is still subject to the normal reset-boundary
// normalization session.Usage.At applies in toSessionUsage, same as any
// other cached window.
func (pr *Probe) exhaustedSnapshot(sig probeLimitSignal, now time.Time) usageSnapshot {
	prev, _ := pr.cachedAny()
	snap := usageSnapshot{ObservedAt: now, ResponseObserved: true, FiveHour: prev.FiveHour, Weekly: prev.Weekly}
	if sig.FiveHour {
		snap.FiveHour = exhaustedWindowSnapshot(prev.FiveHour)
	}
	if sig.Weekly {
		snap.Weekly = exhaustedWindowSnapshot(prev.Weekly)
	}
	return snap
}

// exhaustedWindowSnapshot builds one exhausted window, carrying forward
// prev's own Reset time when prev had one (see exhaustedSnapshot).
func exhaustedWindowSnapshot(prev usageWindowSnapshot) usageWindowSnapshot {
	reset := prev.ResetAt
	if prev.State == session.UsageUnknown {
		reset = time.Time{}
	}
	return usageWindowSnapshot{State: session.UsageExhausted, Percent: 100, ResetAt: reset}
}

// detachProbeSession ends a plain interactive probe session process.
// Unlike detachClaudeClient (built for `claude attach <id>`'s own
// documented Ctrl+Z detach hotkey), a brand-new `claude --session-id ...`
// session does not treat a literal Ctrl+Z byte the same way: verified
// against the installed CLI, it is instead read as an ordinary job-control
// suspend request, leaving the process permanently "suspended" (the CLI's
// own message: "Run `fg` to bring Claude Code back") rather than exiting,
// until force-killed. SIGINT is what actually works for an otherwise-idle
// session (also verified against the installed CLI); SIGTERM was tried
// too and does not reliably end the process either. A SIGKILL fallback
// here is safe, unlike a mid-turn kill (observed to leave that session's
// conversation permanently unresumable): this probe never depends on any
// given refresh's conversation surviving (see probeIdentity's doc
// comment), so losing it costs nothing beyond this one process needing to
// be started again next refresh.
func (pr *Probe) detachProbeSession(ctx context.Context, cmd *exec.Cmd, wait <-chan error) {
	if cmd.Process == nil {
		return
	}
	// A bounded, unconditional wait (not tied to ctx) -- cleaning up this
	// process is worth attempting even if the caller's own ctx has
	// already been cancelled (e.g. agentsctl itself is exiting).
	time.Sleep(pr.detachSettleDelay())
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGINT)
	if _, ok := waitForAttachment(ctx, wait, usageProbeDetachTimeout); ok {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	_, _ = waitForAttachment(ctx, wait, usageProbeDetachTimeout)
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
func toSessionUsage(snap usageSnapshot, now time.Time) session.Usage {
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
func toSessionUsageWindow(w usageWindowSnapshot) session.UsageWindow {
	return session.UsageWindow{State: w.State, Percent: w.Percent, Reset: w.ResetAt}
}
