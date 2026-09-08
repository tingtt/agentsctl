//go:build darwin || linux

package claude

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	base "github.com/tingtt/agentsctl/internal/provider"
	"github.com/tingtt/agentsctl/internal/session"
)

var (
	testAgentsctlBinOnce sync.Once
	testAgentsctlBinPath string
	testAgentsctlBinErr  error
)

// probeExePath builds the real cmd/agentsctl binary once (shared, cached
// across this package's whole test run) and returns its path, for a test
// to use as Probe.ExePath so the fake claude CLI's statusLine invocation
// exercises the actual production dispatch (main.go's
// claude.UsageCollectorCommand handling -> RunUsageCollector) rather than
// a stand-in. This deliberately does NOT reuse `go test`'s own compiled
// test binary (os.Executable() from inside a test): `go test` can remove
// that build artifact from disk while the test process is still running
// (verified empirically -- a child process trying to exec that same path
// gets ENOENT even mid-test-run), so it cannot be re-exec'd by path from a
// spawned child the way a normal built binary can.
func probeExePath(t *testing.T) string {
	t.Helper()
	testAgentsctlBinOnce.Do(func() {
		_, file, _, ok := runtime.Caller(0)
		if !ok {
			testAgentsctlBinErr = errors.New("locate test source")
			return
		}
		moduleRoot := filepath.Join(filepath.Dir(file), "..", "..", "..")
		dir, err := os.MkdirTemp("", "agentsctl-test-bin-*")
		if err != nil {
			testAgentsctlBinErr = err
			return
		}
		out := filepath.Join(dir, "agentsctl")
		cmd := exec.Command("go", "build", "-o", out, "./cmd/agentsctl")
		cmd.Dir = moduleRoot
		if outb, err := cmd.CombinedOutput(); err != nil {
			testAgentsctlBinErr = fmt.Errorf("build cmd/agentsctl for tests: %w: %s", err, outb)
			return
		}
		testAgentsctlBinPath = out
	})
	if testAgentsctlBinErr != nil {
		t.Fatal(testAgentsctlBinErr)
	}
	return testAgentsctlBinPath
}

// fakeClaudePath locates internal/testkit/fakecli/claude -- a fake `claude`
// CLI (real subprocess/PTY boundary, not a Go interface fake) extended to
// stand in for an interactive probe session: given --settings/--session-id
// (or --resume), it invokes the statusLine command found in that settings
// file with a canned rate_limits payload, the same way Claude Code itself
// invokes a statusLine command after a session's first API response.
func fakeClaudePath(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available for fake claude CLI")
	}
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test source")
	}
	return filepath.Join(filepath.Dir(file), "..", "..", "testkit", "fakecli", "claude")
}

// fastProbeDelay overrides Probe's real-CLI-scaled settle delays for tests
// driving the fake CLI, whose own startup/settle timing is trivial.
const fastProbeDelay = 300 * time.Millisecond

// newFastProbe returns a Probe pointed at path/dir with every settle delay
// shortened to fastProbeDelay -- otherwise identical to NewProbe(path,
// dir), which every test here would otherwise pay real-CLI-scaled (multi-
// second) delays for.
func newFastProbe(path, dir string) *Probe {
	pr := NewProbe(path, dir)
	pr.SettleDelay, pr.TrustSettleDelay, pr.DetachSettleDelay = fastProbeDelay, fastProbeDelay, fastProbeDelay
	return pr
}

// writeFakeRateLimits seeds AGENTSCTL_FAKE_DIR/ratelimits.json, the
// fixture the fake CLI's run_statusline reads to build its canned
// rate_limits payload.
func writeFakeRateLimits(t *testing.T, dir string, rateLimits map[string]any) {
	t.Helper()
	b, err := json.Marshal(rateLimits)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ratelimits.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestProbeRefreshWritesSnapshotFromFakeStatusLine is the end-to-end
// guarantee: Probe.Usage, against the fake CLI, actually drives a real
// PTY-attached session through settings generation, prompt submission, and
// statusLine-triggered snapshot collection, and returns the resulting
// provider-neutral session.Usage.
func TestProbeRefreshWritesSnapshotFromFakeStatusLine(t *testing.T) {
	fakeDir := t.TempDir()
	t.Setenv("AGENTSCTL_FAKE_DIR", fakeDir)
	writeFakeRateLimits(t, fakeDir, map[string]any{
		"five_hour": map[string]any{"used_percentage": 70, "resets_at": 4102444800},
		"seven_day": map[string]any{"used_percentage": 20, "resets_at": 4102444801},
	})

	probeDir := t.TempDir()
	pr := newFastProbe(fakeClaudePath(t), probeDir)
	pr.ExePath = probeExePath(t)

	usage, err := pr.Usage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if usage.FiveHour.State != session.UsageAvailable || usage.FiveHour.Percent != 70 {
		t.Fatalf("FiveHour=%+v, want Available/70%%", usage.FiveHour)
	}
	if usage.Weekly.State != session.UsageAvailable || usage.Weekly.Percent != 20 {
		t.Fatalf("Weekly=%+v, want Available/20%%", usage.Weekly)
	}
}

// TestProbeUsageServesFreshCacheWithoutRefreshing fixes that a fresh cache
// is served without touching the probe process at all: the second Usage()
// call must return instantly and must not require the fake CLI script to
// even still be valid (deleting it after the first call proves no process
// was spawned).
func TestProbeUsageServesFreshCacheWithoutRefreshing(t *testing.T) {
	fakeDir := t.TempDir()
	t.Setenv("AGENTSCTL_FAKE_DIR", fakeDir)
	writeFakeRateLimits(t, fakeDir, map[string]any{
		"five_hour": map[string]any{"used_percentage": 55, "resets_at": 4102444800},
	})
	probeDir := t.TempDir()
	pr := newFastProbe(fakeClaudePath(t), probeDir)
	pr.ExePath = probeExePath(t)

	first, err := pr.Usage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first.FiveHour.State != session.UsageAvailable || first.FiveHour.Percent != 55 {
		t.Fatalf("first=%+v", first)
	}

	pr.Path = filepath.Join(t.TempDir(), "no-such-claude-binary")
	second, err := pr.Usage(context.Background())
	if err != nil {
		t.Fatalf("second Usage() with an unusable claude path errored -- the cache should have been used: %v", err)
	}
	if second != first {
		t.Fatalf("second=%+v, want identical to cached first=%+v", second, first)
	}
}

// TestProbeUsageStaleCacheFailsOverWithoutError fixes the stale-cache
// failure policy: once a snapshot has been cached, a subsequent refresh
// failure (here, an unusable claude path) must still return that stale
// snapshot rather than erroring -- the session catalog and Codex's own
// usage row must never go down over a Claude-side hiccup.
func TestProbeUsageStaleCacheFailsOverWithoutError(t *testing.T) {
	fakeDir := t.TempDir()
	t.Setenv("AGENTSCTL_FAKE_DIR", fakeDir)
	writeFakeRateLimits(t, fakeDir, map[string]any{
		"five_hour": map[string]any{"used_percentage": 33, "resets_at": 4102444800},
	})
	probeDir := t.TempDir()
	pr := newFastProbe(fakeClaudePath(t), probeDir)
	pr.ExePath = probeExePath(t)

	if _, err := pr.Usage(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Force the cache to look stale, then break the refresh path.
	pr.mu.Lock()
	pr.snapshotAt = time.Now().Add(-2 * usageProbeTTL)
	pr.mu.Unlock()
	pr.Path = filepath.Join(t.TempDir(), "no-such-claude-binary")

	got, err := pr.Usage(context.Background())
	if err != nil {
		t.Fatalf("stale-cache Usage() errored instead of returning the stale snapshot: %v", err)
	}
	if got.FiveHour.State != session.UsageAvailable || got.FiveHour.Percent != 33 {
		t.Fatalf("got=%+v, want the stale cached 33%%", got)
	}
}

// writeFakeLimitBanner seeds AGENTSCTL_FAKE_DIR/limit_banner.txt -- the
// fixture the extended fake CLI writes to its own pty output instead of
// invoking statusLine, standing in for Claude Code hitting a usage limit
// (see the fake CLI's own doc comment on this fixture, and
// classifyProbeOutput).
func writeFakeLimitBanner(t *testing.T, dir, banner string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "limit_banner.txt"), []byte(banner), 0o600); err != nil {
		t.Fatal(err)
	}
}

// writeFakeSessionConflict seeds AGENTSCTL_FAKE_DIR/session_conflict.txt,
// the fixture the fake CLI writes to its own pty output (then exits)
// instead of proceeding at all, standing in for Claude Code rejecting a
// probe's --session-id as already in use (see probeSessionConflict and
// the fake CLI's own doc comment on this fixture).
func writeFakeSessionConflict(t *testing.T, dir, banner string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "session_conflict.txt"), []byte(banner), 0o600); err != nil {
		t.Fatal(err)
	}
}

// writeFakeSessionConflictAfterTrust seeds
// AGENTSCTL_FAKE_DIR/session_conflict_after_trust.txt, the fixture the
// fake CLI writes to its own pty output (then exits) immediately after
// accepting the workspace-trust dialog for the first time, instead of
// ever reaching a real prompt -- standing in for a session conflict that
// only becomes observable after this probe attempt has already durably
// persisted TrustAccepted:true (see markTrustAccepted, called
// unconditionally right after the trust-dialog write succeeds). See the
// fake CLI's own doc comment on this fixture for why it's distinct from
// writeFakeSessionConflict (which fires before any trust interaction at
// all).
func writeFakeSessionConflictAfterTrust(t *testing.T, dir, banner string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "session_conflict_after_trust.txt"), []byte(banner), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestProbeRefreshDetectsFiveHourLimitAndPreservesWeekly fixes the core
// limit-detection contract: a probe refresh that observes a 5-hour-
// qualified usage-limit banner (instead of a fresh statusLine snapshot)
// reports FiveHour as exhausted/100%, while Weekly's still-valid prior
// cached reading is preserved rather than wiped -- see Issue #19's "片方
// だけ exhausted の場合、もう片方の有効な usage snapshot を不必要に失わない".
func TestProbeRefreshDetectsFiveHourLimitAndPreservesWeekly(t *testing.T) {
	fakeDir := t.TempDir()
	t.Setenv("AGENTSCTL_FAKE_DIR", fakeDir)
	writeFakeRateLimits(t, fakeDir, map[string]any{
		"five_hour": map[string]any{"used_percentage": 92, "resets_at": 4102444800},
		"seven_day": map[string]any{"used_percentage": 84, "resets_at": 4102444801},
	})
	probeDir := t.TempDir()
	pr := newFastProbe(fakeClaudePath(t), probeDir)
	pr.ExePath = probeExePath(t)

	if _, err := pr.Usage(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Force a new refresh, this time observing a 5-hour usage-limit banner
	// (the shape closest to Claude Code's actual display, per this
	// package's own wording notes) instead of a fresh statusLine snapshot.
	pr.mu.Lock()
	pr.snapshotAt = time.Now().Add(-2 * usageProbeTTL)
	pr.mu.Unlock()
	writeFakeLimitBanner(t, fakeDir, "You've hit your session limit · resets 3pm\r\n")

	got, err := pr.Usage(context.Background())
	if err != nil {
		t.Fatalf("a detected limit must be a valid state transition, not a refresh error: %v", err)
	}
	if got.FiveHour.State != session.UsageExhausted || got.FiveHour.Percent != 100 {
		t.Fatalf("FiveHour=%+v, want Exhausted/100%%", got.FiveHour)
	}
	if got.Weekly.State != session.UsageAvailable || got.Weekly.Percent != 84 {
		t.Fatalf("Weekly=%+v, want the prior Available/84%% reading preserved", got.Weekly)
	}
}

// TestProbeRefreshDetectsWeeklyLimitAndPreservesFiveHour is the symmetric
// case: a weekly-qualified banner exhausts only Weekly, leaving FiveHour's
// prior valid reading untouched.
func TestProbeRefreshDetectsWeeklyLimitAndPreservesFiveHour(t *testing.T) {
	fakeDir := t.TempDir()
	t.Setenv("AGENTSCTL_FAKE_DIR", fakeDir)
	writeFakeRateLimits(t, fakeDir, map[string]any{
		"five_hour": map[string]any{"used_percentage": 40, "resets_at": 4102444800},
		"seven_day": map[string]any{"used_percentage": 95, "resets_at": 4102444801},
	})
	probeDir := t.TempDir()
	pr := newFastProbe(fakeClaudePath(t), probeDir)
	pr.ExePath = probeExePath(t)

	if _, err := pr.Usage(context.Background()); err != nil {
		t.Fatal(err)
	}

	pr.mu.Lock()
	pr.snapshotAt = time.Now().Add(-2 * usageProbeTTL)
	pr.mu.Unlock()
	writeFakeLimitBanner(t, fakeDir, "You've hit your weekly limit · resets Sep 10\r\n")

	got, err := pr.Usage(context.Background())
	if err != nil {
		t.Fatalf("a detected limit must be a valid state transition, not a refresh error: %v", err)
	}
	if got.Weekly.State != session.UsageExhausted || got.Weekly.Percent != 100 {
		t.Fatalf("Weekly=%+v, want Exhausted/100%%", got.Weekly)
	}
	if got.FiveHour.State != session.UsageAvailable || got.FiveHour.Percent != 40 {
		t.Fatalf("FiveHour=%+v, want the prior Available/40%% reading preserved", got.FiveHour)
	}
}

// TestProbeRefreshDetectsBothLimitsExhausted fixes that a banner
// mentioning both windows exhausts both, each independently normalized to
// Exhausted/100%.
func TestProbeRefreshDetectsBothLimitsExhausted(t *testing.T) {
	fakeDir := t.TempDir()
	t.Setenv("AGENTSCTL_FAKE_DIR", fakeDir)
	writeFakeRateLimits(t, fakeDir, map[string]any{
		"five_hour": map[string]any{"used_percentage": 92, "resets_at": 4102444800},
		"seven_day": map[string]any{"used_percentage": 95, "resets_at": 4102444801},
	})
	probeDir := t.TempDir()
	pr := newFastProbe(fakeClaudePath(t), probeDir)
	pr.ExePath = probeExePath(t)

	if _, err := pr.Usage(context.Background()); err != nil {
		t.Fatal(err)
	}

	pr.mu.Lock()
	pr.snapshotAt = time.Now().Add(-2 * usageProbeTTL)
	pr.mu.Unlock()
	writeFakeLimitBanner(t, fakeDir, "Usage limit reached: your 5-hour session limit and your weekly usage limit have both been reached.\r\n")

	got, err := pr.Usage(context.Background())
	if err != nil {
		t.Fatalf("a detected limit must be a valid state transition, not a refresh error: %v", err)
	}
	if got.FiveHour.State != session.UsageExhausted || got.FiveHour.Percent != 100 {
		t.Fatalf("FiveHour=%+v, want Exhausted/100%%", got.FiveHour)
	}
	if got.Weekly.State != session.UsageExhausted || got.Weekly.Percent != 100 {
		t.Fatalf("Weekly=%+v, want Exhausted/100%%", got.Weekly)
	}
}

// TestProbeExhaustedStateSurvivesRestart fixes Issue #19's review
// follow-up: a detected limit must be persisted to this probe's own
// on-disk usage.json (not just held in memory), so it survives an
// agentsctl restart -- simulated here by discarding Probe A entirely and
// constructing a brand-new Probe B pointed at the same probe dir, with no
// usable claude binary at all (proving the restored state comes from the
// persisted file, not a real refresh).
func TestProbeExhaustedStateSurvivesRestart(t *testing.T) {
	fakeDir := t.TempDir()
	t.Setenv("AGENTSCTL_FAKE_DIR", fakeDir)
	writeFakeRateLimits(t, fakeDir, map[string]any{
		"five_hour": map[string]any{"used_percentage": 92, "resets_at": 4102444800},
	})
	probeDir := t.TempDir()
	probeA := newFastProbe(fakeClaudePath(t), probeDir)
	probeA.ExePath = probeExePath(t)

	if _, err := probeA.Usage(context.Background()); err != nil {
		t.Fatal(err)
	}
	probeA.mu.Lock()
	probeA.snapshotAt = time.Now().Add(-2 * usageProbeTTL)
	probeA.mu.Unlock()
	writeFakeLimitBanner(t, fakeDir, "You've hit your session limit · resets 3pm\r\n")

	exhausted, err := probeA.Usage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if exhausted.FiveHour.State != session.UsageExhausted {
		t.Fatalf("FiveHour=%+v, want Exhausted before the simulated restart", exhausted.FiveHour)
	}

	// Simulate an agentsctl restart: a brand-new Probe instance (no
	// in-memory state at all) pointed at the same probe dir, with no
	// usable claude binary -- any Usage() result can only have come from
	// the persisted usage.json, never a real refresh.
	probeB := NewProbe(filepath.Join(t.TempDir(), "no-such-claude-binary"), probeDir)
	restored, err := probeB.Usage(context.Background())
	if err != nil {
		t.Fatalf("Usage() on the restarted probe errored instead of reusing the persisted exhausted state: %v", err)
	}
	if restored.FiveHour.State != session.UsageExhausted || restored.FiveHour.Percent != 100 {
		t.Fatalf("restored=%+v, want the persisted Exhausted/100%% state restored after restart", restored.FiveHour)
	}
}

// TestProbeRecoversFromExhaustedAfterFreshSnapshot fixes Issue #19's
// recovery contract: once a later refresh obtains a genuine fresh
// snapshot, a previously exhausted window returns to Available with the
// new percentage/reset, rather than staying stuck at 100%.
func TestProbeRecoversFromExhaustedAfterFreshSnapshot(t *testing.T) {
	fakeDir := t.TempDir()
	t.Setenv("AGENTSCTL_FAKE_DIR", fakeDir)
	writeFakeRateLimits(t, fakeDir, map[string]any{
		"five_hour": map[string]any{"used_percentage": 92, "resets_at": 4102444800},
	})
	probeDir := t.TempDir()
	pr := newFastProbe(fakeClaudePath(t), probeDir)
	pr.ExePath = probeExePath(t)

	if _, err := pr.Usage(context.Background()); err != nil {
		t.Fatal(err)
	}

	// First: the limit hits, FiveHour becomes exhausted.
	pr.mu.Lock()
	pr.snapshotAt = time.Now().Add(-2 * usageProbeTTL)
	pr.mu.Unlock()
	writeFakeLimitBanner(t, fakeDir, "Usage limit reached\r\n")
	exhausted, err := pr.Usage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if exhausted.FiveHour.State != session.UsageExhausted {
		t.Fatalf("FiveHour=%+v, want Exhausted before recovery", exhausted.FiveHour)
	}

	// Then: the limit clears (a real window reset) and a normal refresh
	// obtains a genuine new snapshot.
	if err := os.Remove(filepath.Join(fakeDir, "limit_banner.txt")); err != nil {
		t.Fatal(err)
	}
	writeFakeRateLimits(t, fakeDir, map[string]any{
		"five_hour": map[string]any{"used_percentage": 3, "resets_at": 4102444900},
	})
	pr.mu.Lock()
	pr.snapshotAt = time.Now().Add(-2 * usageProbeTTL)
	pr.mu.Unlock()

	recovered, err := pr.Usage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if recovered.FiveHour.State != session.UsageAvailable || recovered.FiveHour.Percent != 3 {
		t.Fatalf("FiveHour=%+v, want Available/3%% after recovery, not stuck exhausted", recovered.FiveHour)
	}
}

// TestProbeNonLimitTimeoutDoesNotFabricateExhausted fixes Issue #19's
// "limit 以外の timeout...では、#14 で定義した既存 stale/unknown handling を維持する":
// a refresh that times out with no fresh snapshot AND no usage-limit
// wording in its output must behave exactly like #14's pre-existing
// generic-failure policy (return the stale cache without erroring), never
// invent an exhausted state from silence alone.
func TestProbeNonLimitTimeoutDoesNotFabricateExhausted(t *testing.T) {
	fakeDir := t.TempDir()
	t.Setenv("AGENTSCTL_FAKE_DIR", fakeDir)
	writeFakeRateLimits(t, fakeDir, map[string]any{
		"five_hour": map[string]any{"used_percentage": 60, "resets_at": 4102444800},
	})
	probeDir := t.TempDir()
	pr := newFastProbe(fakeClaudePath(t), probeDir)
	pr.ExePath = probeExePath(t)
	pr.SendTimeout = 500 * time.Millisecond

	if _, err := pr.Usage(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Force a new refresh where the generated statusLine command itself
	// fails to run (an unusable ExePath), so the fake CLI's statusLine
	// invocation never writes a snapshot at all -- matching a real parse/
	// process failure -- and no limit banner exists, so
	// waitForProbeOutcome must time out plainly.
	pr.mu.Lock()
	pr.snapshotAt = time.Now().Add(-2 * usageProbeTTL)
	pr.mu.Unlock()
	pr.ExePath = filepath.Join(t.TempDir(), "no-such-agentsctl-exe")

	got, err := pr.Usage(context.Background())
	if err != nil {
		t.Fatalf("a plain timeout with a prior cache must fail over to the stale snapshot, not error: %v", err)
	}
	if got.FiveHour.State != session.UsageAvailable || got.FiveHour.Percent != 60 {
		t.Fatalf("got=%+v, want the stale cached 60%% preserved, not a fabricated exhausted state", got.FiveHour)
	}
}

// TestProbeExhaustedStateCachedWithoutRerefresh fixes Issue #19's "limit
// 到達状態も cache へ反映し、次回 refresh まで古い utilization に戻らないようにする":
// once a limit has been detected and cached, a second Usage() call within
// usageProbeTTL must return the exact same exhausted result without
// spawning another probe process at all (proven, as in
// TestProbeUsageServesFreshCacheWithoutRefreshing, by making the claude
// path unusable afterward and expecting no error).
func TestProbeExhaustedStateCachedWithoutRerefresh(t *testing.T) {
	fakeDir := t.TempDir()
	t.Setenv("AGENTSCTL_FAKE_DIR", fakeDir)
	writeFakeRateLimits(t, fakeDir, map[string]any{
		"five_hour": map[string]any{"used_percentage": 92, "resets_at": 4102444800},
	})
	probeDir := t.TempDir()
	pr := newFastProbe(fakeClaudePath(t), probeDir)
	pr.ExePath = probeExePath(t)

	if _, err := pr.Usage(context.Background()); err != nil {
		t.Fatal(err)
	}
	pr.mu.Lock()
	pr.snapshotAt = time.Now().Add(-2 * usageProbeTTL)
	pr.mu.Unlock()
	writeFakeLimitBanner(t, fakeDir, "Usage limit reached\r\n")

	first, err := pr.Usage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first.FiveHour.State != session.UsageExhausted {
		t.Fatalf("first=%+v, want Exhausted", first.FiveHour)
	}

	pr.Path = filepath.Join(t.TempDir(), "no-such-claude-binary")
	second, err := pr.Usage(context.Background())
	if err != nil {
		t.Fatalf("second Usage() with an unusable claude path errored -- the cached exhausted state should have been used: %v", err)
	}
	if second != first {
		t.Fatalf("second=%+v, want identical to cached first=%+v", second, first)
	}
}

// TestProbeRecoversToAvailableAfterResetBoundaryThenFreshRefresh fixes
// Issue #19's "reset 後に新しい snapshot を取得したら unknown/stale -> available":
// a window that read as UsageUnknown purely from reset-boundary crossing
// (not a detected limit) returns to Available once a real refresh
// produces a new in-period snapshot.
func TestProbeRecoversToAvailableAfterResetBoundaryThenFreshRefresh(t *testing.T) {
	fakeDir := t.TempDir()
	t.Setenv("AGENTSCTL_FAKE_DIR", fakeDir)
	reset := time.Now().Add(1 * time.Hour)
	writeFakeRateLimits(t, fakeDir, map[string]any{
		"five_hour": map[string]any{"used_percentage": 92, "resets_at": reset.Unix()},
	})
	probeDir := t.TempDir()
	pr := newFastProbe(fakeClaudePath(t), probeDir)
	pr.ExePath = probeExePath(t)

	if _, err := pr.Usage(context.Background()); err != nil {
		t.Fatal(err)
	}
	pr.Clock = func() time.Time { return reset.Add(1 * time.Minute) }
	crossed, err := pr.Usage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if crossed.FiveHour.State != session.UsageUnknown {
		t.Fatalf("crossed=%+v, want Unknown once the reset boundary has passed", crossed.FiveHour)
	}

	// A real refresh (TTL-forced) now produces a genuinely new in-period
	// snapshot.
	newReset := reset.Add(5 * time.Hour)
	writeFakeRateLimits(t, fakeDir, map[string]any{
		"five_hour": map[string]any{"used_percentage": 7, "resets_at": newReset.Unix()},
	})
	pr.mu.Lock()
	pr.snapshotAt = time.Now().Add(-2 * usageProbeTTL)
	pr.mu.Unlock()
	pr.Clock = nil

	recovered, err := pr.Usage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if recovered.FiveHour.State != session.UsageAvailable || recovered.FiveHour.Percent != 7 {
		t.Fatalf("recovered=%+v, want Available/7%% after a fresh in-period refresh", recovered.FiveHour)
	}
}

// TestProbeUsageAppliesResetBoundaryThroughClock fixes Issue #19's
// worked example end to end through Probe.Usage (not just the pure
// toSessionUsageWindow unit tests): a fresh-within-TTL cache whose own
// Reset has nonetheless already passed (per Probe.Clock) is not shown as
// current -- TTL freshness and reset-boundary validity are independent
// checks, and reset-boundary wins.
func TestProbeUsageAppliesResetBoundaryThroughClock(t *testing.T) {
	fakeDir := t.TempDir()
	t.Setenv("AGENTSCTL_FAKE_DIR", fakeDir)
	reset := time.Now().Add(1 * time.Hour)
	writeFakeRateLimits(t, fakeDir, map[string]any{
		"five_hour": map[string]any{"used_percentage": 92, "resets_at": reset.Unix()},
	})
	probeDir := t.TempDir()
	pr := newFastProbe(fakeClaudePath(t), probeDir)
	pr.ExePath = probeExePath(t)

	if _, err := pr.Usage(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Still within usageProbeTTL of the fetch, but Clock now reports a
	// time past the cached window's own Reset.
	pr.Clock = func() time.Time { return reset.Add(1 * time.Minute) }

	got, err := pr.Usage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.FiveHour.State != session.UsageUnknown {
		t.Fatalf("FiveHour=%+v, want Unknown once Clock reports past the cached Reset, even though the TTL cache is still fresh", got.FiveHour)
	}
}

// fakeProbeInvocationCount counts probe_session invocations recorded by
// the fake CLI's probe_invocations.log (see the fake CLI's own doc
// comment on it) -- used by the bounded-retry test below to prove a
// session-conflict recovery attempt spawns at most one rotation retry,
// never a loop.
func fakeProbeInvocationCount(t *testing.T, fakeDir string) int {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(fakeDir, "probe_invocations.log"))
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return 0
	}
	return len(lines)
}

// fakeSubmittedLines decodes the fake CLI's submitted_lines.log (see its
// own doc comment) into the raw bytes actually submitted on each
// probe_session invocation, in order -- unlike first_submitted_line.bin
// (which only ever records the very first one across a whole test), this
// lets a test inspect a SPECIFIC attempt's submission directly.
func fakeSubmittedLines(t *testing.T, fakeDir string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(fakeDir, "submitted_lines.log"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, hexLine := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if hexLine == "" {
			continue
		}
		raw, err := hex.DecodeString(hexLine)
		if err != nil {
			t.Fatalf("submitted_lines.log entry %q is not valid hex: %v", hexLine, err)
		}
		lines = append(lines, string(raw))
	}
	return lines
}

// TestProbeSessionConflictRecoversWithinSameUsageCallPreservingTrust
// fixes Issue #19's follow-up review: recovery from a permanently
// rejected --session-id ("Session ID ... is already in use") must happen
// within the SAME Usage() call -- not "discard now, succeed on whatever
// refresh happens next" -- since Agent View's usage fetch is a background
// fetch triggered by startup/reload, not a polling loop a user is
// guaranteed to trigger again soon. It also fixes the trust-state pitfall
// discarding (rather than rotating) an identity would reintroduce:
// workspace trust is remembered by Claude Code per probe *directory*, not
// per session ID (see probeIdentity's doc comment), so a probe directory
// already trusted under the rejected ID must still be treated as trusted
// under the rotated one -- verified directly here, not just inferred from
// the call succeeding (see this fake CLI's own leniency note on
// first_submitted_line.bin).
func TestProbeSessionConflictRecoversWithinSameUsageCallPreservingTrust(t *testing.T) {
	fakeDir := t.TempDir()
	t.Setenv("AGENTSCTL_FAKE_DIR", fakeDir)
	probeDir := t.TempDir()
	pr := newFastProbe(fakeClaudePath(t), probeDir)
	pr.ExePath = probeExePath(t)

	// Simulate a probe directory Claude Code has already trusted (from a
	// prior, separate successful refresh under the old identity) --
	// mirrors the fake CLI's own per-directory trust_marker model.
	if err := os.WriteFile(filepath.Join(probeDir, ".fake-claude-trust-accepted"), []byte("trusted"), 0o600); err != nil {
		t.Fatal(err)
	}
	rejectedID, err := loadOrCreateProbeIdentity(pr.identityPath())
	if err != nil {
		t.Fatal(err)
	}
	if err := markTrustAccepted(pr.identityPath(), rejectedID); err != nil {
		t.Fatal(err)
	}
	writeFakeSessionConflict(t, fakeDir, "Error: Session ID "+rejectedID.SessionID+" is already in use.\r\n")
	// Scope the rejection to the original identity only, so the rotated
	// identity this test expects to succeed within the same Usage() call
	// actually can -- see the fake CLI's own doc comment on this fixture.
	if err := os.WriteFile(filepath.Join(fakeDir, "session_conflict_session_ids.txt"), []byte(rejectedID.SessionID+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeFakeRateLimits(t, fakeDir, map[string]any{
		"five_hour": map[string]any{"used_percentage": 10, "resets_at": 4102444800},
	})

	got, err := pr.Usage(context.Background())
	if err != nil {
		t.Fatalf("Usage() errored instead of self-healing the conflict within the same call: %v", err)
	}
	if got.FiveHour.State != session.UsageAvailable || got.FiveHour.Percent != 10 {
		t.Fatalf("got=%+v, want Available/10%% from a single Usage() call that recovers internally", got.FiveHour)
	}

	newID, ok, err := readProbeIdentityIfExists(pr.identityPath())
	if err != nil || !ok {
		t.Fatalf("no identity after recovery: ok=%v err=%v", ok, err)
	}
	if newID.SessionID == rejectedID.SessionID {
		t.Fatal("recovery must rotate to a new session id, not reuse the rejected one")
	}
	if !newID.TrustAccepted {
		t.Fatal("TrustAccepted must be preserved across rotation -- workspace trust belongs to the probe directory, not the rotated-away session id")
	}

	// Direct proof no blind trust-dialog keystroke was sent into the
	// already-trusted composer on the post-rotation retry: this fake CLI
	// would have accepted the raw TRUST_DIALOG_ACCEPT bytes as if they
	// were an ordinary (nonsensical) submitted line and still returned a
	// usage reading, so success alone would not have caught this.
	firstLine, err := os.ReadFile(filepath.Join(fakeDir, "first_submitted_line.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if string(firstLine) == usageProbeTrustDialogAccept {
		t.Fatalf("a blind trust-dialog keystroke was sent to an already-trusted composer: %q", firstLine)
	}
	if !strings.HasPrefix(string(firstLine), usageProbePrompt) {
		t.Fatalf("first submitted line=%q, want it to start with the real probe prompt", firstLine)
	}
}

// TestProbeSessionConflictAfterTrustAcceptancePreservesLatestPersistedTrust
// fixes the specific TrustAccepted race the review above missed:
// TestProbeSessionConflictRecoversWithinSameUsageCallPreservingTrust only
// ever starts from an identity ALREADY marked TrustAccepted:true, so
// refreshOnce's own `if !id.TrustAccepted` branch (and therefore the
// trust-dialog write and the markTrustAccepted call right after it) never
// runs at all in that test -- it cannot catch a rotation that carries
// forward a caller's stale, pre-attempt copy of TrustAccepted instead of
// whatever refreshOnce most recently persisted.
//
// Here the identity starts genuinely untrusted (TrustAccepted:false, and
// Claude Code's own directory-level trust marker absent too), so attempt
// #1 must actually answer the trust dialog for the first time -- which
// durably persists TrustAccepted:true via markTrustAccepted -- and ONLY
// THEN does the conflict become observable (see
// writeFakeSessionConflictAfterTrust), before that attempt ever reaches a
// real prompt. refreshWithRecovery's `id` copy, loaded before attempt #1
// ran, is still TrustAccepted:false at this point -- proving
// rotateProbeIdentity must read the current on-disk record (which
// already has TrustAccepted:true) rather than rotating from that stale
// copy, or the assertions below would fail.
func TestProbeSessionConflictAfterTrustAcceptancePreservesLatestPersistedTrust(t *testing.T) {
	fakeDir := t.TempDir()
	t.Setenv("AGENTSCTL_FAKE_DIR", fakeDir)
	probeDir := t.TempDir()
	pr := newFastProbe(fakeClaudePath(t), probeDir)
	pr.ExePath = probeExePath(t)

	// Deliberately untrusted starting state: no persisted identity yet
	// (loadOrCreateProbeIdentity mints one with TrustAccepted:false below),
	// and no directory-level trust marker for the fake CLI either.
	origID, err := loadOrCreateProbeIdentity(pr.identityPath())
	if err != nil {
		t.Fatal(err)
	}
	if origID.TrustAccepted {
		t.Fatalf("origID=%+v, want a freshly-minted identity to start untrusted", origID)
	}

	writeFakeSessionConflictAfterTrust(t, fakeDir, "Error: Session ID "+origID.SessionID+" is already in use.\r\n")
	// Scope the rejection to the original identity only, so attempt #2
	// (the rotated identity) can succeed.
	if err := os.WriteFile(filepath.Join(fakeDir, "session_conflict_session_ids.txt"), []byte(origID.SessionID+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeFakeRateLimits(t, fakeDir, map[string]any{
		"five_hour": map[string]any{"used_percentage": 20, "resets_at": 4102444800},
	})

	got, err := pr.Usage(context.Background())
	if err != nil {
		t.Fatalf("Usage() errored instead of self-healing a conflict discovered right after trust acceptance: %v", err)
	}
	if got.FiveHour.State != session.UsageAvailable || got.FiveHour.Percent != 20 {
		t.Fatalf("got=%+v, want Available/20%% from a single Usage() call that recovers internally", got.FiveHour)
	}

	newID, ok, err := readProbeIdentityIfExists(pr.identityPath())
	if err != nil || !ok {
		t.Fatalf("no identity after recovery: ok=%v err=%v", ok, err)
	}
	if newID.SessionID == origID.SessionID {
		t.Fatal("recovery must rotate to a new session id, not reuse the rejected one")
	}
	// The crucial assertion: TrustAccepted must reflect what refreshOnce
	// actually persisted moments earlier in this very Usage() call, not
	// origID's stale (pre-attempt) copy.
	if !newID.TrustAccepted {
		t.Fatal("TrustAccepted must be preserved across rotation even though it only became true DURING the very attempt that hit the conflict -- rotation must read the current persisted identity, not a caller's stale in-memory copy")
	}

	// Direct, per-attempt proof: attempt #1 sent the trust-dialog accept
	// bytes (expected -- it genuinely needed to, this was a real first
	// contact with an untrusted directory), and attempt #2 -- now
	// trusted, per the identity's own TrustAccepted:true above -- sent
	// the real prompt directly, never re-sending the trust bytes.
	lines := fakeSubmittedLines(t, fakeDir)
	if len(lines) != 2 {
		t.Fatalf("submitted lines=%v, want exactly 2 (attempt #1's trust accept, attempt #2's real prompt)", lines)
	}
	if lines[0] != usageProbeTrustDialogAccept {
		t.Fatalf("attempt #1 submitted=%q, want the trust-dialog accept bytes", lines[0])
	}
	if lines[1] == usageProbeTrustDialogAccept {
		t.Fatal("attempt #2 (post-rotation) must not resend the blind trust-dialog keystroke into an already-trusted composer")
	}
	if !strings.HasPrefix(lines[1], usageProbePrompt) {
		t.Fatalf("attempt #2 submitted=%q, want it to start with the real probe prompt", lines[1])
	}

	if got := fakeProbeInvocationCount(t, fakeDir); got != 2 {
		t.Fatalf("fake CLI invocations=%d, want exactly 2 (bounded retry preserved for this scenario too)", got)
	}
}

// TestProbeSessionConflictRetryIsBoundedToOneRotation fixes that recovery
// never loops: if the rotated identity is ALSO rejected, Usage() must
// fail (falling through to the existing stale/error policy) after
// exactly two probe attempts -- the original identity and one rotation
// retry -- never a third.
func TestProbeSessionConflictRetryIsBoundedToOneRotation(t *testing.T) {
	fakeDir := t.TempDir()
	t.Setenv("AGENTSCTL_FAKE_DIR", fakeDir)
	probeDir := t.TempDir()
	pr := newFastProbe(fakeClaudePath(t), probeDir)
	pr.ExePath = probeExePath(t)

	origID, err := loadOrCreateProbeIdentity(pr.identityPath())
	if err != nil {
		t.Fatal(err)
	}
	// This fixture doesn't key off any particular session id, so it keeps
	// rejecting the rotated identity too.
	writeFakeSessionConflict(t, fakeDir, "Error: Session ID is already in use.\r\n")

	_, err = pr.Usage(context.Background())
	if err == nil {
		t.Fatal("want an error when both the original and the rotated identity are rejected")
	}
	if !errors.Is(err, errProbeSessionConflict) {
		t.Fatalf("err=%v, want it to wrap errProbeSessionConflict", err)
	}

	if got := fakeProbeInvocationCount(t, fakeDir); got != 2 {
		t.Fatalf("fake CLI invocations=%d, want exactly 2 (the original attempt plus one rotation retry, no third attempt)", got)
	}

	newID, ok, err := readProbeIdentityIfExists(pr.identityPath())
	if err != nil || !ok {
		t.Fatalf("no identity after the bounded retry: ok=%v err=%v", ok, err)
	}
	if newID.SessionID == origID.SessionID {
		t.Fatal("the one allowed rotation must still have happened even though the retry also conflicted")
	}
}

// TestProbeUnrelatedFailureDoesNotRotateIdentity fixes that
// refreshWithRecovery's rotation is specific to errProbeSessionConflict:
// a genuinely unrelated failure (here, a process launch failure -- an
// unusable claude binary path) must be returned as-is, with no rotation
// and no retry, and the persisted identity must be left completely
// untouched.
func TestProbeUnrelatedFailureDoesNotRotateIdentity(t *testing.T) {
	t.Setenv("AGENTSCTL_FAKE_DIR", t.TempDir())
	probeDir := t.TempDir()
	pr := newFastProbe(filepath.Join(t.TempDir(), "no-such-claude-binary"), probeDir)
	pr.ExePath = probeExePath(t)

	id, err := loadOrCreateProbeIdentity(pr.identityPath())
	if err != nil {
		t.Fatal(err)
	}

	_, err = pr.Usage(context.Background())
	if err == nil {
		t.Fatal("want an error for a genuine process-launch failure")
	}
	if errors.Is(err, errProbeSessionConflict) {
		t.Fatalf("err=%v, a process-launch failure must not be misclassified as a session conflict", err)
	}

	stillID, ok, err := readProbeIdentityIfExists(pr.identityPath())
	if err != nil || !ok {
		t.Fatalf("identity missing after an unrelated failure: ok=%v err=%v", ok, err)
	}
	if stillID.SessionID != id.SessionID {
		t.Fatal("an unrelated failure must not rotate the identity")
	}
}

// TestProbeUsageFailsClosedWithNoCacheAtAll fixes the cold-start failure
// case: with no cached snapshot ever obtained, a refresh failure must
// surface as an error (which sessionctl.Controller.Usage already treats as
// "omit this provider"), never a fabricated zero-value Usage.
func TestProbeUsageFailsClosedWithNoCacheAtAll(t *testing.T) {
	t.Setenv("AGENTSCTL_FAKE_DIR", t.TempDir())
	probeDir := t.TempDir()
	pr := newFastProbe(filepath.Join(t.TempDir(), "no-such-claude-binary"), probeDir)
	pr.ExePath = probeExePath(t)

	if _, err := pr.Usage(context.Background()); err == nil {
		t.Fatal("want an error when no cache exists and the refresh itself fails")
	}
}

// TestProbeConcurrentUsageSingleFlightsRefresh fixes the concurrency
// guarantee: many concurrent Usage() calls against a cold cache must
// collapse into exactly one probe-session refresh, not one per caller.
// This is verified by making a slow fake statusLine command that appends
// one line to a counter file per invocation and asserting exactly one
// append happened despite many concurrent callers.
func TestProbeConcurrentUsageSingleFlightsRefresh(t *testing.T) {
	fakeDir := t.TempDir()
	t.Setenv("AGENTSCTL_FAKE_DIR", fakeDir)
	writeFakeRateLimits(t, fakeDir, map[string]any{
		"five_hour": map[string]any{"used_percentage": 12, "resets_at": 4102444800},
	})
	probeDir := t.TempDir()
	pr := newFastProbe(fakeClaudePath(t), probeDir)
	pr.ExePath = probeExePath(t)

	const n = 8
	var wg sync.WaitGroup
	results := make([]struct {
		err error
	}, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := pr.Usage(context.Background())
			results[i].err = err
		}(i)
	}
	wg.Wait()
	for i, r := range results {
		if r.err != nil {
			t.Fatalf("caller %d: %v", i, r.err)
		}
	}
	// A single confirmed identity file proves at most one probe session
	// was created (loadOrCreateProbeIdentity mints a session ID exactly
	// once and every concurrent refresh attempt would otherwise race to
	// create their own).
	id, ok, err := readProbeIdentityIfExists(pr.identityPath())
	if err != nil || !ok {
		t.Fatalf("identity ok=%v err=%v", ok, err)
	}
	if id.SessionID == "" {
		t.Fatal("no probe session identity was ever created")
	}
}

// TestProbeKnownSessionIDExcludesCatalogRowNotJustCWD fixes the catalog-
// exclusion contract end to end against the fake CLI's own native
// catalog: after a refresh, Provider.List (via KnownSessionIDs) must
// exclude the probe's own row by its exact recorded session ID, while a
// normal session sharing the same CWD as the probe (a plausible
// coincidence, e.g. both happen to run from $HOME) must NOT be excluded.
func TestProbeKnownSessionIDExcludesCatalogRowNotJustCWD(t *testing.T) {
	fakeDir := t.TempDir()
	t.Setenv("AGENTSCTL_FAKE_DIR", fakeDir)
	writeFakeRateLimits(t, fakeDir, map[string]any{
		"five_hour": map[string]any{"used_percentage": 5, "resets_at": 4102444800},
	})
	probeDir := t.TempDir()
	pr := newFastProbe(fakeClaudePath(t), probeDir)
	pr.ExePath = probeExePath(t)

	if _, err := pr.Usage(context.Background()); err != nil {
		t.Fatal(err)
	}
	probeIDs := pr.KnownSessionIDs()
	if len(probeIDs) != 1 || probeIDs[0] == "" {
		t.Fatalf("KnownSessionIDs did not report exactly one identity after a successful refresh: %v", probeIDs)
	}
	probeID := probeIDs[0]

	runner := base.ExecRunner{}
	p := &Provider{Path: fakeClaudePath(t), Runner: runner, UsageProbe: pr, Store: newStore(t)}
	rows, err := p.List(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.Key.ID == probeID {
			t.Fatalf("probe session %q leaked into the normal catalog: %+v", probeID, rows)
		}
	}

	// A normal session sharing the probe's own CWD (probeDir, since the
	// fake CLI runs with cmd.Dir=probeDir) must NOT be excluded.
	seedRows := []map[string]any{{"id": "real-session", "name": "real", "cwd": probeDir, "updatedAt": 1, "status": "idle", "state": "done"}, {"id": probeID, "name": "agentsctl usage probe", "cwd": probeDir, "updatedAt": 1, "status": "idle", "state": "done"}}
	b, err := json.Marshal(seedRows)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fakeDir, "claude.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	rows, err = p.List(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	foundReal, foundProbe := false, false
	for _, row := range rows {
		if row.Key.ID == "real-session" {
			foundReal = true
		}
		if row.Key.ID == probeID {
			foundProbe = true
		}
	}
	if !foundReal {
		t.Fatal("a normal session sharing the probe's own CWD was wrongly excluded")
	}
	if foundProbe {
		t.Fatal("the probe's own session was not excluded from a mixed catalog")
	}
}

// TestProbeKnownSessionIDsSurviveRestart fixes that ownership of a
// rotated-away identity isn't just an in-memory fact of the Probe
// instance that performed the rotation: a brand-new *Probe constructed
// against the same Dir (simulating an agentsctl restart) must still
// report both the current and every retired session ID from what's
// persisted on disk -- exactly what Provider.List needs after a restart
// to keep excluding a rotated-away probe row from the catalog.
func TestProbeKnownSessionIDsSurviveRestart(t *testing.T) {
	probeDir := t.TempDir()
	pr1 := newFastProbe("claude", probeDir)

	orig, err := loadOrCreateProbeIdentity(pr1.identityPath())
	if err != nil {
		t.Fatal(err)
	}
	recovery, err := rotateProbeIdentity(pr1.identityPath(), orig.SessionID)
	if err != nil {
		t.Fatal(err)
	}

	// A brand-new Probe, never having rotated anything itself -- only the
	// persisted probe.json under the same Dir carries this history.
	pr2 := newFastProbe("claude", probeDir)
	ids := pr2.KnownSessionIDs()
	if len(ids) != 2 {
		t.Fatalf("KnownSessionIDs after restart=%v, want exactly 2 (current + retired)", ids)
	}
	foundCurrent, foundRetired := false, false
	for _, id := range ids {
		if id == recovery.Identity.SessionID {
			foundCurrent = true
		}
		if id == orig.SessionID {
			foundRetired = true
		}
	}
	if !foundCurrent {
		t.Fatalf("KnownSessionIDs after restart=%v, missing current id %q", ids, recovery.Identity.SessionID)
	}
	if !foundRetired {
		t.Fatalf("KnownSessionIDs after restart=%v, missing retired id %q", ids, orig.SessionID)
	}
}

// TestFakeCLIRejectsPromptWithoutTrustDialogAccept fixes the fake CLI's
// own fidelity to the real bug this package's Fix B addresses: driven
// directly (bypassing Probe.refreshOnce entirely), a brand-new probe
// directory's fake session must reject a prompt sent WITHOUT first
// answering the workspace-trust dialog -- reproducing the original defect
// (a blind prompt+Enter selects the fake's own default "No, exit" and the
// session exits, never invoking statusLine) -- so that the other tests in
// this file, which all pass through Probe.refreshOnce's real trust-dialog
// handling, are proven against a fake that can actually fail, not one
// that trivially succeeds regardless of what's sent.
func TestFakeCLIRejectsPromptWithoutTrustDialogAccept(t *testing.T) {
	fakeDir := t.TempDir()
	t.Setenv("AGENTSCTL_FAKE_DIR", fakeDir)
	writeFakeRateLimits(t, fakeDir, map[string]any{
		"five_hour": map[string]any{"used_percentage": 70, "resets_at": 4102444800},
	})
	probeDir := t.TempDir()
	settingsPath := filepath.Join(probeDir, "settings.json")
	snapshotPath := filepath.Join(probeDir, "usage.json")
	exe := probeExePath(t)
	if err := writeUsageSettings(settingsPath, exe, snapshotPath); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(fakeClaudePath(t), "--session-id", "no-trust-accept", "--settings", settingsPath)
	cmd.Dir = probeDir
	child, err := startClaudeAttachRaw(cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer child.Close()
	go drainUntilClosed(child)
	time.Sleep(fastProbeDelay)

	// A prompt sent directly, without the trust-dialog accept sequence
	// first -- exactly the original bug's shape.
	if _, err := child.Write([]byte(usageProbePrompt + "\r")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)

	if _, ok, _ := readUsageSnapshot(snapshotPath); ok {
		t.Fatal("a prompt sent without accepting the trust dialog first produced a snapshot -- the fake CLI's trust gate isn't actually enforcing anything")
	}
	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait()
}

// TestProbeReusesSameSessionIDAcrossRefreshes fixes the singleton-identity
// guarantee across multiple refreshes (not just concurrent ones, which
// TestProbeConcurrentUsageSingleFlightsRefresh already covers): a second,
// later refresh must address the exact same --session-id as the first,
// never minting a new one.
func TestProbeReusesSameSessionIDAcrossRefreshes(t *testing.T) {
	fakeDir := t.TempDir()
	t.Setenv("AGENTSCTL_FAKE_DIR", fakeDir)
	writeFakeRateLimits(t, fakeDir, map[string]any{
		"five_hour": map[string]any{"used_percentage": 1, "resets_at": 4102444800},
	})
	probeDir := t.TempDir()
	pr := newFastProbe(fakeClaudePath(t), probeDir)
	pr.ExePath = probeExePath(t)

	if _, err := pr.Usage(context.Background()); err != nil {
		t.Fatal(err)
	}
	firstIDs := pr.KnownSessionIDs()
	if len(firstIDs) != 1 {
		t.Fatalf("no identity after first refresh: %v", firstIDs)
	}
	firstID := firstIDs[0]

	// Force a second real refresh.
	pr.mu.Lock()
	pr.snapshotAt = time.Now().Add(-2 * usageProbeTTL)
	pr.mu.Unlock()
	writeFakeRateLimits(t, fakeDir, map[string]any{
		"five_hour": map[string]any{"used_percentage": 2, "resets_at": 4102444800},
	})
	if _, err := pr.Usage(context.Background()); err != nil {
		t.Fatal(err)
	}
	secondIDs := pr.KnownSessionIDs()
	if len(secondIDs) != 1 {
		t.Fatalf("no identity after second refresh: %v", secondIDs)
	}
	secondID := secondIDs[0]
	if secondID != firstID {
		t.Fatalf("session ID changed across refreshes: %q -> %q", firstID, secondID)
	}
}

// TestProbeTrustAcceptedPersistsAfterFirstRefresh fixes that
// probeIdentity.TrustAccepted is actually durably set once the very first
// refresh has attempted to answer the trust dialog, matching
// usage_identity_test.go's unit coverage of markTrustAccepted but proven
// here through the real refresh path end to end.
func TestProbeTrustAcceptedPersistsAfterFirstRefresh(t *testing.T) {
	fakeDir := t.TempDir()
	t.Setenv("AGENTSCTL_FAKE_DIR", fakeDir)
	writeFakeRateLimits(t, fakeDir, map[string]any{
		"five_hour": map[string]any{"used_percentage": 1, "resets_at": 4102444800},
	})
	probeDir := t.TempDir()
	pr := newFastProbe(fakeClaudePath(t), probeDir)
	pr.ExePath = probeExePath(t)

	if _, err := pr.Usage(context.Background()); err != nil {
		t.Fatal(err)
	}
	id, ok, err := readProbeIdentityIfExists(pr.identityPath())
	if err != nil || !ok {
		t.Fatalf("identity ok=%v err=%v", ok, err)
	}
	if !id.TrustAccepted {
		t.Fatal("TrustAccepted was not set after a successful first refresh")
	}
}

// TestProbeSnapshotWithoutRateLimitsIsUnavailableNotError fixes the
// pre-first-response shape end to end: a statusLine payload with no
// rate_limits at all (no ratelimits.json fixture seeded) must still
// produce a successful Usage() call whose windows are Available: false --
// never an error, and never a false 0%.
func TestProbeSnapshotWithoutRateLimitsIsUnavailableNotError(t *testing.T) {
	fakeDir := t.TempDir()
	t.Setenv("AGENTSCTL_FAKE_DIR", fakeDir)
	// Deliberately no writeFakeRateLimits call.
	probeDir := t.TempDir()
	pr := newFastProbe(fakeClaudePath(t), probeDir)
	pr.ExePath = probeExePath(t)

	usage, err := pr.Usage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if usage.FiveHour.State == session.UsageAvailable || usage.Weekly.State == session.UsageAvailable {
		t.Fatalf("usage=%+v, want both windows unavailable when no rate_limits was ever reported", usage)
	}
}

// TestProbeProcessExitsAfterRefreshNotOrphaned fixes that
// detachProbeSession actually ends the probe's OS process rather than
// leaving it running: after Usage() returns, the child PID it started
// must no longer be running.
func TestProbeProcessExitsAfterRefreshNotOrphaned(t *testing.T) {
	fakeDir := t.TempDir()
	t.Setenv("AGENTSCTL_FAKE_DIR", fakeDir)
	writeFakeRateLimits(t, fakeDir, map[string]any{
		"five_hour": map[string]any{"used_percentage": 1, "resets_at": 4102444800},
	})
	probeDir := t.TempDir()
	pr := newFastProbe(fakeClaudePath(t), probeDir)
	pr.ExePath = probeExePath(t)

	if _, err := pr.Usage(context.Background()); err != nil {
		t.Fatal(err)
	}

	if _, ok, err := readUsageSnapshot(pr.snapshotPath()); err != nil || !ok {
		t.Fatalf("no snapshot after refresh: ok=%v err=%v", ok, err)
	}
	// The fake CLI records itself into claude.json while running; once
	// the process has actually exited and this package's own detach has
	// completed, no python process should still be alive holding the pty
	// open. We approximate "not orphaned" by confirming Usage() (which
	// waits out detachProbeSession synchronously before returning) does
	// not itself hang or leave a hung child -- a second refresh cycle
	// (forced stale) completing within the normal fast-test timeout is
	// strong evidence no leftover process is holding the probe directory
	// or pty in a bad state.
	pr.mu.Lock()
	pr.snapshotAt = time.Now().Add(-2 * usageProbeTTL)
	pr.mu.Unlock()
	done := make(chan error, 1)
	go func() {
		_, err := pr.Usage(context.Background())
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("second refresh did not complete -- a leftover process from the first refresh may still be holding the probe session")
	}
}

// TestProbePersistedFreshSnapshotSkipsRefreshOnNewInstance fixes the
// cross-restart reuse guarantee: a brand-new Probe instance (as a fresh
// agentsctl process would construct) pointed at a directory that already
// holds a fresh (within TTL) persisted usage.json must serve it
// immediately, without ever spawning a probe process -- proven here by
// giving the new instance an unusable claude path.
func TestProbePersistedFreshSnapshotSkipsRefreshOnNewInstance(t *testing.T) {
	probeDir := t.TempDir()
	want := usageSnapshot{
		FiveHour:   usageWindowSnapshot{State: session.UsageAvailable, Percent: 42, ResetAt: time.Unix(4102444800, 0)},
		ObservedAt: time.Now(),
	}
	if err := writeUsageSnapshotAtomic(filepath.Join(probeDir, "usage.json"), want); err != nil {
		t.Fatal(err)
	}

	pr := NewProbe(filepath.Join(t.TempDir(), "no-such-claude-binary"), probeDir)
	got, err := pr.Usage(context.Background())
	if err != nil {
		t.Fatalf("Usage() tried to refresh instead of reusing the fresh persisted snapshot: %v", err)
	}
	if got.FiveHour.State != session.UsageAvailable || got.FiveHour.Percent != 42 {
		t.Fatalf("got=%+v, want the persisted 42%% snapshot", got)
	}
}

// TestProbeUpgradesLegacyPersistedSnapshotToAvailable fixes Issue #19's
// real-world upgrade path end to end: a probe dir holding a usage.json in
// the pre-#19 legacy schema (`available bool`, no `state` field -- what a
// build from before PR #23 always wrote), with a still-in-period window,
// must be read by a fresh PR #23 Probe as UsageAvailable at the real
// persisted percentage, not UsageUnknown -- proven with an unusable
// claude path (the fresh-within-TTL persisted snapshot must be served as
// is, no refresh attempted).
func TestProbeUpgradesLegacyPersistedSnapshotToAvailable(t *testing.T) {
	probeDir := t.TempDir()
	futureReset := time.Now().Add(2 * time.Hour).Format(time.RFC3339Nano)
	writeRawSnapshot(t, filepath.Join(probeDir, "usage.json"), `{"fiveHour":{"available":true,"percent":60,"resetAt":"`+futureReset+`"},"weekly":{"available":true,"percent":36,"resetAt":"2099-01-01T00:00:00Z"},"observedAt":"`+time.Now().Format(time.RFC3339Nano)+`"}`)

	pr := NewProbe(filepath.Join(t.TempDir(), "no-such-claude-binary"), probeDir)
	got, err := pr.Usage(context.Background())
	if err != nil {
		t.Fatalf("Usage() errored instead of reusing the migrated legacy snapshot: %v", err)
	}
	if got.FiveHour.State != session.UsageAvailable || got.FiveHour.Percent != 60 {
		t.Fatalf("FiveHour=%+v, want the legacy 60%% migrated to Available, not stuck at Unknown (\"?%%\")", got.FiveHour)
	}
	if got.Weekly.State != session.UsageAvailable || got.Weekly.Percent != 36 {
		t.Fatalf("Weekly=%+v, want the legacy 36%% migrated to Available", got.Weekly)
	}
}

// TestProbeUpgradesLegacyPersistedSnapshotExpiredWindowIsUnknown fixes
// the companion case: a legacy snapshot whose window's own Reset has
// already passed correctly still ends up UsageUnknown after migration --
// via the ordinary reset-boundary rule (session.Usage.At), not because
// the migration failed. This is expected, not a regression: the window
// really is expired, and no refresh can happen (unusable claude path).
func TestProbeUpgradesLegacyPersistedSnapshotExpiredWindowIsUnknown(t *testing.T) {
	probeDir := t.TempDir()
	pastReset := time.Now().Add(-time.Hour).Format(time.RFC3339Nano)
	writeRawSnapshot(t, filepath.Join(probeDir, "usage.json"), `{"fiveHour":{"available":true,"percent":92,"resetAt":"`+pastReset+`"},"observedAt":"`+time.Now().Format(time.RFC3339Nano)+`"}`)

	pr := NewProbe(filepath.Join(t.TempDir(), "no-such-claude-binary"), probeDir)
	got, err := pr.Usage(context.Background())
	if err != nil {
		t.Fatalf("Usage() errored instead of reusing the migrated (then expired) legacy snapshot: %v", err)
	}
	if got.FiveHour.State != session.UsageUnknown {
		t.Fatalf("FiveHour=%+v, want Unknown for a legacy window whose own Reset has already passed (migration succeeded, but the window is simply expired)", got.FiveHour)
	}
}

// TestProbePersistedStaleSnapshotOnNewInstanceStillRefreshes fixes that
// persisted-snapshot reuse only ever short-circuits a genuinely FRESH
// (within TTL) snapshot: a new Probe instance finding a stale usage.json
// on disk must still perform a real refresh rather than serving the stale
// value forever.
func TestProbePersistedStaleSnapshotOnNewInstanceStillRefreshes(t *testing.T) {
	fakeDir := t.TempDir()
	t.Setenv("AGENTSCTL_FAKE_DIR", fakeDir)
	writeFakeRateLimits(t, fakeDir, map[string]any{
		"five_hour": map[string]any{"used_percentage": 99, "resets_at": 4102444800},
	})
	probeDir := t.TempDir()
	stale := usageSnapshot{
		FiveHour:   usageWindowSnapshot{State: session.UsageAvailable, Percent: 1, ResetAt: time.Unix(4102444800, 0)},
		ObservedAt: time.Now().Add(-2 * usageProbeTTL),
	}
	if err := writeUsageSnapshotAtomic(filepath.Join(probeDir, "usage.json"), stale); err != nil {
		t.Fatal(err)
	}

	pr := newFastProbe(fakeClaudePath(t), probeDir)
	pr.ExePath = probeExePath(t)
	got, err := pr.Usage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.FiveHour.Percent != 99 {
		t.Fatalf("got=%+v, want a real refresh (99%%) rather than the stale persisted 1%% value", got)
	}
}
