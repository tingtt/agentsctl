package claude

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/tingtt/agentsctl/internal/provider/claude/probestate"
	"github.com/tingtt/agentsctl/internal/session"
)

// TestRunUsageCollectorParsesStdinAndWritesSnapshot fixes the collector's
// end-to-end contract as Claude Code itself would invoke it: a JSON
// payload on stdin, --out naming where the derived snapshot lands.
func TestRunUsageCollectorParsesStdinAndWritesSnapshot(t *testing.T) {
	out := filepath.Join(t.TempDir(), "usage.json")
	stdin := strings.NewReader(`{"cost":{"total_api_duration_ms":1000},"rate_limits":{"five_hour":{"used_percentage":70,"resets_at":1000},"seven_day":{"used_percentage":20,"resets_at":2000}}}`)
	if err := RunUsageCollector([]string{"--out", out}, stdin); err != nil {
		t.Fatal(err)
	}
	snap, ok, err := probestate.NewSnapshotStore(out).Load()
	if err != nil || !ok {
		t.Fatalf("readUsageSnapshot ok=%v err=%v", ok, err)
	}
	if snap.FiveHour.State != session.UsageAvailable || snap.FiveHour.Percent != 70 {
		t.Fatalf("FiveHour=%+v, want Available/70%%", snap.FiveHour)
	}
	if snap.Weekly.State != session.UsageAvailable || snap.Weekly.Percent != 20 {
		t.Fatalf("Weekly=%+v, want Available/20%%", snap.Weekly)
	}
}

// TestRunUsageCollectorRequiresOut fixes that a missing --out fails
// loudly instead of silently discarding the payload.
func TestRunUsageCollectorRequiresOut(t *testing.T) {
	if err := RunUsageCollector(nil, strings.NewReader(`{}`)); err == nil {
		t.Fatal("missing --out was accepted")
	}
}

// TestRunUsageCollectorRejectsMalformedStdin fixes that a malformed
// payload surfaces as an error rather than silently writing an empty/wrong
// snapshot.
func TestRunUsageCollectorRejectsMalformedStdin(t *testing.T) {
	out := filepath.Join(t.TempDir(), "usage.json")
	if err := RunUsageCollector([]string{"--out", out}, strings.NewReader("not json")); err == nil {
		t.Fatal("malformed stdin was accepted")
	}
}
