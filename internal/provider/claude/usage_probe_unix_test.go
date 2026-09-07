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
	pr := NewProbe(fakeClaudePath(t), nil, probeDir)
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
	pr := NewProbe(fakeClaudePath(t), nil, probeDir)
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
	pr := NewProbe(fakeClaudePath(t), nil, probeDir)
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
	pr := NewProbe(filepath.Join(t.TempDir(), "no-such-claude-binary"), nil, probeDir)
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
	pr := NewProbe(fakeClaudePath(t), nil, probeDir)
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
	pr := NewProbe(fakeClaudePath(t), nil, probeDir)
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
