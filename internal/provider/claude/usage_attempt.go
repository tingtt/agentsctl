//go:build darwin || linux

package claude

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/tingtt/agentsctl/internal/provider/claude/probestate"
)

// probeAttemptResult is one probeAttemptRunner.Run call's typed outcome
// (paired with the returned error): Limit.any() true means a usage-limit
// indication was observed instead of a genuine fresh snapshot (Snapshot is
// then unused -- see refreshCoordinator.resolveOutcome, which is the only
// place that turns this into a persistable probestate.Snapshot); Limit
// zero and err nil means Snapshot is a genuine fresh reading. A non-nil
// error (possibly wrapping errProbeSessionConflict) means neither field is
// meaningful.
type probeAttemptResult struct {
	Snapshot probestate.Snapshot
	Limit    probeLimitSignal
}

// probeAttemptRunner owns exactly ONE Claude PTY attempt against one
// already-decided session identity: starting the session, settings setup,
// settling, the workspace-trust interaction, sending the probe prompt,
// observing the resulting output/snapshot, and detaching -- see Run's own
// doc comment for the full sequence.
//
// It deliberately does NOT own:
//   - session ID rotation or retry (see refreshCoordinator.refreshWithRecovery
//     -- the only caller of Run, and the only place that decides what to do
//     about an errProbeSessionConflict result)
//   - retired-identity ownership policy (probestate.IdentityStore)
//   - the cross-process refresh lock (refreshCoordinator)
//   - cache policy (Probe)
//
// A caller cannot get this runner to retry on its own: Run returns exactly
// once per call, always for the one identity it was given, with no retry
// loop of any kind inside it.
type probeAttemptRunner struct {
	// dir is this probe's dedicated app-data directory.
	dir string
	// claudePath is the `claude` binary path to run.
	claudePath string
	// exePath resolves "this agentsctl executable" for the generated
	// statusLine command.
	exePath func() (string, error)
	// settingsPath and snapshotPath are this probe's dedicated
	// settings.json and usage.json paths.
	settingsPath string
	snapshotPath string
	// settleDelay, trustSettleDelay, and detachSettleDelay are this
	// attempt's real-CLI-measured settle waits (see the Probe-level
	// usageProbe*Delay constants they default from).
	settleDelay       time.Duration
	trustSettleDelay  time.Duration
	detachSettleDelay time.Duration
	// sendTimeout bounds how long Run waits for either a genuine fresh
	// snapshot or a classified usage-limit indication before giving up
	// with a plain (non-limit) failure.
	sendTimeout time.Duration
	// identity is used for exactly one domain operation:
	// MarkTrustAccepted, called right after this attempt answers the
	// workspace-trust dialog for the first time (see Run's own doc
	// comment). Run never calls any other IdentityStore method -- loading
	// and rotating the identity are refreshCoordinator's job, not this
	// runner's.
	identity *probestate.IdentityStore
}

// Run does the actual probe-session work for a single attempt, addressing
// Claude via id's SessionID (supplied by the caller --
// refreshCoordinator.refreshWithRecovery -- rather than loaded here, so a
// post-conflict retry can pass a freshly-rotated identity without this
// method needing any recovery logic of its own): it starts the one owned
// Claude session under a PTY this call owns transiently, answers the
// workspace-trust dialog if id.TrustAccepted is false, sends
// usageProbePrompt to elicit a real API response (consuming a small
// amount of quota -- see usageProbePrompt's doc comment), waits for
// either the collector to observe a snapshot that actually reflects a
// completed response to this prompt or Claude Code's own terminal output
// to show a usage-limit or session-conflict indication (see
// waitForProbeOutcome/classifyProbeOutput/checkSessionConflict), then
// detaches -- the process is never left running across refreshes: each
// attempt runs only one transient probe process, addressing exactly one
// --session-id. That does NOT mean Claude's own native catalog only ever
// shows one probe row, though: a rotation (see
// probestate.IdentityStore.Rotate) leaves the rejected SessionID's row
// sitting in that catalog indefinitely, since Claude Code never removes
// it on its own. Provider.List instead excludes every session ID this
// probe has ever owned -- current and retired alike (see
// UsageProbeSource.KnownSessionIDs) -- rather than relying on the catalog
// ever containing only one such row.
//
// A detected limit is returned as a valid (err == nil) probeAttemptResult
// with Limit set, not a failure -- see Issue #19's "限定到達を...単なる
// refresh failure として扱わない"; turning that into a persistable exhausted
// snapshot is refreshCoordinator.resolveOutcome's job, not this method's
// (it would need Probe's own process-local cache to know what the OTHER,
// unaffected window's last valid reading was, which this runner has no
// business knowing about). A detected session conflict is returned as an
// error wrapping errProbeSessionConflict (see checkSessionConflict), which
// only refreshCoordinator.refreshWithRecovery ever interprets specially.
// Any other, genuinely unrelated failure (timeout, parse failure, process
// launch failure) returns a plain error, preserving #14's existing
// stale-cache fallback policy for those cases unchanged -- this method
// never rotates or retries for these; that decision belongs entirely to
// its caller.
func (r *probeAttemptRunner) Run(ctx context.Context, id probestate.Identity) (probeAttemptResult, error) {
	if err := os.MkdirAll(r.dir, 0o700); err != nil {
		return probeAttemptResult{}, err
	}
	exe, err := r.exePath()
	if err != nil {
		return probeAttemptResult{}, fmt.Errorf("resolve agentsctl executable: %w", err)
	}
	if err := writeUsageSettings(r.settingsPath, exe, r.snapshotPath); err != nil {
		return probeAttemptResult{}, fmt.Errorf("write claude usage probe settings: %w", err)
	}

	args := []string{"--session-id", id.SessionID, "--settings", r.settingsPath}
	cmd := exec.CommandContext(ctx, r.claudePath, args...)
	cmd.Dir = r.dir
	child, err := startClaudeAttachRaw(cmd)
	if err != nil {
		return probeAttemptResult{}, fmt.Errorf("start claude usage probe session: %w", err)
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
	if err := probeSettle(ctx, r.settleDelay); err != nil {
		return probeAttemptResult{}, err
	}

	// A rejected --session-id shows up here, within the settle window,
	// well before any prompt is sent (see probeSessionConflict's doc
	// comment) -- checked before the trust-dialog logic below so the
	// common case never misreports it as a confusing, unrelated pty write
	// failure once the (already-dead) child's slave end is written to.
	if err, ok := checkSessionConflict(capture, id.SessionID); ok {
		return probeAttemptResult{}, err
	}

	if !id.TrustAccepted {
		if _, err := child.Write([]byte(usageProbeTrustDialogAccept)); err != nil {
			if cerr, ok := checkSessionConflict(capture, id.SessionID); ok {
				return probeAttemptResult{}, cerr
			}
			return probeAttemptResult{}, fmt.Errorf("accept claude workspace trust dialog: %w", err)
		}
		// Persisted before the prompt below, not after refresh succeeds:
		// this dialog is answered at most once ever, regardless of
		// whether the rest of this particular refresh goes on to
		// succeed or fail (see probestate.Identity.TrustAccepted). Marks
		// whatever identity is LATEST persisted at this instant, not
		// necessarily this attempt's own `id`
		// (see probestate.IdentityStore.MarkTrustAccepted's own doc
		// comment) -- workspace trust is directory-level, so that's the
		// correct target even if another process rotated concurrently
		// with this very attempt.
		//
		// This error must not be silently discarded: MarkTrustAccepted is a
		// meaningful identity-lock transaction (see
		// probestate.IdentityStore.MarkTrustAccepted's own doc comment),
		// not a fire-and-forget write, and continuing on to send the
		// real prompt below on the unverified assumption that it
		// durably persisted would leave a LATER refresh's own
		// `!id.TrustAccepted` check still true even though Claude Code's
		// own state already considers this directory trusted -- exactly
		// the blind-resend hazard TrustAccepted exists to avoid (see
		// probestate.Identity's own doc comment). Failing this attempt
		// instead falls through to Usage's existing stale-cache
		// fallback, same as any other Run failure.
		//
		// This alone doesn't fully close that hazard: the refresh
		// coordinator's own cross-process lock is this package's actual
		// primary defense, serializing first-trust handling across every
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
		// real, captured, quoted installed-CLI terminal output -- no real
		// Claude Code workspace-trust dialog text has ever been captured
		// and verified against the installed CLI for this probe, and doing
		// so would require further live-quota-consuming Claude calls this
		// package avoids. A guessed pattern risks exactly the fail-unsafe
		// outcome this whole mechanism exists to prevent (a false
		// negative sending real navigation/Enter keystrokes into a live
		// chat composer), so this residual window is accepted and
		// documented rather than closed with an unverified classifier.
		if err := r.identity.MarkTrustAccepted(); err != nil {
			return probeAttemptResult{}, fmt.Errorf("persist claude workspace trust acceptance: %w", err)
		}
		if err := probeSettle(ctx, r.trustSettleDelay); err != nil {
			return probeAttemptResult{}, err
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
		if cerr, ok := checkSessionConflict(capture, id.SessionID); ok {
			return probeAttemptResult{}, cerr
		}
		return probeAttemptResult{}, fmt.Errorf("send claude usage probe prompt: %w", err)
	}

	snap, sig, waitErr := waitForProbeOutcome(ctx, r.snapshotPath, capture, sentAt, r.sendTimeout)

	r.detach(ctx, cmd, wait)

	if sig.any() {
		return probeAttemptResult{Limit: sig}, nil
	}
	if waitErr != nil {
		return probeAttemptResult{}, waitErr
	}
	return probeAttemptResult{Snapshot: snap}, nil
}

// checkSessionConflict reports (via ok) whether capture shows Claude Code
// rejecting sessionID as already in use (see probeSessionConflict). It is
// a pure classifier with no side effect on persisted state -- deciding
// what to do about a conflict (rotate the identity and retry once) is
// refreshCoordinator.refreshWithRecovery's responsibility, not this
// function's; this only needs to produce a distinguishable error
// (wrapping errProbeSessionConflict, checked via errors.Is, never by
// re-parsing a message string). Called at more than one point in Run (see
// its own call sites) so a conflict is caught promptly in the common case
// but never missed just because it showed up a moment later than the
// first check ran.
func checkSessionConflict(capture *probeOutputCapture, sessionID string) (err error, ok bool) {
	if !probeSessionConflict(capture.String()) {
		return nil, false
	}
	return fmt.Errorf("claude usage probe: session id %s rejected as already in use: %w", sessionID, errProbeSessionConflict), true
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
// timeout elapses with neither -- a genuine non-limit failure (see Run's
// own doc comment for how each outcome is handled).
func waitForProbeOutcome(ctx context.Context, path string, capture *probeOutputCapture, after time.Time, timeout time.Duration) (probestate.Snapshot, probeLimitSignal, error) {
	deadline := time.Now().Add(timeout)
	store := probestate.NewSnapshotStore(path)
	for {
		if snap, ok, err := store.Load(); err == nil && ok && snap.ObservedAt.After(after) && snap.ResponseObserved {
			return snap, probeLimitSignal{}, nil
		}
		if sig := classifyProbeOutput(capture.String()); sig.any() {
			return probestate.Snapshot{}, sig, nil
		}
		if !time.Now().Before(deadline) {
			return probestate.Snapshot{}, probeLimitSignal{}, fmt.Errorf("claude usage probe: no fresh statusLine snapshot within %s", timeout)
		}
		select {
		case <-ctx.Done():
			return probestate.Snapshot{}, probeLimitSignal{}, ctx.Err()
		case <-time.After(usageProbePollInterval):
		}
	}
}

// detach ends a plain interactive probe session process. Unlike
// detachClaudeClient (built for `claude attach <id>`'s own documented
// Ctrl+Z detach hotkey), a brand-new `claude --session-id ...` session
// does not treat a literal Ctrl+Z byte the same way: verified against the
// installed CLI, it is instead read as an ordinary job-control suspend
// request, leaving the process permanently "suspended" (the CLI's own
// message: "Run `fg` to bring Claude Code back") rather than exiting,
// until force-killed. SIGINT is what actually works for an otherwise-idle
// session (also verified against the installed CLI); SIGTERM was tried
// too and does not reliably end the process either. A SIGKILL fallback
// here is safe, unlike a mid-turn kill (observed to leave that session's
// conversation permanently unresumable): this probe never depends on any
// given refresh's conversation surviving (see probestate.Identity's doc
// comment), so losing it costs nothing beyond this one process needing to
// be started again next refresh.
func (r *probeAttemptRunner) detach(ctx context.Context, cmd *exec.Cmd, wait <-chan error) {
	if cmd.Process == nil {
		return
	}
	// A bounded, unconditional wait (not tied to ctx) -- cleaning up this
	// process is worth attempting even if the caller's own ctx has
	// already been cancelled (e.g. agentsctl itself is exiting).
	time.Sleep(r.detachSettleDelay)
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGINT)
	if _, ok := waitForAttachment(ctx, wait, usageProbeDetachTimeout); ok {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	_, _ = waitForAttachment(ctx, wait, usageProbeDetachTimeout)
}
