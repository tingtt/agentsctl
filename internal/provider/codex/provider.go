package codex

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	processinfo "github.com/tingtt/agentsctl/internal/process"
	base "github.com/tingtt/agentsctl/internal/provider"
	"github.com/tingtt/agentsctl/internal/session"
	"github.com/tingtt/agentsctl/internal/state"
	"golang.org/x/sys/unix"
)

type Dispatcher interface {
	Dispatch(context.Context, string, string, []string) (state.Run, error)
	Stop(context.Context, string) error
	ResumeExisting(context.Context, string, string) (state.Run, error)
}

type Provider struct {
	Path        string
	API         AppServer
	Runner      base.Runner
	Store       *state.Store
	Runtime     Dispatcher
	WriterOwner func(string, processinfo.Identity) (bool, error)
}

func (p *Provider) ID() session.ProviderID { return session.ProviderCodex }
func (p *Provider) Available() error {
	path := p.Path
	if path == "" {
		path = "codex"
	}
	_, err := exec.LookPath(path)
	return err
}

func (p *Provider) List(ctx context.Context, archived bool) ([]session.Session, error) {
	threads, err := p.API.List(ctx, archived)
	if err != nil {
		return nil, err
	}
	if !archived {
		_ = p.reconcile(threads)
	}
	d, _ := p.Store.Load()
	managed := map[string]state.Run{}
	for _, r := range d.Runs {
		if r.Provider == "codex" && r.SessionID != "" && (r.State == "running" || r.State == "starting") {
			managed[r.SessionID] = r
		}
	}
	rows := make([]session.Session, 0, len(threads)+len(d.Runs))
	for _, t := range threads {
		run, ok := managed[t.ID]
		runtime := session.RuntimeNone
		caps := session.Capabilities{Rename: true, Archive: true}
		reason := ""
		if ok {
			runtime = session.RuntimeDetached
			caps.Attach = true
			caps.Stop = run.State == "running" || run.State == "starting"
		} else if p.writerAbsent(t.ID) {
			caps.Attach = true
		} else {
			runtime = session.RuntimeExternal
			reason = "external or unknown Codex writer cannot be attached or stopped safely"
		}
		caps.Reason = reason
		rows = append(rows, session.Session{Key: session.Key{Provider: session.ProviderCodex, ID: t.ID}, Name: value(t.Name), Summary: value(t.Preview), CWD: t.CWD, UpdatedAt: time.Unix(t.UpdatedAt, 0), Activity: codexActivity(t), Runtime: runtime, Archived: archived, RunID: run.ID, Capabilities: caps})
	}
	if !archived {
		for _, r := range d.Runs {
			if r.Provider != "codex" || r.SessionID != "" {
				continue
			}
			activity, runtime, name := session.ActivityStarting, session.RuntimeDetached, "Starting"
			caps := session.Capabilities{Attach: true, Stop: true}
			if r.State == "failed" || r.State == "stale" || r.State == "stopped" {
				activity, runtime, name = session.ActivityFailed, session.RuntimeStopped, "Unbound run"
				caps = session.Capabilities{Reason: r.Error}
			}
			rows = append(rows, session.Session{Key: session.Key{Provider: session.ProviderCodex, ID: r.ID}, Name: name, Summary: r.Error, CWD: r.CWD, UpdatedAt: r.StartedAt, Activity: activity, Runtime: runtime, RunID: r.ID, Capabilities: caps})
		}
	}
	return rows, nil
}
func (p *Provider) Dispatch(ctx context.Context, prompt, cwd string) (session.Session, error) {
	before, err := p.API.List(ctx, false)
	if err != nil {
		return session.Session{}, err
	}
	baseline := make([]string, 0, len(before))
	for _, t := range before {
		baseline = append(baseline, t.ID)
	}
	r, err := p.Runtime.Dispatch(ctx, prompt, cwd, baseline)
	if err != nil {
		return session.Session{}, err
	}
	return session.Session{Key: session.Key{Provider: session.ProviderCodex, ID: r.ID}, Name: "Starting", CWD: cwd, UpdatedAt: time.Now(), Activity: session.ActivityStarting, Runtime: session.RuntimeDetached, RunID: r.ID, Capabilities: session.Capabilities{Attach: true, Stop: true}}, nil
}
func (p *Provider) Stop(ctx context.Context, k session.Key) error {
	d, err := p.Store.Load()
	if err != nil {
		return err
	}
	for _, r := range d.Runs {
		if r.ID == k.ID || r.SessionID == k.ID {
			return p.Runtime.Stop(ctx, r.ID)
		}
	}
	return errors.New("refusing to stop a Codex writer not owned by agentsctl")
}
func (p *Provider) Archive(ctx context.Context, k session.Key) error { return p.API.Archive(ctx, k.ID) }
func (p *Provider) Unarchive(ctx context.Context, k session.Key) error {
	return p.API.Unarchive(ctx, k.ID)
}
func (p *Provider) Rename(ctx context.Context, k session.Key, name string) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("name is required")
	}
	return p.API.Rename(ctx, k.ID, name)
}

func (p *Provider) PrepareAttach(ctx context.Context, s session.Session) (string, error) {
	if s.RunID != "" {
		return s.RunID, nil
	}
	if !p.writerAbsent(s.Key.ID) {
		return "", errors.New("external or unknown writer cannot be attached safely")
	}
	r, err := p.Runtime.ResumeExisting(ctx, s.Key.ID, s.CWD)
	if err != nil {
		return "", err
	}
	return r.ID, nil
}
func (p *Provider) reconcile(threads []Thread) error {
	d, err := p.Store.Load()
	if err != nil {
		return err
	}
	return p.Store.Update(func(next *state.Data) error {
		for id, r := range d.Runs {
			if r.Provider != "codex" || r.SessionID != "" || r.State == "failed" || r.State == "stale" {
				continue
			}
			base := map[string]bool{}
			for _, x := range r.Baseline {
				base[x] = true
			}
			var candidates []string
			for _, t := range threads {
				owner := p.WriterOwner
				if owner == nil {
					owner = processinfo.OwnsWriterLock
				}
				owned, _ := owner(filepath.Join(p.API.CodexHome(), "thread-writer-locks", t.ID+".lock"), processinfo.Identity{PID: r.PID, StartTime: r.StartTime, UID: r.UID})
				if !base[t.ID] && filepath.Clean(t.CWD) == filepath.Clean(r.CWD) && owned {
					candidates = append(candidates, t.ID)
				}
			}
			switch len(candidates) {
			case 1:
				r.SessionID = candidates[0]
				r.Error = ""
			case 0:
				continue
			default:
				r.Error = "ambiguous Codex thread binding; candidates were not guessed"
			}
			next.Runs[id] = r
		}
		return nil
	})
}

func (p *Provider) writerAbsent(id string) bool {
	home := p.API.CodexHome()
	if home == "" {
		return false
	}
	path := filepath.Join(home, "thread-writer-locks", id+".lock")
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return false
	}
	_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
	return true
}
func value(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}
func codexActivity(t Thread) session.Activity {
	status := strings.ToLower(t.Status.Type)
	for _, f := range t.Status.ActiveFlags {
		if strings.Contains(strings.ToLower(f), "rate") || strings.Contains(strings.ToLower(f), "quota") {
			return session.ActivityWaitingQuota
		}
	}
	switch status {
	case "active", "running", "inprogress":
		return session.ActivityWorking
	case "waitingforinput", "needsinput":
		return session.ActivityNeedsInput
	case "completed":
		return session.ActivityCompleted
	case "failed", "error":
		return session.ActivityFailed
	case "idle", "notloaded":
		return session.ActivityIdle
	default:
		if len(t.Turns) > 0 {
			switch strings.ToLower(t.Turns[len(t.Turns)-1].Status) {
			case "inprogress":
				return session.ActivityWorking
			case "completed":
				return session.ActivityCompleted
			case "failed":
				return session.ActivityFailed
			}
		}
		return session.ActivityUnknown
	}
}

var _ = fmt.Sprintf
