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
	"sort"
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
	Daemon      DaemonLifecycle
	WriterOwner func(string, processinfo.Identity) (bool, error)
	// ControlSocket overrides the shared app-server control socket the
	// Observer connects to; empty means the default under the resolved
	// Codex home (see resolveCodexHome).
	ControlSocket string

	// writerFree replaces the writer-lock probe (see writerAbsent); tests
	// only.
	writerFree func(threadID string) bool
	obs        observerHub
}

// codexHome is the Codex home the app-server reported, or, before any
// short-lived app-server call has reported one, the one resolved the way
// Codex itself resolves it.
func (p *Provider) codexHome() string {
	if home := p.API.CodexHome(); home != "" {
		return home
	}
	home, err := resolveCodexHome(os.Getenv, os.UserHomeDir)
	if err != nil {
		return ""
	}
	return home
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
	var fetch uint64
	if !archived {
		fetch = p.runtime().beginCatalogFetch()
	}
	threads, err := p.API.List(ctx, archived)
	if err != nil {
		return nil, err
	}
	if !archived {
		_ = p.reconcile(threads)
		p.applyPendingRenames(ctx, threads)
	}
	runs, _ := p.Store.Runs()
	rows := p.sessionRows(threads, runs, archived, func(t Thread, writerFree func() bool) observation {
		return observeThread(t.Status, writerFree)
	})
	if !archived {
		// Until the shared app-server connection replaces them, the rows
		// the existing execution path produces (provisional runs,
		// reconciled threads) only become visible through List; keep the
		// Observer's catalog in step so its snapshots include them.
		p.syncObservedCatalog(fetch, threads)
	}
	return rows, nil
}

// sessionRows normalizes a thread catalog plus the local managed-run state
// into session rows. It is shared by List and the Observer snapshot so both
// build rows the same way; only Activity/Runtime come from observe, the
// caller's source of runtime observation. Which actions a row offers never
// depends on observe: it stays with the managed-run and writer-lock rules
// below.
func (p *Provider) sessionRows(threads []Thread, runs map[string]localstate.Run, archived bool, observe func(Thread, func() bool) observation) []session.Session {
	managed := map[string]localstate.Run{}
	renameFailed := map[string]string{}
	// provisional collects, per thread, the keys its bound runs were listed
	// under while still unbound (see the unbound-run rows below). Only a
	// run whose SessionID is set -- i.e. one reconcile proved to this
	// thread, or one started to resume it -- contributes, regardless of
	// the run's state: a run that already stopped must not orphan the
	// Starting row's identity.
	provisional := map[string][]session.Key{}
	for _, r := range runs {
		if r.Provider != "codex" || r.SessionID == "" {
			continue
		}
		provisional[r.SessionID] = append(provisional[r.SessionID], session.Key{Provider: session.ProviderCodex, ID: r.ID})
		if r.RenameError != "" {
			renameFailed[r.SessionID] = r.RenameError
		}
		if r.State == "running" || r.State == "starting" {
			managed[r.SessionID] = r
		}
	}
	rows := make([]session.Session, 0, len(threads)+len(runs))
	for _, t := range threads {
		if hiddenThread(t) {
			continue
		}
		run, ok := managed[t.ID]
		summary := value(t.Preview)
		if msg, failed := renameFailed[t.ID]; failed && value(t.Name) == "" {
			// A rename-only session whose name could not be applied would
			// otherwise show only its bootstrap preview, with no hint the
			// requested name was lost.
			summary = msg
		}
		var free *bool
		writerFree := func() bool {
			if free == nil {
				v := p.writerAbsent(t.ID)
				free = &v
			}
			return *free
		}
		observed := observe(t, writerFree)
		runtime := observed.Runtime
		actions := session.Actions{session.ActionRename: {Available: true}, session.ActionArchive: {Available: true}}
		switch {
		case ok:
			// An agentsctl-managed run keeps its existing Runtime whatever
			// was observed: it is not the shared app-server's runtime.
			runtime = session.RuntimeDetached
			actions[session.ActionOpen] = session.Availability{Available: true}
			if run.State == "running" || run.State == "starting" {
				actions[session.ActionStop] = session.Availability{Available: true}
			} else {
				actions[session.ActionStop] = session.Availability{Reason: "managed run is not currently running"}
			}
		case writerFree():
			actions[session.ActionOpen] = session.Availability{Available: true}
			actions[session.ActionStop] = session.Availability{Reason: "no agentsctl-managed run is tracking this session"}
		default:
			reason := "external or unknown Codex writer cannot be attached or stopped safely"
			actions[session.ActionOpen] = session.Availability{Reason: reason}
			actions[session.ActionStop] = session.Availability{Reason: reason}
		}
		rows = append(rows, session.Session{Key: session.Key{Provider: session.ProviderCodex, ID: t.ID}, Name: value(t.Name), Summary: summary, CWD: t.CWD, CreatedAt: time.Unix(t.CreatedAt, 0), UpdatedAt: time.Unix(t.UpdatedAt, 0), Activity: observed.Activity, Runtime: runtime, Archived: archived, RunID: run.ID, PreviousKeys: sortedKeys(provisional[t.ID]), Actions: actions})
	}
	if !archived {
		for _, r := range runs {
			if r.Provider != "codex" || r.SessionID != "" {
				continue
			}
			// An unbound run is listed under its run ID, a provisional Key:
			// once reconcile proves the run to a thread, the thread's row
			// (Key.ID = thread ID) replaces it and names this Key in
			// PreviousKeys above. Nothing here guesses that link.
			activity, runtime, name := session.ActivityStarting, session.RuntimeDetached, startingName(awaitingBootstrapBind(r))
			actions := session.Actions{session.ActionOpen: openWhileStarting(awaitingBootstrapBind(r)), session.ActionStop: {Available: true}}
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
	return rows
}

// sortedKeys returns keys ordered by ID so PreviousKeys does not depend on
// Go's randomized map iteration order over the run records.
func sortedKeys(keys []session.Key) []session.Key {
	sort.Slice(keys, func(i, j int) bool { return keys[i].ID < keys[j].ID })
	return keys
}

func (p *Provider) Dispatch(ctx context.Context, prompt, cwd string) (session.Session, error) {
	// A composer input that is only `/rename <name>` is never forwarded to
	// Codex: as an initial prompt it would reach the model as plain text.
	// The requested name stays with agentsctl (see recordPendingRename) and
	// the model only ever sees a fixed bootstrap prompt.
	var pendingRename string
	if name, isRename := parseRenameOnly(prompt); isRename {
		if name == "" {
			return session.Session{}, errors.New("name must not be empty")
		}
		prompt, pendingRename = renameBootstrapPrompt, name
	}
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
	if pendingRename != "" {
		if err := p.recordPendingRename(ctx, r.ID, pendingRename); err != nil {
			return session.Session{}, err
		}
	}
	createdAt := time.Now()
	actions := session.Actions{session.ActionOpen: openWhileStarting(pendingRename != ""), session.ActionStop: {Available: true}}
	return session.Session{Key: session.Key{Provider: session.ProviderCodex, ID: r.ID}, Name: startingName(pendingRename != ""), CWD: cwd, CreatedAt: createdAt, UpdatedAt: createdAt, Activity: session.ActivityStarting, Runtime: session.RuntimeDetached, RunID: r.ID, Actions: actions}, nil
}

// awaitingBootstrapBind reports whether r is a rename-only bootstrap run
// that reconcile has not yet bound to its real thread.
//
// Invariant: a rename-only bootstrap run must not be attached while it is
// still unbound. Its first model turn is what creates the listed/resumable
// thread that reconciliation must bind before the session can safely be
// treated as a normal Codex session; attaching the provisional run earlier
// can split the run and the thread into separate identities. Stop stays
// available. Ordinary Starting runs carry no PendingRename and are
// unaffected.
func awaitingBootstrapBind(r localstate.Run) bool {
	return r.PendingRename != "" && r.SessionID == ""
}

// startingName is the display name of a provisional Starting row. A rename-only
// bootstrap run says what it is waiting for; Activity stays
// session.ActivityStarting either way.
func startingName(awaitingRename bool) string {
	if awaitingRename {
		return "Starting (Waiting rename)"
	}
	return "Starting"
}

// openWhileStarting is the Open availability of a provisional Starting row:
// available, except while the run is a rename-only bootstrap awaiting its
// thread (see awaitingBootstrapBind).
func openWhileStarting(awaitingBind bool) session.Availability {
	if awaitingBind {
		return session.Availability{Reason: "Codex rename-only session is still starting"}
	}
	return session.Availability{Available: true}
}

// recordPendingRename attaches name to the just-started run runID. It runs
// after the supervisor has recorded the run, so the name lives in the same
// local run state reconciliation already reads; a concurrent bind of the
// run only ever adds SessionID, which this update leaves alone. If the name
// cannot be recorded the run is stopped rather than left running a
// bootstrap turn nothing will ever rename.
func (p *Provider) recordPendingRename(ctx context.Context, runID, name string) error {
	applied, err := p.Store.UpdateRunIf(runID,
		func(localstate.Run) bool { return true },
		func(r localstate.Run) localstate.Run { r.PendingRename = name; return r },
	)
	if err == nil && !applied {
		err = errors.New("started run is not tracked")
	}
	if err == nil {
		return nil
	}
	err = fmt.Errorf("record pending rename: %w", err)
	if stopErr := p.Runtime.Stop(ctx, runID); stopErr != nil {
		err = errors.Join(err, fmt.Errorf("stop run: %w", stopErr))
	}
	return err
}

// applyPendingRenames renames, through the same native rename an existing
// session uses, each thread that reconcile has just bound to a run still
// carrying a PendingRename, and clears the pending state. There is one
// attempt: the thread exists whatever happens, so a failure is recorded on
// the run (RenameError, surfaced by List) and an ordinary rename of the
// thread is the retry -- no retry loop of its own. A List cancelled
// mid-attempt says nothing about the rename, so it leaves the name pending.
// The applied name is written into threads so the same List already shows
// it.
func (p *Provider) applyPendingRenames(ctx context.Context, threads []Thread) {
	runs, err := p.Store.Runs()
	if err != nil {
		return
	}
	index := make(map[string]int, len(threads))
	for i, t := range threads {
		index[t.ID] = i
	}
	for id, r := range runs {
		if r.Provider != "codex" || r.PendingRename == "" || r.SessionID == "" {
			continue
		}
		i, listed := index[r.SessionID]
		if !listed {
			continue
		}
		name, threadID := r.PendingRename, r.SessionID
		renameErr := p.Rename(ctx, session.Key{Provider: session.ProviderCodex, ID: threadID}, name)
		if renameErr != nil && ctx.Err() != nil {
			continue
		}
		if renameErr == nil {
			threads[i].Name = &name
		}
		_, _ = p.Store.UpdateRunIf(id,
			func(current localstate.Run) bool {
				return current.PendingRename == name && current.SessionID == threadID
			},
			func(current localstate.Run) localstate.Run {
				current.PendingRename = ""
				if renameErr != nil {
					current.RenameError = fmt.Sprintf("rename to %q failed: %v", name, renameErr)
				}
				return current
			},
		)
	}
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
	if err := p.API.Archive(ctx, k.ID); err != nil {
		return err
	}
	p.catalogChanged()
	return nil
}

// catalogChanged asks the Observer's connection to re-read the catalog
// after a change made through the short-lived app-server, which the shared
// daemon does not broadcast.
func (p *Provider) catalogChanged() { p.runtime().requestCatalog() }

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
	if err := p.API.Unarchive(ctx, k.ID); err != nil {
		return err
	}
	p.catalogChanged()
	return nil
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
	if err := p.API.Rename(ctx, k.ID, name); err != nil {
		return err
	}
	p.catalogChanged()
	p.clearRenameError(k.ID)
	return nil
}

// clearRenameError forgets a failed pending rename of threadID once the
// thread has been renamed by hand: the failure it recorded is resolved. Best
// effort -- the rename itself already succeeded.
func (p *Provider) clearRenameError(threadID string) {
	if p.Store == nil {
		return
	}
	runs, err := p.Store.Runs()
	if err != nil {
		return
	}
	for id, r := range runs {
		if r.SessionID != threadID || r.RenameError == "" {
			continue
		}
		_, _ = p.Store.UpdateRunIf(id,
			func(current localstate.Run) bool { return current.SessionID == threadID && current.RenameError != "" },
			func(current localstate.Run) localstate.Run { current.RenameError = ""; return current },
		)
	}
}

func (p *Provider) PrepareAttach(ctx context.Context, s session.Session) (string, error) {
	if s.RunID != "" {
		// The row's Actions are advisory: a stale or hand-built session
		// still reaches here, so the invariant is enforced on the current
		// run record rather than on what the caller believes.
		runs, err := p.Store.Runs()
		if err != nil {
			return "", fmt.Errorf("check managed run before attach: %w", err)
		}
		if awaitingBootstrapBind(runs[s.RunID]) {
			return "", errors.New("codex rename-only session is still starting and cannot be attached until its thread is bound")
		}
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
			owned, _ := owner(writerLockPath(p.API.CodexHome(), t.ID), processinfo.Identity{PID: r.PID, StartTime: r.StartTime, UID: r.UID})
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

// writerAbsent reports whether no process holds threadID's writer lock.
// Anything it cannot establish (unknown Codex home, an untrusted lock file,
// an I/O error) reports false: fail closed.
func (p *Provider) writerAbsent(id string) bool {
	if p.writerFree != nil {
		return p.writerFree(id)
	}
	home := p.codexHome()
	if home == "" {
		return false
	}
	path := writerLockPath(home, id)
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
