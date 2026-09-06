package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
	"unicode"

	base "github.com/tingtt/agentsctl/internal/provider"
	"github.com/tingtt/agentsctl/internal/session"
	"github.com/tingtt/agentsctl/internal/state"
)

// NativeRenamer performs Claude's native, in-place session rename (see
// Provider.Rename). It is an interface -- rather than Provider calling
// straight into internal/pty -- so this package stays buildable on every
// platform even though the only real implementation
// (NewNativeRenamer/internal/pty's transient-attach transport, in
// rename_unix.go) is darwin/linux only.
type NativeRenamer interface {
	Rename(ctx context.Context, path, id, name string, timeout time.Duration) error
}

// renameTimeout is looser than AttachClaude's interactive detach timeout
// (app_unix.go passes 2s there, where a slow detach is directly visible to
// a waiting user): RenameClaude's headless detach has no one watching it in
// real time, and was observed, under heavy concurrent-claude-process load,
// to occasionally take noticeably longer than 5s for the attach client to
// actually exit after SIGTERM even though it reliably did exit.
const renameTimeout = 8 * time.Second

type Provider struct {
	Path   string
	Runner base.Runner
	Store  *state.Store
	// Renamer is the native rename transport Rename delegates to. Required
	// for Rename to work; production wires it from NewNativeRenamer(). A nil
	// Renamer makes Rename fail closed rather than silently falling back to
	// a local-only rename.
	Renamer NativeRenamer
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
	d, _ := p.Store.Load()
	rows := make([]session.Session, 0, len(raw))
	for _, v := range raw {
		id := text(v, "id", "sessionId")
		if id == "" {
			continue
		}
		isArchived := d.ClaudeArchived[id]
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
		// state.Data.ClaudeNames is only consulted when the native catalog
		// has no name at all, which is legacy migration compatibility for
		// overrides written before native rename existed (see
		// state.Data.ClaudeNames' doc comment) -- it never hides a name
		// Claude itself now reports.
		name := text(v, "name", "displayName")
		if name == "" {
			name = d.ClaudeNames[id]
		}
		// Rename never stops or otherwise touches the session (it is a
		// rename-only session action -- see Provider.Rename), so — unlike
		// Stop/Archive — it is available for any non-archived row
		// regardless of Activity/Runtime, active sessions included.
		rows = append(rows, session.Session{Key: session.Key{Provider: session.ProviderClaude, ID: id}, Name: name, Summary: text(v, "summary", "description", "lastMessage"), CWD: text(v, "cwd", "workingDirectory"), CreatedAt: created, UpdatedAt: updated, Activity: activity, Runtime: runtime, Archived: isArchived, Capabilities: session.Capabilities{Attach: attachable, Stop: runtime == session.RuntimeDetached, Rename: !isArchived, Archive: runtime == session.RuntimeStopped, Unarchive: isArchived, Respawn: runtime == session.RuntimeStopped}})
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
	return session.Session{Key: session.Key{Provider: session.ProviderClaude, ID: id}, Summary: prompt, CWD: cwd, CreatedAt: createdAt, UpdatedAt: createdAt, Activity: session.ActivityStarting, Runtime: session.RuntimeDetached, Capabilities: session.Capabilities{Attach: true, Stop: true}}, nil
}
func (p *Provider) Stop(ctx context.Context, k session.Key) error {
	res, err := p.Runner.Run(ctx, p.path(), []string{"stop", k.ID}, "")
	if err != nil {
		return fmt.Errorf("claude stop: %w: %s", err, strings.TrimSpace(string(res.Stderr)))
	}
	return nil
}
func (p *Provider) Archive(_ context.Context, k session.Key) error {
	return p.Store.Update(func(d *state.Data) error { d.ClaudeArchived[k.ID] = true; return nil })
}
func (p *Provider) Unarchive(_ context.Context, k session.Key) error {
	return p.Store.Update(func(d *state.Data) error { delete(d.ClaudeArchived, k.ID); return nil })
}

// Rename performs Claude's own native, in-place session rename via a
// Claude-specific transport (NativeRenamer/internal/pty.RenameClaude on
// darwin/linux): it drives a transient, headless `claude attach <id>`
// client that sends the CLI's own `/rename <name>` slash command, then
// detaches only that client — the native session, its lifetime, and its
// identity (id/sessionId/pid) are all left untouched. See the doc comment
// on internal/pty.RenameClaude for what was verified against the installed
// CLI, and NativeRenamer's for why this is an interface rather than a
// direct call into internal/pty.
//
// `claude --bg --resume <id> --name <name>` is deliberately not used here:
// verified (against `claude` 2.1.260/2.1.263) to always fork a new session
// rather than mutate the original's saved options, for any session state
// (active or stopped) and even given the full (not short) session ID.
//
// Rename is rename-only: on failure, no state changes at all — in
// particular, it never falls back to writing a local-only override, and it
// never touches state.Data.ClaudeArchived or any other session-lifecycle
// field. On confirmed success it deletes any stale
// state.Data.ClaudeNames[k.ID] left over from before native rename existed
// (see that field's doc comment), since List() now treats it purely as a
// legacy fallback and a stale entry must not go on hiding the new native
// name.
//
// The native catalog check (confirmRenamed) always runs and is what
// actually decides success/failure here, even when the transport itself
// returned an error: verified against the installed CLI in a
// resource-constrained sandbox, the transport's own detach step (ending the
// transient attach client — see internal/pty.RenameClaude) can time out
// well after `/rename` has already been durably applied, which would
// otherwise report a rename that plainly succeeded (name changed, same
// session, confirmed via the catalog) as a failure to the caller. A
// transport error is only surfaced if the catalog also fails to confirm the
// name — i.e. the rename genuinely didn't happen.
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
	transportErr := p.Renamer.Rename(ctx, p.path(), k.ID, name, renameTimeout)
	if err := p.confirmRenamed(ctx, k.ID, name); err != nil {
		if transportErr != nil {
			return fmt.Errorf("claude rename: %w (native catalog also did not confirm the rename: %v)", transportErr, err)
		}
		return err
	}
	return p.Store.Update(func(d *state.Data) error { delete(d.ClaudeNames, k.ID); return nil })
}

// confirmRenamed re-queries `claude agents --json --all` -- the same
// native, machine-readable source List() uses, never PTY output -- to
// confirm the rename actually landed in Claude's own catalog before Rename
// reports success or touches local state. A short bounded retry absorbs
// the small propagation delay observed against the installed CLI between
// the attach client detaching and `claude agents` reflecting the new name.
func (p *Provider) confirmRenamed(ctx context.Context, id, name string) error {
	var lastErr error
	for attempt := 0; ; attempt++ {
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
		if attempt >= 4 {
			return fmt.Errorf("could not confirm native rename: %w", lastErr)
		}
		time.Sleep(300 * time.Millisecond)
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
