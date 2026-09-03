package claude

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	base "github.com/tingtt/agentsctl/internal/provider"
	"github.com/tingtt/agentsctl/internal/session"
	"github.com/tingtt/agentsctl/internal/state"
)

type fakeRunner struct {
	result base.Result
	err    error
	args   []string
}

// TestListUsesNativeStartedAtMillisecondsAndStatus fixes the native
// `status`/`state` combinations observed from the installed Claude CLI
// (`claude agents --json --all`) at each point in a real session's
// lifecycle: freshly dispatched and actively running ({"status":"busy",
// "state":"working"}), finished ({"status":"idle","state":"done"}), and a
// legacy pre-daemon-tracking row that carries only `state` ("stopped", no
// `status` field at all). A native value this build has never seen must
// fall back to ActivityUnknown rather than be guessed at.
func TestListUsesNativeStartedAtMillisecondsAndStatus(t *testing.T) {
	r := &fakeRunner{result: base.Result{Stdout: []byte(`[
		{"id":"working","startedAt":1788438925422,"status":"busy","state":"working"},
		{"id":"done","startedAt":1788438925000,"status":"idle","state":"done"},
		{"id":"legacy-stopped","startedAt":1788438924500,"state":"stopped"},
		{"id":"unexpected","startedAt":1788438924000,"status":"new-native-status","state":"new-native-state"}
	]`)}}
	p := Provider{Path: "ignored", Runner: r, Store: state.New(filepath.Join(t.TempDir(), "state.json"))}
	rows, err := p.List(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	wantCreated := time.UnixMilli(1788438925422)
	if !rows[0].CreatedAt.Equal(wantCreated) || rows[0].Activity != session.ActivityWorking {
		t.Fatalf("working=%+v", rows[0])
	}
	if rows[1].Activity != session.ActivityCompleted || !rows[1].Capabilities.Attach || rows[1].Runtime != session.RuntimeStopped {
		t.Fatalf("done=%+v", rows[1])
	}
	if rows[2].Activity != session.ActivityCompleted || !rows[2].Capabilities.Attach || rows[2].Runtime != session.RuntimeStopped {
		t.Fatalf("legacy-stopped=%+v", rows[2])
	}
	if rows[3].Activity != session.ActivityUnknown {
		t.Fatalf("unexpected=%+v", rows[3])
	}
}

// TestNativeBlockedStateMapsToNeedsInput fixes Claude's own "Needs input"
// bucketing for state:"blocked" (the raw string used by both a stuck
// server-error retry loop and an awaiting-decision session in the installed
// CLI): agentsctl surfaces both the same way, as ActivityNeedsInput.
func TestNativeBlockedStateMapsToNeedsInput(t *testing.T) {
	if got := claudeActivity("idle", "blocked"); got != session.ActivityNeedsInput {
		t.Fatalf("blocked=%v", got)
	}
}

func TestDispatchParsesCurrentClaudeBackgroundOutput(t *testing.T) {
	r := &fakeRunner{result: base.Result{Stdout: []byte("backgrounded · 54a3fdb1\n  claude attach 54a3fdb1    open in this terminal\n")}}
	p := Provider{Path: "ignored", Runner: r, Store: state.New(filepath.Join(t.TempDir(), "state.json"))}
	created, err := p.Dispatch(context.Background(), "prompt", "/work")
	if err != nil || created.Key.ID != "54a3fdb1" || created.Activity != session.ActivityStarting {
		t.Fatalf("created=%+v err=%v", created, err)
	}
}

func (f *fakeRunner) Run(_ context.Context, _ string, args []string, _ string) (base.Result, error) {
	f.args = append([]string(nil), args...)
	return f.result, f.err
}

func TestMalformedJSONDoesNotBecomeCatalog(t *testing.T) {
	p := Provider{Path: "ignored", Runner: &fakeRunner{result: base.Result{Stdout: []byte("not-json")}}, Store: state.New(filepath.Join(t.TempDir(), "state.json"))}
	if _, err := p.List(context.Background(), false); err == nil {
		t.Fatal("malformed JSON accepted")
	}
}
func TestArchiveIsLocalOverlayAndDoesNotInvokeClaudeDelete(t *testing.T) {
	r := &fakeRunner{result: base.Result{Stdout: []byte(`[{"id":"c1","status":"idle","state":"done"}]`)}}
	p := Provider{Path: "ignored", Runner: r, Store: state.New(filepath.Join(t.TempDir(), "state.json"))}
	if err := p.Archive(context.Background(), session.Key{Provider: session.ProviderClaude, ID: "c1"}); err != nil {
		t.Fatal(err)
	}
	active, err := p.List(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	archived, err := p.List(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 0 || len(archived) != 1 {
		t.Fatalf("active=%d archived=%d", len(active), len(archived))
	}
	capabilities := session.CapabilitiesFor(archived[0])
	if capabilities.Attach || !capabilities.Unarchive {
		t.Fatalf("archived capabilities=%+v", capabilities)
	}
	if len(r.args) < 3 || r.args[0] != "agents" {
		t.Fatalf("unexpected command args: %v", r.args)
	}
}
func TestMissingBinaryIsReported(t *testing.T) {
	p := Provider{Path: filepath.Join(t.TempDir(), "missing")}
	if p.Available() == nil {
		t.Fatal("missing binary reported available")
	}
}

func TestFakeClaudeExecutableListDispatchAndStop(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "claude")
	script := `#!/bin/sh
case "$1" in
  agents) printf '[{"id":"c1","name":"fake","status":"busy","state":"working","cwd":"/work","updatedAt":1}]' ;;
  --bg) printf 'c-new\n' ;;
  stop) exit 0 ;;
  *) exit 2 ;;
esac
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	p := Provider{Path: path, Runner: base.ExecRunner{}, Store: state.New(filepath.Join(dir, "state.json"))}
	rows, err := p.List(context.Background(), false)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
	created, err := p.Dispatch(context.Background(), "literal $(never-evaluated)", dir)
	if err != nil || created.Key.ID != "c-new" {
		t.Fatalf("created=%+v err=%v", created, err)
	}
	if err := p.Stop(context.Background(), created.Key); err != nil {
		t.Fatal(err)
	}
}
