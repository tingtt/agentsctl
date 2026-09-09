//go:build darwin || linux

package claude

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"

	"github.com/tingtt/agentsctl/internal/provider/claude/probestate"
	"github.com/tingtt/agentsctl/internal/session"
)

// refreshCoordinator is the machine-global Root Owner of "run the Claude
// probe session's refresh": across every agentsctl process sharing dir,
// exactly one refreshCoordinator.Refresh call is ever inside the
// identity-load/attempt/rotate/retry lifecycle at a time, and the
// resulting persist happens before that exclusivity is released. A caller
// (Probe's own process-local single-flight, in refreshShared) gets this
// whole lifecycle from one Refresh(ctx) call -- it never acquires the
// refresh lock, checks persisted freshness, runs recovery, or persists
// the result itself; those steps exist only inside this type.
//
// See the DesignDoc's Claude usage probe "Lock ordering": this lock is
// always acquired BEFORE any identity-store lock, and identity-store
// operations are only ever invoked from within a held Refresh call (via
// identity/attempt), never independently -- so the reverse ordering
// (holding an identity lock while waiting for this one) cannot occur.
type refreshCoordinator struct {
	// dir is this probe's dedicated app-data directory -- the refresh
	// lock file lives at filepath.Join(dir, "refresh.lock"), deliberately
	// never probe.json's own lock file (see lock's doc comment).
	dir string
	// identity and snapshot are this coordinator's view of the same two
	// Root Owners Probe itself addresses for other purposes (KnownSessionIDs,
	// loadPersistedSnapshotOnce) -- constructed fresh per coordinator, not
	// shared state, since both are cheap path wrappers.
	identity *probestate.IdentityStore
	snapshot *probestate.SnapshotStore
	// ttl is the freshness policy (probestate.SnapshotFresh) applies
	// against the persisted snapshot re-check below.
	ttl time.Duration
	// now provides "now" for a detected-limit outcome's ObservedAt (see
	// resolveOutcome/exhaustedSnapshot) -- Probe's own Clock-driven now(),
	// so a test can deterministically control it. This is the only use of
	// "now" this type has; the TTL freshness check above deliberately uses
	// the real wall clock instead (see Probe.Clock's own doc comment).
	now func() time.Time
	// attempt runs one single Claude PTY attempt against the given
	// identity, reporting a typed outcome -- see usage_attempt.go's
	// probeAttemptRunner.Run, the sole production value this is ever set
	// to. A func field rather than a formal interface: the only reason to
	// substitute it at all is this package's own tests exercising the
	// coordinator's lock/retry/persist behavior without a real attempt
	// runner, not because more than one real implementation exists.
	attempt func(ctx context.Context, id probestate.Identity) (probeAttemptResult, error)
}

// Refresh is this type's only entry point: acquire the machine refresh
// lock, re-check the persisted snapshot's freshness now that the lock is
// held (reusing another process's just-completed refresh if it's already
// fresh), and otherwise run the identity-load/attempt/rotate/retry
// lifecycle and persist its result -- all before releasing the lock. See
// this type's own doc comment for why a caller can't assemble any subset
// of this sequence on its own.
//
// prev is the caller's current process-local cached snapshot (the zero
// value if it has none) -- used only if this refresh's own attempt
// reports a detected usage-limit outcome, to carry forward whichever
// window the limit did NOT cover (see resolveOutcome/exhaustedSnapshot).
// It is never itself returned as this call's result and never persisted
// as-is: Refresh always either returns a snapshot this attempt actually
// produced (fresh, reused-from-disk, or limit-merged) or an error.
//
// The persist-on-success step (snapshot.Save) deliberately stays inside
// the locked section, not after it: releasing the lock before persisting
// would reopen the exact window this lock exists to close, letting a
// second process's own freshness re-check run against a still-stale
// usage.json and duplicate the just-completed refresh. A write failure
// here is NOT escalated into this call's own error -- an otherwise-valid
// refresh result must survive a persistence-layer hiccup (see the
// DesignDoc's Claude usage cache/failure policy) -- but it does mean this
// specific dedupe guarantee is lost for whichever other process's own
// wait happens to end before some later successful write finally lands:
// that process will see a still-stale (or absent) usage.json under the
// lock and, correctly by its own local information, perform its own
// redundant refresh. This is a narrow, honestly-documented gap, not a
// silently-assumed one -- session ID conflict recovery remains available
// to both processes regardless.
func (c *refreshCoordinator) Refresh(ctx context.Context, prev probestate.Snapshot) (probestate.Snapshot, error) {
	unlock, err := c.lock(ctx)
	if err != nil {
		return probestate.Snapshot{}, fmt.Errorf("acquire claude usage probe refresh lock: %w", err)
	}
	defer unlock()

	// Re-checked against the exact same probestate.SnapshotFresh policy
	// Probe.cachedFresh already applies to the in-memory cache -- no
	// separate or looser freshness rule. This is what turns the lock from
	// "just serialize refreshes" into "actually skip the second one": a
	// caller that had to wait for the lock, arriving here right after
	// another process's own Refresh call already persisted a fresh
	// result, returns that persisted snapshot directly, never touching
	// Claude at all.
	if snap, ok, err := c.snapshot.LoadFresh(time.Now(), c.ttl); err == nil && ok {
		return snap, nil
	}

	snap, err := c.refreshWithRecovery(ctx, prev)
	if err != nil {
		return probestate.Snapshot{}, err
	}
	_ = c.snapshot.Save(snap) // see this method's own doc comment for why a write failure here is deliberately not returned as this call's error
	return snap, nil
}

// refreshWithRecovery is this coordinator's retry/rotation policy -- the
// only place in this package that owns it: attempt itself (the attempt
// runner) never retries or rotates on its own (see usage_attempt.go's own
// doc comment). At most one rotation/recovery attempt and one retry ever
// happen -- never a loop: a second errProbeSessionConflict (or any other
// error) from the retry is returned exactly as attempt reported it,
// falling through to Refresh's/Usage's existing stale-cache-or-error
// handling unchanged. If probestate.IdentityStore.Rotate itself fails (a
// persistence error, not a conflict), the original conflict error is
// returned rather than attempting a retry with no valid identity to use.
func (c *refreshCoordinator) refreshWithRecovery(ctx context.Context, prev probestate.Snapshot) (probestate.Snapshot, error) {
	id, err := c.identity.LoadOrCreate()
	if err != nil {
		return probestate.Snapshot{}, fmt.Errorf("load claude usage probe identity: %w", err)
	}
	result, err := c.attempt(ctx, id)
	if err == nil {
		return c.resolveOutcome(result, prev), nil
	}
	if !errors.Is(err, errProbeSessionConflict) {
		return probestate.Snapshot{}, err
	}
	recovery, rerr := c.identity.Rotate(id.SessionID)
	if rerr != nil {
		return probestate.Snapshot{}, err
	}
	result, err = c.attempt(ctx, recovery.Identity)
	if err != nil {
		return probestate.Snapshot{}, err
	}
	return c.resolveOutcome(result, prev), nil
}

// resolveOutcome turns one attempt's typed result into the snapshot this
// refresh should persist and return: a genuine fresh snapshot passes
// through unchanged, while a detected usage-limit outcome is merged with
// prev via exhaustedSnapshot -- this merge is lifecycle-level policy (it
// depends on Probe's own process-local cache, which the attempt runner
// has no business knowing about), so it lives here, not in the runner.
func (c *refreshCoordinator) resolveOutcome(result probeAttemptResult, prev probestate.Snapshot) probestate.Snapshot {
	if !result.Limit.any() {
		return result.Snapshot
	}
	return exhaustedSnapshot(result.Limit, prev, c.now())
}

// exhaustedSnapshot builds the probestate.Snapshot for a refresh that
// detected a Claude usage limit (see classifyProbeOutput) instead of
// obtaining a trustworthy fresh statusLine snapshot. Each window sig
// marks exhausted becomes session.UsageExhausted at 100%, carrying
// forward that same window's own prior cached Reset time when one is
// known (the server-set reset boundary doesn't change just because this
// refresh couldn't re-confirm it) -- never inventing one. A window sig
// does NOT mark is left exactly as prev had it, so a limit confirmed for
// one window never destroys the other's still-valid snapshot (see Issue
// #19's "片方だけ exhausted の場合...もう片方の有効な snapshot を不必要に失わない");
// that carried-forward window is still subject to the normal
// reset-boundary normalization session.Usage.At applies in toSessionUsage,
// same as any other cached window.
func exhaustedSnapshot(sig probeLimitSignal, prev probestate.Snapshot, now time.Time) probestate.Snapshot {
	snap := probestate.Snapshot{ObservedAt: now, ResponseObserved: true, FiveHour: prev.FiveHour, Weekly: prev.Weekly}
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
func exhaustedWindowSnapshot(prev probestate.WindowSnapshot) probestate.WindowSnapshot {
	reset := prev.ResetAt
	if prev.State == session.UsageUnknown {
		reset = time.Time{}
	}
	return probestate.WindowSnapshot{State: session.UsageExhausted, Percent: 100, ResetAt: reset}
}

// lock acquires the exclusive cross-process refresh lock at
// filepath.Join(c.dir, "refresh.lock") -- deliberately its own file,
// never probe.json's own lock file: an attempt calls
// probestate.IdentityStore.MarkTrustAccepted, which acquires the identity
// transaction lock as its own short-lived transaction, so holding this
// refresh lock across an entire refresh attempt (this lock's actual
// scope, potentially a full Claude round trip) must never be the SAME
// lock as that inner acquisition, or it would either deadlock against
// itself or require reentrant locking. Keeping the two locks -- and the
// two files backing them -- entirely separate keeps the ordering strictly
// one-directional (refresh lock held first, identity lock acquired and
// released many times inside it, never the reverse) with no risk of
// lock-order inversion.
//
// Unlike probestate.IdentityStore's single blocking unix.Flock call (safe
// there because an identity transaction never holds its lock for more
// than one local read plus one atomic write), a caller here can hold this
// lock for as long as a full Claude round trip takes, so Usage(ctx)'s own
// cancellation contract has to keep working while waiting for it: a
// non-blocking LOCK_EX|LOCK_NB attempt, polled at
// usageProbeRefreshLockPollInterval, rather than one call that could
// block past ctx's own deadline with no way to interrupt it.
func (c *refreshCoordinator) lock(ctx context.Context) (func(), error) {
	if err := os.MkdirAll(c.dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(c.dir, "refresh.lock"), os.O_CREATE|os.O_RDWR, 0o600)
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
