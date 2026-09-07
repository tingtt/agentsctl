//go:build darwin || linux

package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	base "github.com/tingtt/agentsctl/internal/provider"
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

// usageProbeConfirmTimeout bounds how long Probe.refresh polls `claude
// agents --json --all` for a brand-new probe session's ID to appear,
// before leaving Confirmed unset for the next refresh to retry (see
// probeIdentity.Confirmed).
const usageProbeConfirmTimeout = 5 * time.Second

// usageProbePollInterval is the polling cadence for both
// waitForFreshSnapshot and confirmSessionAppeared.
const usageProbePollInterval = 250 * time.Millisecond

// usageProbeSettleDelay is a deliberate wait between starting the probe
// session's process and writing usageProbePrompt to it -- unlike
// sendClaudeRename's write-immediately design (verified, by real-CLI
// measurement, safe against `claude attach <id>`: attaching to an
// already-running background session), this starts a brand-new
// interactive session from cold (auth, model/session setup, and only then
// its own raw-mode terminal setup), a startup path with no equivalent
// live-CLI measurement available to prove immediate-write is safe here
// too. Empirically (against this package's own fake CLI test double) an
// immediate write can race a slow-starting child's own terminal-mode
// setup and be silently dropped, so this settles first rather than
// assuming the same zero-latency guarantee sendClaudeRename relies on.
const usageProbeSettleDelay = 1 * time.Second

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
	// Runner runs `claude agents --json --all` to confirm a brand-new
	// probe session actually landed in Claude's native catalog (see
	// confirmSessionAppeared). Required for that confirmation step only --
	// a nil Runner just leaves new probe identities permanently
	// unconfirmed, which is safe (refresh always falls back to
	// --session-id) but never lets refresh use the cheaper --resume path.
	Runner base.Runner
	// Dir is this probe's dedicated app-data directory (never a user
	// project directory): it holds the probe's identity, its dedicated
	// Claude settings, and the persisted usage snapshot.
	Dir string
	// ExePath overrides how refresh resolves "this agentsctl executable"
	// for the generated statusLine command; empty uses os.Executable().
	// Exposed for tests, which run as a `go test` binary rather than the
	// real one.
	ExePath string

	mu          sync.Mutex
	snapshot    usageSnapshot
	hasSnapshot bool
	snapshotAt  time.Time
	refreshCh   chan struct{} // non-nil while a refresh is in flight (single-flight)
}

// NewProbe returns a Probe backed by dir (created lazily on first refresh).
func NewProbe(path string, runner base.Runner, dir string) *Probe {
	return &Probe{Path: path, Runner: runner, Dir: dir}
}

func (pr *Probe) claudePath() string {
	if pr.Path != "" {
		return pr.Path
	}
	return "claude"
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
// usageProbeTTL) is returned immediately with no process work at all. A
// stale or absent cache triggers refreshShared; if that fails but a stale
// snapshot still exists, the stale snapshot is returned rather than
// failing the whole call -- Codex's usage row, and the session catalog
// itself, must never be taken down by a Claude-side hiccup (see the
// DesignDoc's Claude usage cache/failure policy). Only a refresh failure
// with no prior snapshot at all surfaces as an error, which
// sessionctl.Controller.Usage already treats as "omit this provider from
// the usage line" -- never a fake 0%.
func (pr *Probe) Usage(ctx context.Context) (session.Usage, error) {
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

// refresh does the actual probe-session work: it starts (or resumes) the
// one owned Claude session under a PTY this call owns transiently, sends
// usageProbePrompt to elicit a real API response (consuming a small amount
// of quota -- see usageProbePrompt's doc comment), waits for the collector
// to observe a fresh statusLine snapshot, then detaches -- the process is
// never left running across refreshes (see Probe's doc comment): each
// refresh is its own short-lived attach, identified by the same persisted
// session ID every time (see loadOrCreateProbeIdentity), so Claude's own
// native catalog shows exactly one probe session no matter how many
// refreshes have run.
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

	var args []string
	if id.Confirmed {
		args = []string{"--resume", id.SessionID, "--settings", pr.settingsPath()}
	} else {
		args = []string{"--session-id", id.SessionID, "--settings", pr.settingsPath()}
	}
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
	select {
	case <-time.After(usageProbeSettleDelay):
	case <-ctx.Done():
		return usageSnapshot{}, ctx.Err()
	}

	sentAt := time.Now()
	if _, err := child.Write([]byte(usageProbePrompt + "\r")); err != nil {
		return usageSnapshot{}, fmt.Errorf("send claude usage probe prompt: %w", err)
	}

	snap, waitErr := waitForFreshSnapshot(ctx, pr.snapshotPath(), sentAt, usageProbeSendTimeout)

	if !id.Confirmed && pr.confirmSessionAppeared(ctx, id.SessionID) {
		_ = markProbeConfirmed(pr.identityPath(), id)
	}

	_ = detachClaudeClient(ctx, cmd, child, wait, openDetachTimeout)

	if waitErr != nil {
		return usageSnapshot{}, waitErr
	}
	return snap, nil
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

// confirmSessionAppeared polls `claude agents --json --all` for id,
// bounded by usageProbeConfirmTimeout, reporting whether it was found. A
// nil Runner (or any poll error) simply reports false -- refresh leaves
// Confirmed unset and retries --session-id next time, which is safe
// (idempotent from Claude's perspective if the session already exists)
// even if never actually necessary.
func (pr *Probe) confirmSessionAppeared(ctx context.Context, id string) bool {
	if pr.Runner == nil {
		return false
	}
	deadline := time.Now().Add(usageProbeConfirmTimeout)
	for {
		if res, err := pr.Runner.Run(ctx, pr.claudePath(), []string{"agents", "--json", "--all"}, ""); err == nil {
			var raw []map[string]any
			if json.Unmarshal(res.Stdout, &raw) == nil {
				if _, found := nativeSessionByID(raw, id); found {
					return true
				}
			}
		}
		if !time.Now().Before(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(usageProbePollInterval):
		}
	}
}

// nativeSessionByID reports whether raw (a decoded `claude agents --json
// --all` response) contains a row for id.
func nativeSessionByID(raw []map[string]any, id string) (row map[string]any, found bool) {
	for _, v := range raw {
		if text(v, "id", "sessionId") == id {
			return v, true
		}
	}
	return nil, false
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
