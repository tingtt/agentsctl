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

	base "github.com/tingtt/agentsctl/internal/provider"
	"github.com/tingtt/agentsctl/internal/session"
	"github.com/tingtt/agentsctl/internal/state"
)

type Provider struct {
	Path   string
	Runner base.Runner
	Store  *state.Store
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
		rows = append(rows, session.Session{Key: session.Key{Provider: session.ProviderClaude, ID: id}, Name: text(v, "name", "displayName"), Summary: text(v, "summary", "description", "lastMessage"), CWD: text(v, "cwd", "workingDirectory"), CreatedAt: created, UpdatedAt: updated, Activity: activity, Runtime: runtime, Archived: isArchived, Capabilities: session.Capabilities{Attach: attachable, Stop: runtime == session.RuntimeDetached, Rename: false, Archive: runtime == session.RuntimeStopped, Unarchive: isArchived, Respawn: runtime == session.RuntimeStopped}})
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
func (p *Provider) Rename(context.Context, session.Key, string) error {
	return errors.New("installed Claude CLI has no native rename operation")
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
