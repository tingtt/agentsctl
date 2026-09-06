package claude

import (
	"context"
	"errors"
	"fmt"
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

// TestListExposesRenameForWorkingAndStoppedSessions fixes the capability
// bug this fixes: Rename must be available for any non-archived Claude
// session regardless of Activity/Runtime — active sessions included, since
// the local display-name overlay (see Rename) never stops or otherwise
// touches the session, unlike Stop/Archive.
func TestListExposesRenameForWorkingAndStoppedSessions(t *testing.T) {
	r := &fakeRunner{result: base.Result{Stdout: []byte(`[
		{"id":"working","status":"busy","state":"working"},
		{"id":"done","status":"idle","state":"done"}
	]`)}}
	p := Provider{Path: "ignored", Runner: r, Store: state.New(filepath.Join(t.TempDir(), "state.json"))}
	rows, err := p.List(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if !row.Capabilities.Rename {
			t.Fatalf("row %+v has no Rename capability", row)
		}
	}
}

// fakeRenamer is a test NativeRenamer: it records the arguments it was
// called with and, on success, updates fakeRunner's canned `claude agents`
// response so Provider.confirmRenamed's follow-up native-catalog check
// observes the new name — mirroring what the real transport (a successful
// `/rename` against the live daemon) actually causes.
type fakeRenamer struct {
	err                  error
	calls                []renameCall
	runner               *fakeRunner
	confirmWithNativeID  string // id key to update in runner's canned JSON on success
	confirmWithFieldName string // "name" or "displayName" — which native field carries it
}
type renameCall struct {
	path, id, name string
	timeout        time.Duration
}

func (f *fakeRenamer) Rename(_ context.Context, path, id, name string, timeout time.Duration) error {
	f.calls = append(f.calls, renameCall{path, id, name, timeout})
	// Mirrors the real transport: the native mutation (and so the catalog
	// update) happens before the transport's own detach/cleanup step, so a
	// transport error (e.g. a slow-to-exit attach client) does not mean the
	// rename itself never reached the catalog -- see
	// TestRenameSucceedsWhenCatalogConfirmsDespiteTransportError.
	if f.runner != nil {
		field := f.confirmWithFieldName
		if field == "" {
			field = "name"
		}
		targetID := f.confirmWithNativeID
		if targetID == "" {
			targetID = id
		}
		f.runner.result.Stdout = []byte(fmt.Sprintf(`[{"id":%q,%q:%q,"status":"idle","state":"done"}]`, targetID, field, name))
	}
	return f.err
}

// TestRenameInvokesNativeTransportForTheGivenSessionAndConfirmsViaCatalog
// fixes the new native-rename contract end to end: Provider.Rename must
// call the NativeRenamer with exactly the target session's ID (never a
// different/derived one), and — since the transport's own success says
// nothing about whether Claude's catalog actually reflects it — Rename
// must not report success, or write anything to local state, until a
// follow-up `claude agents --json --all` (via the same Runner used
// elsewhere) confirms the native name changed.
func TestRenameInvokesNativeTransportForTheGivenSessionAndConfirmsViaCatalog(t *testing.T) {
	r := &fakeRunner{result: base.Result{Stdout: []byte(`[{"id":"c1","name":"native-name","status":"busy","state":"working"}]`)}}
	renamer := &fakeRenamer{runner: r}
	store := state.New(filepath.Join(t.TempDir(), "state.json"))
	p := Provider{Path: "ignored", Runner: r, Store: store, Renamer: renamer}
	key := session.Key{Provider: session.ProviderClaude, ID: "c1"}
	if err := p.Rename(context.Background(), key, "My New Name"); err != nil {
		t.Fatal(err)
	}
	if len(renamer.calls) != 1 || renamer.calls[0].id != "c1" || renamer.calls[0].name != "My New Name" {
		t.Fatalf("native transport calls=%+v", renamer.calls)
	}
	rows, err := p.List(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Name != "My New Name" {
		t.Fatalf("rows=%+v, want native name from the (now-updated) catalog", rows)
	}
}

// TestRenameReturnsNativeTransportFailureWithoutTouchingLocalState fixes
// that a failing native rename (e.g. the transport couldn't attach, or the
// attach client died) must be reported to the caller as an error — and
// must never be silently treated as a successful local-only rename.
func TestRenameReturnsNativeTransportFailureWithoutTouchingLocalState(t *testing.T) {
	r := &fakeRunner{result: base.Result{Stdout: []byte(`[{"id":"c1","name":"native-name","status":"busy","state":"working"}]`)}}
	renamer := &fakeRenamer{err: errors.New("claude attach did not stay up long enough to send rename")}
	store := state.New(filepath.Join(t.TempDir(), "state.json"))
	p := Provider{Path: "ignored", Runner: r, Store: store, Renamer: renamer}
	key := session.Key{Provider: session.ProviderClaude, ID: "c1"}
	if err := p.Rename(context.Background(), key, "My New Name"); err == nil {
		t.Fatal("native transport failure was swallowed")
	}
	rows, err := p.List(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].Name != "native-name" {
		t.Fatalf("rows=%+v, a failed rename must not change the displayed name", rows)
	}
	d, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(d.ClaudeNames) != 0 {
		t.Fatalf("failed rename must not fall back to a local overlay: %+v", d.ClaudeNames)
	}
}

// TestRenameSucceedsWhenCatalogConfirmsDespiteTransportError fixes a real
// observation against the installed CLI in a resource-constrained sandbox:
// the native `/rename` mutation was durably applied and immediately visible
// in `claude agents --json --all` (same id), yet the transient attach
// client's own detach step (unrelated to whether the rename happened)
// separately timed out and returned an error. Provider.Rename must report
// this as success — the catalog, not the transport's own return value, is
// authoritative for whether the rename happened — never leave a confirmed
// rename looking like a failure to the caller.
func TestRenameSucceedsWhenCatalogConfirmsDespiteTransportError(t *testing.T) {
	r := &fakeRunner{result: base.Result{Stdout: []byte(`[{"id":"c1","name":"native-name","status":"busy","state":"working"}]`)}}
	renamer := &fakeRenamer{runner: r, err: errors.New("claude attach client did not exit after detach")}
	store := state.New(filepath.Join(t.TempDir(), "state.json"))
	p := Provider{Path: "ignored", Runner: r, Store: store, Renamer: renamer}
	key := session.Key{Provider: session.ProviderClaude, ID: "c1"}
	if err := p.Rename(context.Background(), key, "My New Name"); err != nil {
		t.Fatalf("Rename reported failure despite the native catalog confirming the rename: %v", err)
	}
	rows, err := p.List(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].Name != "My New Name" {
		t.Fatalf("rows=%+v", rows)
	}
}

// TestRenameFailsWhenNativeCatalogNeverConfirms covers the transport
// reporting success while the native catalog disagrees (e.g. the attach
// client exited before the daemon durably applied /rename): Rename must
// surface this as an error — the UI must never look like it succeeded when
// the catalog can't back that up — rather than declaring victory on the
// transport call alone.
func TestRenameFailsWhenNativeCatalogNeverConfirms(t *testing.T) {
	r := &fakeRunner{result: base.Result{Stdout: []byte(`[{"id":"c1","name":"still-old-name","status":"busy","state":"working"}]`)}}
	renamer := &fakeRenamer{} // succeeds, but deliberately does not update r's canned response
	store := state.New(filepath.Join(t.TempDir(), "state.json"))
	p := Provider{Path: "ignored", Runner: r, Store: store, Renamer: renamer}
	key := session.Key{Provider: session.ProviderClaude, ID: "c1"}
	if err := p.Rename(context.Background(), key, "My New Name"); err == nil {
		t.Fatal("rename reported success despite the native catalog never reflecting the new name")
	}
	d, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(d.ClaudeNames) != 0 {
		t.Fatalf("unconfirmed rename must not fall back to a local overlay: %+v", d.ClaudeNames)
	}
}

// TestRenameDeletesStaleLocalOverrideOnConfirmedSuccess fixes the
// migration-cleanup half of the new contract: a pre-existing
// state.Data.ClaudeNames entry from before native rename existed must not
// go on shadowing the session once a native rename for it is confirmed.
func TestRenameDeletesStaleLocalOverrideOnConfirmedSuccess(t *testing.T) {
	r := &fakeRunner{result: base.Result{Stdout: []byte(`[{"id":"c1","name":"native-name","status":"busy","state":"working"}]`)}}
	store := state.New(filepath.Join(t.TempDir(), "state.json"))
	if err := store.Update(func(d *state.Data) error { d.ClaudeNames["c1"] = "stale pre-native override"; return nil }); err != nil {
		t.Fatal(err)
	}
	renamer := &fakeRenamer{runner: r}
	p := Provider{Path: "ignored", Runner: r, Store: store, Renamer: renamer}
	key := session.Key{Provider: session.ProviderClaude, ID: "c1"}
	if err := p.Rename(context.Background(), key, "New Native Name"); err != nil {
		t.Fatal(err)
	}
	d, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := d.ClaudeNames["c1"]; ok {
		t.Fatalf("stale override survived a confirmed native rename: %+v", d.ClaudeNames)
	}
}

// TestListPrefersNativeNameOverStaleLocalOverride fixes List's half of the
// same contract independent of Rename: even a session this build never
// renamed itself must show Claude's own native name over any leftover
// local override for that ID.
func TestListPrefersNativeNameOverStaleLocalOverride(t *testing.T) {
	r := &fakeRunner{result: base.Result{Stdout: []byte(`[{"id":"c1","name":"native-name","status":"busy","state":"working"}]`)}}
	store := state.New(filepath.Join(t.TempDir(), "state.json"))
	if err := store.Update(func(d *state.Data) error { d.ClaudeNames["c1"] = "stale override"; return nil }); err != nil {
		t.Fatal(err)
	}
	p := Provider{Path: "ignored", Runner: r, Store: store}
	rows, err := p.List(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Name != "native-name" {
		t.Fatalf("rows=%+v, want native name (not the stale override)", rows)
	}
}

// TestListFallsBackToLocalOverrideWhenNativeCatalogHasNoName covers the
// only case state.Data.ClaudeNames may still be consulted: a native
// catalog row that carries no name field at all (observed on
// pre-daemon-tracking rows from the installed CLI).
func TestListFallsBackToLocalOverrideWhenNativeCatalogHasNoName(t *testing.T) {
	r := &fakeRunner{result: base.Result{Stdout: []byte(`[{"id":"c1","state":"done"}]`)}}
	store := state.New(filepath.Join(t.TempDir(), "state.json"))
	if err := store.Update(func(d *state.Data) error { d.ClaudeNames["c1"] = "legacy override"; return nil }); err != nil {
		t.Fatal(err)
	}
	p := Provider{Path: "ignored", Runner: r, Store: store}
	rows, err := p.List(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Name != "legacy override" {
		t.Fatalf("rows=%+v, want the legacy override since the native catalog has no name", rows)
	}
}

func TestRenameRejectsBlankName(t *testing.T) {
	p := Provider{Path: "ignored", Runner: &fakeRunner{}, Store: state.New(filepath.Join(t.TempDir(), "state.json")), Renamer: &fakeRenamer{}}
	if err := p.Rename(context.Background(), session.Key{Provider: session.ProviderClaude, ID: "c1"}, "   "); err == nil {
		t.Fatal("blank rename was accepted")
	}
}

// TestRenameRejectsControlCharacters fixes the PTY-command-injection
// defense: reproduced against the installed CLI, a raw '\r' inside a name
// submits `/rename` early and turns the remainder into a brand-new prompt
// the live agent actually executes (a name of "evil\rhi there" renamed the
// session to "evil" and then had the agent answer "hi there" as a real
// chat turn) — so Provider.Rename must reject every case here before ever
// reaching the transport, not just '\r'/'\n'.
func TestRenameRejectsControlCharacters(t *testing.T) {
	renamer := &fakeRenamer{}
	p := Provider{Path: "ignored", Runner: &fakeRunner{}, Store: state.New(filepath.Join(t.TempDir(), "state.json")), Renamer: renamer}
	key := session.Key{Provider: session.ProviderClaude, ID: "c1"}
	cases := []string{
		"evil\rhi there", // CR: submits /rename early, remainder becomes a new prompt
		"evil\nhi there", // LF: same risk via the other line terminator
		"evil\x1b[2J",    // ESC: could start an escape/CSI sequence a client interprets as input
		"evil\x03",       // C0 control (Ctrl+C)
		"evil\x7f",       // DEL
		"evil\x1a",       // literal Ctrl+Z — agentsctl's own attach-detach byte
		"evil\x1d",       // literal Ctrl+] — agentsctl's own outer-terminal detach key
	}
	for _, name := range cases {
		if err := p.Rename(context.Background(), key, name); err == nil {
			t.Fatalf("control character in %q was accepted", name)
		}
	}
	if len(renamer.calls) != 0 {
		t.Fatalf("a rejected name must never reach the native transport: calls=%+v", renamer.calls)
	}
}

// TestRenameAllowsUnicodeNames fixes that ordinary Unicode — Japanese text
// and plain spaces, both verified against the installed CLI as valid
// `/rename` arguments needing no quoting — is not rejected by the same
// validation that blocks control characters.
func TestRenameAllowsUnicodeNames(t *testing.T) {
	for _, name := range []string{"simple-name", "My Session Name", "日本語 セッション"} {
		r := &fakeRunner{result: base.Result{Stdout: []byte(`[{"id":"c1","name":"native-name","status":"busy","state":"working"}]`)}}
		renamer := &fakeRenamer{runner: r}
		p := Provider{Path: "ignored", Runner: r, Store: state.New(filepath.Join(t.TempDir(), "state.json")), Renamer: renamer}
		key := session.Key{Provider: session.ProviderClaude, ID: "c1"}
		if err := p.Rename(context.Background(), key, name); err != nil {
			t.Fatalf("name %q was rejected: %v", name, err)
		}
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
