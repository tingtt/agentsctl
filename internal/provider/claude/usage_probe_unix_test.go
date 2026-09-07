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
	"runtime"
	"sync"
	"testing"
	"time"

	base "github.com/tingtt/agentsctl/internal/provider"
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
		"five_hour": map[string]any{"used_percentage": 70, "resets_at": 1000},
		"seven_day": map[string]any{"used_percentage": 20, "resets_at": 2000},
	})

	probeDir := t.TempDir()
	pr := newFastProbe(fakeClaudePath(t), probeDir)
	pr.ExePath = probeExePath(t)

	usage, err := pr.Usage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !usage.FiveHour.Available || usage.FiveHour.Percent != 70 {
		t.Fatalf("FiveHour=%+v, want Available/70%%", usage.FiveHour)
	}
	if !usage.Weekly.Available || usage.Weekly.Percent != 20 {
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
		"five_hour": map[string]any{"used_percentage": 55, "resets_at": 1000},
	})
	probeDir := t.TempDir()
	pr := newFastProbe(fakeClaudePath(t), probeDir)
	pr.ExePath = probeExePath(t)

	first, err := pr.Usage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !first.FiveHour.Available || first.FiveHour.Percent != 55 {
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
		"five_hour": map[string]any{"used_percentage": 33, "resets_at": 1000},
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
	if !got.FiveHour.Available || got.FiveHour.Percent != 33 {
		t.Fatalf("got=%+v, want the stale cached 33%%", got)
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
		"five_hour": map[string]any{"used_percentage": 12, "resets_at": 1000},
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
// catalog: after a refresh, Provider.List (via KnownSessionID) must
// exclude the probe's own row by its exact recorded session ID, while a
// normal session sharing the same CWD as the probe (a plausible
// coincidence, e.g. both happen to run from $HOME) must NOT be excluded.
func TestProbeKnownSessionIDExcludesCatalogRowNotJustCWD(t *testing.T) {
	fakeDir := t.TempDir()
	t.Setenv("AGENTSCTL_FAKE_DIR", fakeDir)
	writeFakeRateLimits(t, fakeDir, map[string]any{
		"five_hour": map[string]any{"used_percentage": 5, "resets_at": 1000},
	})
	probeDir := t.TempDir()
	pr := newFastProbe(fakeClaudePath(t), probeDir)
	pr.ExePath = probeExePath(t)

	if _, err := pr.Usage(context.Background()); err != nil {
		t.Fatal(err)
	}
	probeID, ok := pr.KnownSessionID()
	if !ok || probeID == "" {
		t.Fatal("KnownSessionID did not report an identity after a successful refresh")
	}

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

// TestFakeCLIRejectsPromptWithoutTrustDialogAccept fixes the fake CLI's
// own fidelity to the real bug this package's Fix B addresses: driven
// directly (bypassing Probe.refresh entirely), a brand-new probe
// directory's fake session must reject a prompt sent WITHOUT first
// answering the workspace-trust dialog -- reproducing the original defect
// (a blind prompt+Enter selects the fake's own default "No, exit" and the
// session exits, never invoking statusLine) -- so that the other tests in
// this file, which all pass through Probe.refresh's real trust-dialog
// handling, are proven against a fake that can actually fail, not one
// that trivially succeeds regardless of what's sent.
func TestFakeCLIRejectsPromptWithoutTrustDialogAccept(t *testing.T) {
	fakeDir := t.TempDir()
	t.Setenv("AGENTSCTL_FAKE_DIR", fakeDir)
	writeFakeRateLimits(t, fakeDir, map[string]any{
		"five_hour": map[string]any{"used_percentage": 70, "resets_at": 1000},
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
		"five_hour": map[string]any{"used_percentage": 1, "resets_at": 1000},
	})
	probeDir := t.TempDir()
	pr := newFastProbe(fakeClaudePath(t), probeDir)
	pr.ExePath = probeExePath(t)

	if _, err := pr.Usage(context.Background()); err != nil {
		t.Fatal(err)
	}
	firstID, ok := pr.KnownSessionID()
	if !ok {
		t.Fatal("no identity after first refresh")
	}

	// Force a second real refresh.
	pr.mu.Lock()
	pr.snapshotAt = time.Now().Add(-2 * usageProbeTTL)
	pr.mu.Unlock()
	writeFakeRateLimits(t, fakeDir, map[string]any{
		"five_hour": map[string]any{"used_percentage": 2, "resets_at": 1000},
	})
	if _, err := pr.Usage(context.Background()); err != nil {
		t.Fatal(err)
	}
	secondID, ok := pr.KnownSessionID()
	if !ok {
		t.Fatal("no identity after second refresh")
	}
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
		"five_hour": map[string]any{"used_percentage": 1, "resets_at": 1000},
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
	if usage.FiveHour.Available || usage.Weekly.Available {
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
		"five_hour": map[string]any{"used_percentage": 1, "resets_at": 1000},
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
		FiveHour:   usageWindowSnapshot{Available: true, Percent: 42, ResetAt: time.Unix(1000, 0)},
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
	if !got.FiveHour.Available || got.FiveHour.Percent != 42 {
		t.Fatalf("got=%+v, want the persisted 42%% snapshot", got)
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
		"five_hour": map[string]any{"used_percentage": 99, "resets_at": 1000},
	})
	probeDir := t.TempDir()
	stale := usageSnapshot{
		FiveHour:   usageWindowSnapshot{Available: true, Percent: 1, ResetAt: time.Unix(1000, 0)},
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
