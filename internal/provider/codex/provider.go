package codex

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/tingtt/agentsctl/internal/localstate"
	processinfo "github.com/tingtt/agentsctl/internal/process"
	base "github.com/tingtt/agentsctl/internal/provider"
	"github.com/tingtt/agentsctl/internal/provider/codex/writerlock"
	"github.com/tingtt/agentsctl/internal/session"
	"golang.org/x/sys/unix"
)

type Dispatcher interface {
	Dispatch(context.Context, string, string, []string, map[string]string) (localstate.Run, error)
	Stop(context.Context, string) error
	ResumeExisting(context.Context, string, string, map[string]string) (localstate.Run, error)
	Attach(ctx context.Context, runID string, in *os.File, out io.Writer) error
}

type Provider struct {
	Path        string
	API         AppServer
	Runner      base.Runner
	Store       *localstate.Store
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
	runs, _ := p.Store.Runs()
	managed := map[string]localstate.Run{}
	for _, r := range runs {
		if r.Provider == "codex" && r.SessionID != "" && (r.State == "running" || r.State == "starting") {
			managed[r.SessionID] = r
		}
	}
	rows := make([]session.Session, 0, len(threads)+len(runs))
	for _, t := range threads {
		run, ok := managed[t.ID]
		runtime := session.RuntimeNone
		actions := session.Actions{session.ActionRename: {Available: true}, session.ActionArchive: {Available: true}}
		switch {
		case ok:
			runtime = session.RuntimeDetached
			actions[session.ActionOpen] = session.Availability{Available: true}
			if run.State == "running" || run.State == "starting" {
				actions[session.ActionStop] = session.Availability{Available: true}
			} else {
				actions[session.ActionStop] = session.Availability{Reason: "managed run is not currently running"}
			}
		case p.writerAbsent(t.ID):
			actions[session.ActionOpen] = session.Availability{Available: true}
			actions[session.ActionStop] = session.Availability{Reason: "no agentsctl-managed run is tracking this session"}
		default:
			runtime = session.RuntimeExternal
			reason := "external or unknown Codex writer cannot be attached or stopped safely"
			actions[session.ActionOpen] = session.Availability{Reason: reason}
			actions[session.ActionStop] = session.Availability{Reason: reason}
		}
		rows = append(rows, session.Session{Key: session.Key{Provider: session.ProviderCodex, ID: t.ID}, Name: value(t.Name), Summary: value(t.Preview), CWD: t.CWD, CreatedAt: time.Unix(t.CreatedAt, 0), UpdatedAt: time.Unix(t.UpdatedAt, 0), Activity: codexActivity(t), Runtime: runtime, Archived: archived, RunID: run.ID, Actions: actions})
	}
	if !archived {
		for _, r := range runs {
			if r.Provider != "codex" || r.SessionID != "" {
				continue
			}
			activity, runtime, name := session.ActivityStarting, session.RuntimeDetached, "Starting"
			actions := session.Actions{session.ActionOpen: {Available: true}, session.ActionStop: {Available: true}}
			if isTerminalRunState(r.State) {
				// This row never became a real Codex app-server thread — its
				// Key.ID is agentsctl's own run ID, not a thread ID
				// thread/archive would accept. r.Error (the startup
				// failure, e.g. a fork/exec error) is kept as diagnostic
				// Summary text but must not gate Archive: "why the run
				// failed" and "whether this row can be archived" are
				// unrelated. Archive() (below) detects this same
				// SessionID=="" + terminal-state shape and performs a local
				// state cleanup instead of calling the app-server.
				activity, runtime, name = session.ActivityFailed, session.RuntimeStopped, "Unbound run"
				actions = session.Actions{session.ActionArchive: {Available: true}}
			}
			rows = append(rows, session.Session{Key: session.Key{Provider: session.ProviderCodex, ID: r.ID}, Name: name, Summary: r.Error, CWD: r.CWD, CreatedAt: r.StartedAt, UpdatedAt: r.StartedAt, Activity: activity, Runtime: runtime, RunID: r.ID, Actions: actions})
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
	r, err := p.Runtime.Dispatch(ctx, prompt, cwd, baseline, managedEnvironment())
	if err != nil {
		return session.Session{}, err
	}
	createdAt := time.Now()
	actions := session.Actions{session.ActionOpen: {Available: true}, session.ActionStop: {Available: true}}
	return session.Session{Key: session.Key{Provider: session.ProviderCodex, ID: r.ID}, Name: "Starting", CWD: cwd, CreatedAt: createdAt, UpdatedAt: createdAt, Activity: session.ActivityStarting, Runtime: session.RuntimeDetached, RunID: r.ID, Actions: actions}, nil
}
func (p *Provider) Stop(ctx context.Context, k session.Key) error {
	runs, err := p.Store.Runs()
	if err != nil {
		return err
	}
	for _, r := range runs {
		if r.ID == k.ID || r.SessionID == k.ID {
			return p.Runtime.Stop(ctx, r.ID)
		}
	}
	return errors.New("refusing to stop a Codex writer not owned by agentsctl")
}

// Archive removes k's session. For an actual Codex thread (no local run
// record shares k.ID, or that record is already bound to a thread) this
// calls the app-server's native thread/archive. For an agentsctl-owned
// unbound run — a local run that started but never got proven to any
// app-server thread (localstate.Run.SessionID == "") — k.ID is agentsctl's
// own run ID, never a Codex thread ID, so it must never reach the
// app-server under any state, not just the terminal one List() actually
// offers Archive for: a running/starting unbound run is rejected outright
// rather than silently falling through to a native call that would misuse
// its ID. Only a terminal (failed/stale/stopped) unbound run is deleted,
// and that deletion re-verifies the same shape atomically (see
// localstate.Store.DeleteTerminalUnboundRun) so a run that changed between
// this Load and that delete — e.g. it got bound to a thread, or left its
// terminal state — is never wrongly removed.
func (p *Provider) Archive(ctx context.Context, k session.Key) error {
	runs, err := p.Store.Runs()
	if err != nil {
		return err
	}
	if r, ok := runs[k.ID]; ok && r.SessionID == "" {
		if !isTerminalRunState(r.State) {
			return fmt.Errorf("refusing to archive an active unbound Codex run: %s", k.ID)
		}
		return p.Store.DeleteTerminalUnboundRun(k.ID, isTerminalRunState)
	}
	return p.API.Archive(ctx, k.ID)
}

// isTerminalRunState reports whether a localstate.Run.State value is
// terminal — the run reached an end state without (or, for a previously-
// bound run, regardless of) further progress. Shared by List (which run
// shape becomes a diagnostic "Unbound run" row) and Archive (which local
// runs are eligible for local cleanup), so the two stay in agreement about
// what "terminal" means.
func isTerminalRunState(state string) bool {
	return state == "failed" || state == "stale" || state == "stopped"
}
func (p *Provider) Unarchive(ctx context.Context, k session.Key) error {
	return p.API.Unarchive(ctx, k.ID)
}

// fiveHourWindowDurationMins and weeklyWindowDurationMins are the
// windowDurationMins values the installed Codex CLI reports for its
// rolling 5-hour and weekly rate-limit windows, respectively. Usage
// classifies each RateLimitWindow by this duration rather than by its
// Primary/Secondary position -- the app-server has been observed to place
// either window in either slot (see RateLimitSnapshot's doc comment).
const (
	fiveHourWindowDurationMins = 300
	weeklyWindowDurationMins   = 10080
)

// Usage implements sessionctl.UsageSource via the app-server's
// account/rateLimits/read method (see AppServer.RateLimits) -- the same
// native, machine-readable transport List/Rename/Archive already use. Each
// of Primary/Secondary is classified into FiveHour/Weekly by its own
// WindowDurationMins, never by which slot it arrived in. A window with an
// unrecognized or absent WindowDurationMins is left unclassified (fail
// closed: never guessed into either bucket), so it simply doesn't
// contribute a FiveHour/Weekly reading.
func (p *Provider) Usage(ctx context.Context) (session.Usage, error) {
	limits, err := p.API.RateLimits(ctx)
	if err != nil {
		return session.Usage{}, err
	}
	usage := session.Usage{Provider: session.ProviderCodex}
	for _, w := range []*RateLimitWindow{limits.RateLimits.Primary, limits.RateLimits.Secondary} {
		if w == nil || w.WindowDurationMins == nil {
			continue
		}
		switch *w.WindowDurationMins {
		case fiveHourWindowDurationMins:
			usage.FiveHour = rateLimitWindow(w)
		case weeklyWindowDurationMins:
			usage.Weekly = rateLimitWindow(w)
		}
	}
	return usage, nil
}

// rateLimitWindow converts one app-server RateLimitWindow into the
// provider-neutral session.UsageWindow. A window with no ResetsAt means the
// backend didn't report a reset time for it -- reported as
// session.UsageUnknown (the zero value) so it renders as "not reported",
// never a false 0%; a 0% UsedPercent with a real ResetsAt is a valid,
// session.UsageAvailable reading. Codex's own transport has no limit-
// reached signal of its own (unlike Claude's usage probe -- see Issue #19
// and the DesignDoc's Usage capability), so this never reports
// session.UsageExhausted.
func rateLimitWindow(w *RateLimitWindow) session.UsageWindow {
	if w.ResetsAt == nil {
		return session.UsageWindow{}
	}
	return session.UsageWindow{State: session.UsageAvailable, Percent: w.UsedPercent, Reset: time.Unix(*w.ResetsAt, 0)}
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
	r, err := p.Runtime.ResumeExisting(ctx, s.Key.ID, s.CWD, managedEnvironment())
	if err != nil {
		return "", err
	}
	return r.ID, nil
}

func managedEnvironment() map[string]string {
	// Read the invoking agentsctl process here: the persistent supervisor's
	// inherited CODEX_EDITOR may belong to an earlier invocation.
	editor := os.Getenv("CODEX_EDITOR")
	if editor == "" {
		return nil
	}
	return map[string]string{"EDITOR": editor}
}

// Open implements sessionctl.Opener: it resolves s to a managed run --
// binding a still-external, writer-absent thread into one first if needed
// (see PrepareAttach) -- then forwards the real terminal to the
// supervisor's PTY for that run until detach (see
// internal/supervisor.Client.Attach). TUI lifetime and the managed
// process's lifetime are independent: Open returning does not stop the
// Codex CLI process, and the supervisor keeps it (and its PTY) alive
// across agentsctl restarts (see the DesignDoc's Codex supervisor
// Lifetime section).
func (p *Provider) Open(ctx context.Context, s session.Session, in *os.File, out io.Writer) error {
	runID, err := p.PrepareAttach(ctx, s)
	if err != nil {
		return err
	}
	return p.Runtime.Attach(ctx, runID, in, out)
}

// reconcile binds each locally-tracked, not-yet-bound managed run to at
// most one Codex app-server thread, per the DesignDoc's run-to-thread
// binding rule: a candidate thread must be new since the run's own
// pre-dispatch baseline, share its CWD, and be owned (writer lock) by the
// run's own process identity. Zero or multiple candidates never bind.
//
// The writer-lock ownership probe (owner below) is filesystem/process I/O,
// so it runs entirely against an unlocked Runs() snapshot, never inside
// localstate's exclusive lock -- Codex-specific observation like this must
// not become something every other localstate caller (a concurrent TUI
// reading pins, the supervisor saving an unrelated run) blocks on. Once a
// run's binding decision is computed, UpdateRunIf re-verifies -- atomically,
// under the lock, and without I/O -- that the record still matches this
// snapshot before applying it, so a run that changed since (bound,
// deleted, or otherwise mutated by a concurrent writer) is never
// clobbered by a now-stale decision; reconcile simply leaves it for the
// next List to reconsider.
func (p *Provider) reconcile(threads []Thread) error {
	runs, err := p.Store.Runs()
	if err != nil {
		return err
	}
	for id, r := range runs {
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
				owner = writerlock.OwnsWriterLock
			}
			owned, _ := owner(filepath.Join(p.API.CodexHome(), "thread-writer-locks", t.ID+".lock"), processinfo.Identity{PID: r.PID, StartTime: r.StartTime, UID: r.UID})
			if !base[t.ID] && filepath.Clean(t.CWD) == filepath.Clean(r.CWD) && owned {
				candidates = append(candidates, t.ID)
			}
		}
		next := r
		switch len(candidates) {
		case 1:
			next.SessionID = candidates[0]
			next.Error = ""
		case 0:
			continue
		default:
			next.Error = "ambiguous Codex thread binding; candidates were not guessed"
		}
		snapshot, decided := r, next
		if _, err := p.Store.UpdateRunIf(id,
			func(current localstate.Run) bool { return reflect.DeepEqual(current, snapshot) },
			func(localstate.Run) localstate.Run { return decided },
		); err != nil {
			return err
		}
	}
	return nil
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
