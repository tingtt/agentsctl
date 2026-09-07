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

	"github.com/tingtt/agentsctl/internal/session"
)

// usageProbeTTL bounds how long a cached Claude usage snapshot is served
// before Usage() triggers a fresh refresh through the owned probe session
// (see Probe.Usage) -- #14's "1〜5分程度" cache freshness guidance.
const usageProbeTTL = 3 * time.Minute

// usageProbePrompt is the minimal message Probe.refresh sends into the
// probe session to elicit one real API response (the only way Claude Code
// populates rate_limits for its statusLine -- see UsageProbeSource's doc
// comment). It deliberately asks for no tool use: this session has no
// human watching its PTY to approve a permission prompt, and any it
// spawned would simply hang until usageProbeSendTimeout.
const usageProbePrompt = "Reply with just the word OK. Do not use any tools."

// usageProbeSendTimeout bounds how long Probe.refresh waits for the
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
// see refresh), configured via a dedicated --settings file whose
// statusLine points back at this same executable's UsageCollectorCommand.
// It implements UsageProbeSource for Provider.
//
// A Probe is safe for concurrent use: Usage single-flights refreshes (see
// refreshShared) so concurrent callers never spawn more than one probe
// process or send more than one prompt at a time.
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
func (pr *Probe) settingsPath() string { return filepath.Join(pr.Dir, "settings.json") }
func (pr *Probe) snapshotPath() string { return filepath.Join(pr.Dir, "usage.json") }
func (pr *Probe) identityPath() string { return filepath.Join(pr.Dir, "probe.json") }

func (pr *Probe) exePath() (string, error) {
	if pr.ExePath != "" {
		return pr.ExePath, nil
	}
	return os.Executable()
}

// KnownSessionID implements UsageProbeSource: a pure local-file read (no
// process spawned, no catalog call), so Provider.List can cheaply exclude
// the probe's row on every load. An identity that has never been created,
// or that can't be read, reports ok=false -- List then excludes nothing,
// never guessing (see Provider.List's doc comment on this call site).
func (pr *Probe) KnownSessionID() (string, bool) {
	id, ok, err := readProbeIdentityIfExists(pr.identityPath())
	if err != nil || !ok {
		return "", false
	}
	return id.SessionID, true
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
func (pr *Probe) Usage(ctx context.Context) (session.Usage, error) {
	pr.loadPersistedSnapshotOnce()
	if snap, ok := pr.cachedFresh(); ok {
		return toSessionUsage(snap), nil
	}
	snap, err := pr.refreshShared(ctx)
	if err == nil {
		return toSessionUsage(snap), nil
	}
	if stale, ok := pr.cachedAny(); ok {
		return toSessionUsage(stale), nil
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

// refreshShared single-flights refresh: concurrent callers arriving while
// a refresh is already in progress wait for it (bounded by ctx) and reuse
// its result instead of starting a second probe process or prompt --
// guaranteeing at most one probe-session creation and at most one stale
// refresh request in flight at a time, regardless of how many goroutines
// call Usage concurrently.
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

	snap, err := pr.refresh(ctx)

	pr.mu.Lock()
	if err == nil {
		pr.snapshot, pr.hasSnapshot, pr.snapshotAt = snap, true, time.Now()
	}
	pr.refreshCh = nil
	pr.mu.Unlock()
	close(ch)
	return snap, err
}

// refresh does the actual probe-session work: it starts the one owned
// Claude session (always addressed by the same persisted --session-id --
// see probeIdentity's doc comment) under a PTY this call owns transiently,
// answers the workspace-trust dialog if this is the very first launch
// ever for this probe identity, sends usageProbePrompt to elicit a real
// API response (consuming a small amount of quota -- see
// usageProbePrompt's doc comment), waits for the collector to observe a
// fresh statusLine snapshot, then detaches -- the process is never left
// running across refreshes (see Probe's doc comment): each refresh is its
// own short-lived attach, so Claude's own native catalog shows exactly
// one probe session no matter how many refreshes have run.
func (pr *Probe) refresh(ctx context.Context) (usageSnapshot, error) {
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
	id, err := loadOrCreateProbeIdentity(pr.identityPath())
	if err != nil {
		return usageSnapshot{}, fmt.Errorf("load claude usage probe identity: %w", err)
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
	// No real terminal is watching this session's output; drain it for the
	// same reason sendClaudeRename does (an unread pty buffer would
	// otherwise make the child block on its own writes).
	go drainUntilClosed(child)

	// See usageProbeSettleDelay's doc comment for why this waits, unlike
	// sendClaudeRename's immediate write.
	if err := probeSettle(ctx, pr.settleDelay()); err != nil {
		return usageSnapshot{}, err
	}

	if !id.TrustAccepted {
		if _, err := child.Write([]byte(usageProbeTrustDialogAccept)); err != nil {
			return usageSnapshot{}, fmt.Errorf("accept claude workspace trust dialog: %w", err)
		}
		// Persisted before the prompt below, not after refresh succeeds:
		// this dialog is answered at most once ever, regardless of
		// whether the rest of this particular refresh goes on to
		// succeed or fail (see probeIdentity.TrustAccepted).
		_ = markTrustAccepted(pr.identityPath(), id)
		if err := probeSettle(ctx, pr.trustSettleDelay()); err != nil {
			return usageSnapshot{}, err
		}
	}

	sentAt := time.Now()
	if _, err := child.Write([]byte(usageProbePrompt + "\r")); err != nil {
		return usageSnapshot{}, fmt.Errorf("send claude usage probe prompt: %w", err)
	}

	snap, waitErr := waitForFreshSnapshot(ctx, pr.snapshotPath(), sentAt, usageProbeSendTimeout)

	pr.detachProbeSession(ctx, cmd, wait)

	if waitErr != nil {
		return usageSnapshot{}, waitErr
	}
	return snap, nil
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

// waitForFreshSnapshot polls path until it holds a snapshot whose
// ObservedAt is after `after` (proving the collector wrote it in response
// to this refresh's own prompt, not a leftover from an earlier one) or
// timeout elapses.
func waitForFreshSnapshot(ctx context.Context, path string, after time.Time, timeout time.Duration) (usageSnapshot, error) {
	deadline := time.Now().Add(timeout)
	for {
		if snap, ok, err := readUsageSnapshot(path); err == nil && ok && snap.ObservedAt.After(after) {
			return snap, nil
		}
		if !time.Now().Before(deadline) {
			return usageSnapshot{}, fmt.Errorf("claude usage probe: no fresh statusLine snapshot within %s", timeout)
		}
		select {
		case <-ctx.Done():
			return usageSnapshot{}, ctx.Err()
		case <-time.After(usageProbePollInterval):
		}
	}
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
func toSessionUsage(snap usageSnapshot) session.Usage {
	return session.Usage{
		Provider: session.ProviderClaude,
		FiveHour: toSessionUsageWindow(snap.FiveHour),
		Weekly:   toSessionUsageWindow(snap.Weekly),
	}
}
func toSessionUsageWindow(w usageWindowSnapshot) session.UsageWindow {
	if !w.Available {
		return session.UsageWindow{}
	}
	return session.UsageWindow{Available: true, Percent: w.Percent, Reset: w.ResetAt}
}
