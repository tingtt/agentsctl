package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/tingtt/agentsctl/internal/localstate"
	base "github.com/tingtt/agentsctl/internal/provider"
	"github.com/tingtt/agentsctl/internal/session"
)

// NativeRenamer performs Claude's native, in-place session rename (see
// Provider.Rename). It is an interface -- rather than Provider calling
// straight into rename_transport_unix.go -- so this package stays
// buildable on every platform even though the only real implementation
// (NewNativeRenamer/sendClaudeRename, in rename_unix.go/
// rename_transport_unix.go) is darwin/linux only.
//
// Send is split from cleanup (its own return value) rather than being one
// blocking call, specifically so Provider.Rename can run native-catalog
// confirmation and transient-client cleanup concurrently instead of
// serially: cleanup ending the transient attach client has nothing to do
// with whether the rename itself succeeded (see Provider.Rename), and
// serializing them was the single largest source of avoidable rename
// latency (see sendClaudeRename's doc comment for the real-CLI
// measurements behind this).
type NativeRenamer interface {
	// Send starts a transient attach client and submits the rename
	// command, returning once that submission is durable. cleanup is nil
	// only if err is non-nil.
	Send(ctx context.Context, path, id, name string) (cleanup func(context.Context, time.Duration) error, err error)
}

// renameCleanupTimeout is looser than Open's interactive detach timeout
// (openDetachTimeout, where a slow detach is directly visible to a
// waiting user): rename's cleanup has no one watching it in
// real time, and was observed, under heavy concurrent-claude-process load,
// to occasionally take noticeably longer than 5s for the attach client to
// actually exit after SIGTERM even though it reliably did exit. Unlike the
// old design, this no longer sits on the critical path a caller waits on
// to see the rename succeed -- see Provider.Rename.
const renameCleanupTimeout = 8 * time.Second

// Provider deliberately does not implement sessionctl.UsageSource (#14's
// composer usage line). Investigated and ruled out as not currently
// feasible without either burning quota to inspect API response headers or
// hijacking a real interactive terminal:
//   - `claude` has no usage/limits/quota subcommand or flag (checked
//     `claude --help`, every listed subcommand's own `--help`, and
//     `claude auth status --json`, which reports login/plan identity only).
//   - No local cache or state file under `~/.claude` carries a rate-limit
//     snapshot (checked settings/session/cache files for
//     utilization/resetsAt-shaped keys).
//   - Claude Code's `statusLine` hook JSON payload does carry exactly this
//     shape (`rate_limits.five_hour`/`seven_day`, each with
//     `used_percentage`/`resets_at` -- see
//     https://code.claude.com/docs/en/statusline), but it is only invoked
//     from a live, actively-rendering interactive TUI loop after that
//     session's first API response; verified empirically that `claude -p`
//     (print/headless mode, the same mode `--bg` sessions run under) never
//     invokes it at all. There is no way to request one JSON payload
//     on demand without attaching a real terminal to a session, which
//     Usage has no business doing just to read a percentage.
//
// If a stable, on-demand, headless interface appears in a future Claude
// Code release, add a Usage method here the same way codex.Provider's
// wraps account/rateLimits/read -- Controller.Usage already treats
// UsageSource as fully optional per provider, so nothing else needs to
// change.
type Provider struct {
	Path   string
	Runner base.Runner
	Store  *localstate.Store
	// Renamer is the native rename transport Rename delegates to. Required
	// for Rename to work; production wires it from NewNativeRenamer(). A nil
	// Renamer makes Rename fail closed rather than silently falling back to
	// a local-only rename.
	Renamer NativeRenamer
	// ConfirmPollInterval and ConfirmMaxWait override confirmRenamed's
	// native-catalog poll cadence and ceiling; zero uses the documented
	// defaults (confirmPollInterval/confirmMaxWait). Exposed so a test that
	// deliberately never confirms doesn't have to wait out the full
	// production ceiling to stay deterministic.
	ConfirmPollInterval time.Duration
	ConfirmMaxWait      time.Duration
}

func (p *Provider) confirmPollInterval() time.Duration {
	if p.ConfirmPollInterval > 0 {
		return p.ConfirmPollInterval
	}
	return confirmPollInterval
}
func (p *Provider) confirmMaxWait() time.Duration {
	if p.ConfirmMaxWait > 0 {
		return p.ConfirmMaxWait
	}
	return confirmMaxWait
}

func (p *Provider) ID() session.ProviderID { return session.ProviderClaude }
func (p *Provider) path() string {
	if p.Path != "" {
		return p.Path
	}
	return "claude"
}
func (p *Provider) Available() error { _, err := exec.LookPath(p.path()); return err }

func (p *Provider) List(ctx context.Context, archived bool) ([]session.Session, error) {
	res, err := p.Runner.Run(ctx, p.path(), []string{"agents", "--json", "--all"}, "")
	if err != nil {
		return nil, fmt.Errorf("claude agents: %w: %s", err, strings.TrimSpace(string(res.Stderr)))
	}
	var raw []map[string]any
	if err := json.Unmarshal(res.Stdout, &raw); err != nil {
		return nil, fmt.Errorf("decode claude agents JSON: %w", err)
	}
	claudeArchived, legacyNames, _ := p.Store.ClaudeState()
	rows := make([]session.Session, 0, len(raw))
	for _, v := range raw {
		id := text(v, "id", "sessionId")
		if id == "" {
			continue
		}
		isArchived := claudeArchived[id]
		if isArchived != archived {
			continue
		}
		activity := claudeActivity(text(v, "status"), text(v, "state"))
		runtime := session.RuntimeDetached
		if activity == session.ActivityCompleted || activity == session.ActivityFailed {
			runtime = session.RuntimeStopped
		}
		created := timestamp(v["startedAt"])
		updated := timestamp(v["updatedAt"])
		if created.IsZero() {
			created = updated
		}
		if updated.IsZero() {
			updated = created
		}
		attachable := runtime == session.RuntimeDetached || activity == session.ActivityCompleted
		// Claude's own native `name` is canonical (see Provider.Rename):
		// the legacy overlay is only consulted when the native catalog has
		// no name at all, which is migration compatibility for overrides
		// written before native rename existed (see
		// localstate.Store.ClaudeState's doc comment) -- it never hides a
		// name Claude itself now reports.
		name := text(v, "name", "displayName")
		if name == "" {
			name = legacyNames[id]
		}
		actions := session.Actions{}
		if attachable {
			actions[session.ActionOpen] = session.Availability{Available: true}
		} else {
			actions[session.ActionOpen] = session.Availability{Reason: "session is not attachable in its current state"}
		}
		if runtime == session.RuntimeDetached {
			actions[session.ActionStop] = session.Availability{Available: true}
		} else {
			actions[session.ActionStop] = session.Availability{Reason: "session is not running"}
		}
		// Rename never stops or otherwise touches the session (it is a
		// rename-only session action -- see Provider.Rename), so — unlike
		// Stop/Archive — it is available for any non-archived row
		// regardless of Activity/Runtime, active sessions included.
		actions[session.ActionRename] = session.Availability{Available: true}
		if runtime == session.RuntimeStopped {
			actions[session.ActionArchive] = session.Availability{Available: true}
		} else {
			actions[session.ActionArchive] = session.Availability{Reason: "stop the session before archiving"}
		}
		rows = append(rows, session.Session{Key: session.Key{Provider: session.ProviderClaude, ID: id}, Name: name, Summary: text(v, "summary", "description", "lastMessage"), CWD: text(v, "cwd", "workingDirectory"), CreatedAt: created, UpdatedAt: updated, Activity: activity, Runtime: runtime, Archived: isArchived, Actions: actions})
	}
	return rows, nil
}
func (p *Provider) Dispatch(ctx context.Context, prompt, cwd string) (session.Session, error) {
	res, err := p.Runner.Run(ctx, p.path(), []string{"--bg", prompt}, cwd)
	if err != nil {
		return session.Session{}, fmt.Errorf("claude --bg: %w: %s", err, strings.TrimSpace(string(res.Stderr)))
	}
	id := strings.TrimSpace(string(res.Stdout))
	if strings.HasPrefix(id, "{") {
		var v map[string]any
		if json.Unmarshal(res.Stdout, &v) == nil {
			id = text(v, "id", "sessionId")
		}
	} else {
		id = backgroundID(id)
	}
	fields := strings.Fields(id)
	if len(fields) == 0 {
		return session.Session{}, errors.New("claude --bg returned no session id")
	}
	id = fields[0]
	createdAt := time.Now()
	actions := session.Actions{session.ActionOpen: {Available: true}, session.ActionStop: {Available: true}}
	return session.Session{Key: session.Key{Provider: session.ProviderClaude, ID: id}, Summary: prompt, CWD: cwd, CreatedAt: createdAt, UpdatedAt: createdAt, Activity: session.ActivityStarting, Runtime: session.RuntimeDetached, Actions: actions}, nil
}
func (p *Provider) Stop(ctx context.Context, k session.Key) error {
	res, err := p.Runner.Run(ctx, p.path(), []string{"stop", k.ID}, "")
	if err != nil {
		return fmt.Errorf("claude stop: %w: %s", err, strings.TrimSpace(string(res.Stderr)))
	}
	return nil
}
func (p *Provider) Archive(_ context.Context, k session.Key) error {
	return p.Store.SetClaudeArchived(k.ID)
}
func (p *Provider) Unarchive(_ context.Context, k session.Key) error {
	return p.Store.ClearClaudeArchived(k.ID)
}

// Rename performs Claude's own native, in-place session rename via a
// Claude-specific transport (NativeRenamer/sendClaudeRename on
// darwin/linux): it drives a transient, headless `claude attach <id>`
// client that sends the CLI's own `/rename <name>` slash command, then
// detaches only that client — the native session, its lifetime, and its
// identity (id/sessionId/pid) are all left untouched. See the doc comment
// on sendClaudeRename for what was verified against the installed CLI, and
// NativeRenamer's for why this is an interface rather than a direct call
// into rename_transport_unix.go.
//
// `claude --bg --resume <id> --name <name>` is deliberately not used here:
// verified (against `claude` 2.1.260/2.1.263) to always fork a new session
// rather than mutate the original's saved options, for any session state
// (active or stopped) and even given the full (not short) session ID.
//
// Rename is rename-only: on failure, no state changes at all — in
// particular, it never falls back to writing a local-only override, and it
// never touches the Claude archive overlay or any other session-lifecycle
// field. On confirmed success it deletes any stale legacy name for k.ID
// left over from before native rename existed (see
// localstate.Store.ClaudeState's doc comment), since List() now treats it
// purely as a legacy fallback and a stale entry must not go on hiding the
// new native name.
//
// The native catalog check (confirmRenamed) always runs and is what
// actually decides success/failure here, even when Send itself returned an
// error: verified against the installed CLI in a resource-constrained
// sandbox, the transient client's own cleanup can time out well after
// `/rename` has already been durably applied, which would otherwise report
// a rename that plainly succeeded (name changed, same session, confirmed
// via the catalog) as a failure to the caller. A Send error is only
// surfaced if the catalog also fails to confirm the name — i.e. the rename
// genuinely didn't happen.
//
// confirmRenamed and the transient client's cleanup run concurrently, not
// sequentially: cleanup is unrelated to whether the rename succeeded (see
// NativeRenamer), and serializing "wait for the client to exit" before
// "check whether the rename landed" was the single largest source of
// avoidable rename latency (see sendClaudeRename's doc comment). Rename
// still waits for cleanup to finish (bounded by
// renameCleanupTimeout) before returning, the same as before -- only the
// order changed from serial to parallel -- so a transient attach client is
// never left to outlive this call.
func (p *Provider) Rename(ctx context.Context, k session.Key, name string) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("name is required")
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return errors.New("name must not contain control characters")
		}
	}
	if p.Renamer == nil {
		return errors.New("claude native rename is not supported on this platform")
	}
	cleanup, sendErr := p.Renamer.Send(ctx, p.path(), k.ID, name)

	var cleanupWG sync.WaitGroup
	if cleanup != nil {
		cleanupWG.Add(1)
		go func() {
			defer cleanupWG.Done()
			// The cleanup error is deliberately discarded: ending the
			// transient attach client has no bearing on whether the
			// rename itself succeeded, which confirmRenamed below decides
			// independently against the native catalog.
			_ = cleanup(ctx, renameCleanupTimeout)
		}()
	}

	confirmErr := p.confirmRenamed(ctx, k.ID, name)
	cleanupWG.Wait()

	if confirmErr != nil {
		if sendErr != nil {
			return fmt.Errorf("claude rename: %w (native catalog also did not confirm the rename: %v)", sendErr, confirmErr)
		}
		return confirmErr
	}
	return p.Store.ClearLegacyClaudeName(k.ID)
}

// confirmPollInterval and confirmMaxWait bound confirmRenamed's polling of
// `claude agents --json --all`. Real-CLI measurement (6 runs, both a
// working and a completed disposable session) showed the catalog reflects
// a rename within 300-350ms of the command being sent, so confirmMaxWait
// is a generous safety ceiling, not an expected wait.
const (
	confirmPollInterval = 100 * time.Millisecond
	confirmMaxWait      = 5 * time.Second
)

// confirmRenamed polls `claude agents --json --all` -- the same native,
// machine-readable source List() uses, never PTY output -- until it
// reflects the new name, to confirm the rename actually landed in Claude's
// own catalog before Rename reports success or touches local state. It is
// time-bounded (confirmMaxWait) rather than a fixed attempt count, so a
// slow individual `claude agents` call cannot silently balloon the total
// wait past what confirmMaxWait actually allows.
func (p *Provider) confirmRenamed(ctx context.Context, id, name string) error {
	maxWait := p.confirmMaxWait()
	pollInterval := p.confirmPollInterval()
	deadline := time.Now().Add(maxWait)
	var lastErr error
	for {
		res, err := p.Runner.Run(ctx, p.path(), []string{"agents", "--json", "--all"}, "")
		if err != nil {
			lastErr = fmt.Errorf("claude agents: %w: %s", err, strings.TrimSpace(string(res.Stderr)))
		} else {
			var raw []map[string]any
			if err := json.Unmarshal(res.Stdout, &raw); err != nil {
				lastErr = fmt.Errorf("decode claude agents JSON: %w", err)
			} else if got, found := nativeNameByID(raw, id); !found {
				lastErr = fmt.Errorf("session %s not found in claude agents catalog after rename", id)
			} else if got != name {
				lastErr = fmt.Errorf("native session name is %q, want %q", got, name)
			} else {
				return nil
			}
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("could not confirm native rename within %s: %w", maxWait, lastErr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

func nativeNameByID(raw []map[string]any, id string) (name string, found bool) {
	for _, v := range raw {
		if text(v, "id", "sessionId") == id {
			return text(v, "name", "displayName"), true
		}
	}
	return "", false
}

func text(v map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := v[k].(string); ok {
			return s
		}
	}
	return ""
}
func timestamp(v any) time.Time {
	switch x := v.(type) {
	case float64:
		return unixTimestamp(int64(x))
	case string:
		if t, e := time.Parse(time.RFC3339, x); e == nil {
			return t
		}
		if n, e := strconv.ParseInt(x, 10, 64); e == nil {
			return unixTimestamp(n)
		}
	}
	return time.Time{}
}

func unixTimestamp(value int64) time.Time {
	if value >= 100_000_000_000 {
		return time.UnixMilli(value)
	}
	return time.Unix(value, 0)
}

func backgroundID(output string) string {
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		for i := range fields {
			if fields[i] == "attach" && i+1 < len(fields) {
				return fields[i+1]
			}
		}
	}
	fields := strings.Fields(output)
	if len(fields) > 0 {
		return fields[len(fields)-1]
	}
	return ""
}

// claudeActivity maps the installed Claude CLI's native `state` (working,
// blocked, done, stopped — the lifecycle signal, verified against
// `claude agents --json --all` output from dispatch through completion) and
// `status` (busy/idle — a coarser, secondary overlay used only when `state`
// is absent, as on pre-daemon-tracking rows) fields to the common Activity
// model. There is no observed native "starting" value: a freshly dispatched
// session is already reported as state "working" by the time it is first
// observable, so ActivityStarting is produced only by agentsctl's own
// Dispatch return value, never derived from List.
func claudeActivity(status, state string) session.Activity {
	switch strings.ToLower(state) {
	case "working", "running", "active":
		return session.ActivityWorking
	case "blocked", "needsinput", "waiting", "waiting_for_input":
		return session.ActivityNeedsInput
	case "waitingforquota", "waiting_for_quota":
		return session.ActivityWaitingQuota
	case "done", "completed", "stopped":
		return session.ActivityCompleted
	case "failed", "error":
		return session.ActivityFailed
	case "starting", "opening":
		return session.ActivityStarting
	case "idle", "ready":
		return session.ActivityIdle
	}
	switch strings.ToLower(status) {
	case "busy":
		return session.ActivityWorking
	case "idle":
		return session.ActivityIdle
	}
	return session.ActivityUnknown
}
