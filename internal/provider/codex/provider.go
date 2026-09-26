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

// ManagedRuntime stops legacy supervisor-managed Codex runs that were
// started before Dispatch moved to the shared app-server daemon. Nothing
// new is ever started through it: Dispatch talks to the daemon over RPC
// (see Provider.Dispatch), and Open connects a foreground client to the
// daemon (see Provider.Open).
type ManagedRuntime interface {
	Stop(context.Context, string) error
}

// ForegroundClient runs an interactive client process on the caller's
// terminal for the duration of one Open. It returns nil when the process
// exits successfully or the user detaches from it (Ctrl+]), after which
// the process has been ended and reaped; any other end is an error.
// terminal.ForegroundPTY is the production implementation.
type ForegroundClient interface {
	Run(ctx context.Context, path string, args []string, cwd string, in *os.File, out io.Writer) error
}

type Provider struct {
	Path    string
	API     AppServer
	Runner  base.Runner
	Store   *localstate.Store
	Runtime ManagedRuntime
	Daemon  DaemonLifecycle
	// Foreground runs the interactive Codex TUI client on the caller's
	// terminal (see Open). It is separate from Runner, which only captures
	// command output.
	Foreground  ForegroundClient
	WriterOwner func(string, processinfo.Identity) (bool, error)
	// ControlSocket overrides the shared app-server control socket used by
	// the Observer and Open preflight; empty means the daemon lifecycle
	// resolves the endpoint (see readySocket).
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

// path is the Codex CLI executable: Path, or "codex" from PATH.
func (p *Provider) path() string {
	if p.Path == "" {
		return "codex"
	}
	return p.Path
}

func (p *Provider) Available() error {
	_, err := exec.LookPath(p.path())
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
// caller's source of runtime observation. It supplies advisory Open, Stop,
// and Archive availability; each operation re-checks its own safety
// conditions against the daemon.
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
		observed := observe(t, func() bool { return p.writerAbsent(t.ID) })
		runtime := observed.Runtime
		actions := session.Actions{session.ActionRename: {Available: true}, session.ActionArchive: archiveAvailability(observed)}
		// Open follows the observation, not whether agentsctl manages a run:
		// a legacy managed run holding the writer lock outside the shared
		// app-server is as unopenable as any other external writer. An
		// Unknown observation stays openable; Open establishes the status
		// itself.
		if observed.Runtime == session.RuntimeExternal {
			actions[session.ActionOpen] = session.Availability{Reason: errExternalWriter.Error()}
		} else {
			actions[session.ActionOpen] = session.Availability{Available: true}
		}
		switch {
		case ok:
			// An agentsctl-managed run keeps its existing Runtime whatever
			// was observed: it is not the shared app-server's runtime.
			runtime = session.RuntimeDetached
			if run.State == "running" || run.State == "starting" {
				actions[session.ActionStop] = session.Availability{Available: true}
			} else {
				actions[session.ActionStop] = session.Availability{Reason: "managed run is not currently running"}
			}
		case observed.Runtime == session.RuntimeExternal:
			actions[session.ActionStop] = session.Availability{Reason: "external or unknown Codex writer cannot be stopped safely"}
		case observed.Activity == session.ActivityWorking || observed.Activity == session.ActivityNeedsInput:
			actions[session.ActionStop] = session.Availability{Available: true}
		case observed.Activity == session.ActivityIdle || observed.Activity == session.ActivityFailed:
			actions[session.ActionStop] = session.Availability{Reason: "session is not running"}
		default:
			actions[session.ActionStop] = session.Availability{Reason: "Codex activity is unknown"}
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

// archiveAvailability is the advisory Archive availability of a thread
// observed as observed: a running session (Working or waiting on the user)
// is refused, and a thread another process writes is never archived.
// An observation that establishes neither (e.g. Unknown while the Observer
// is down) leaves Archive offered; Archive itself re-checks with the
// daemon before acting (see archivable).
func archiveAvailability(observed observation) session.Availability {
	switch {
	case observed.Runtime == session.RuntimeExternal:
		return session.Availability{Reason: errArchiveExternalWriter.Error()}
	case observed.Activity == session.ActivityWorking || observed.Activity == session.ActivityNeedsInput:
		return session.Availability{Reason: errArchiveRunning.Error()}
	}
	return session.Availability{Available: true}
}

// sortedKeys returns keys ordered by ID so PreviousKeys does not depend on
// Go's randomized map iteration order over the run records.
func sortedKeys(keys []session.Key) []session.Key {
	sort.Slice(keys, func(i, j int) bool { return keys[i].ID < keys[j].ID })
	return keys
}

// awaitingBootstrapBind reports whether r is a legacy rename-only bootstrap
// run that reconcile has not yet bound to its real thread. Only runs
// persisted before Dispatch moved to the shared daemon have this shape. Like every unbound
// run it cannot be opened (see openWhileStarting); it only gets a more
// specific reason, since its first model turn is what creates the thread
// reconciliation must bind.
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

// openWhileStarting is the Open availability of a provisional Starting row.
// Its Key is agentsctl's run ID, not a Codex thread ID, so there is nothing
// to resume until reconcile binds the run to its thread.
func openWhileStarting(awaitingBind bool) session.Availability {
	if awaitingBind {
		return session.Availability{Reason: "Codex rename-only session is still starting"}
	}
	return session.Availability{Reason: "Codex session is still starting and has no bound thread yet"}
}

// applyPendingRenames renames, through the same native rename an existing
// session uses, each thread that reconcile has just bound to a legacy run
// still carrying a PendingRename, and clears the pending state. Dispatch no
// longer records one; this only settles runs persisted before it moved to
// the shared daemon. There is one
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

// Stop interrupts the exact shared-daemon turn that is active when the
// operation resolves it. Legacy locally managed runs keep their existing
// process-based Stop path.
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
	events := newTurnLifecycleEvents()
	socket, err := p.readySocket(ctx)
	if err != nil {
		return err
	}
	return connectDaemon(ctx, socket, events.observe, func(conn *rpcConn) error {
		state, err := readCurrentTurnState(ctx, conn, k.ID)
		var turnID string
		switch {
		case err == nil:
			if state.status.Type == statusNotLoaded && !p.writerAbsent(k.ID) {
				return errors.New("external or unknown Codex writer cannot be stopped safely")
			}
			turnID = state.activeTurnID
		case errors.Is(err, errTurnsListUnmaterialized):
			turnID = p.runtime().activeTurnHint(k.ID)
			if turnID == "" {
				return fmt.Errorf("cannot safely stop active Codex thread %s: current turn is not materialized yet and its exact turn ID is unavailable", k.ID)
			}
		default:
			return fmt.Errorf("resolve active Codex turn: %w", err)
		}
		if turnID == "" {
			p.runtime().forgetActiveTurn(k.ID, "")
			return nil
		}
		if err := interruptTurn(ctx, conn, k.ID, turnID); err != nil {
			if err := p.reconcileStopRace(ctx, conn, k.ID, turnID, err); err != nil {
				return err
			}
			p.runtime().forgetActiveTurn(k.ID, "")
			return nil
		}
		if err := events.waitInactive(ctx, conn, k.ID); err != nil {
			return fmt.Errorf("wait for Codex turn %s to stop: %w", turnID, err)
		}
		if err := p.confirmStoppedTurn(ctx, conn, k.ID, turnID); err != nil {
			return err
		}
		p.runtime().forgetActiveTurn(k.ID, "")
		return nil
	})
}

func (p *Provider) reconcileStopRace(ctx context.Context, conn *rpcConn, threadID, turnID string, interruptErr error) error {
	if err := p.confirmStoppedTurn(ctx, conn, threadID, turnID); err != nil {
		return errors.Join(fmt.Errorf("interrupt Codex turn %s: %w", turnID, interruptErr), err)
	}
	return nil
}

func (p *Provider) confirmStoppedTurn(ctx context.Context, conn *rpcConn, threadID, turnID string) error {
	state, err := readCurrentTurnState(ctx, conn, threadID)
	if err != nil {
		return fmt.Errorf("confirm Codex turn %s stopped: %w", turnID, err)
	}
	if state.activeTurnID == "" {
		return nil
	}
	if state.activeTurnID != turnID {
		return fmt.Errorf("Codex turn %s ended, but thread %s now has different active turn %s; refusing to interrupt it", turnID, threadID, state.activeTurnID)
	}
	return fmt.Errorf("Codex turn %s remains active on thread %s", turnID, threadID)
}

// Archive removes k's session. For an actual Codex thread (no local run
// record shares k.ID, or that record is already bound to a thread) this
// calls native thread/archive on the shared daemon, and only once the
// daemon itself confirmed the thread is at rest (see archivable). For an
// agentsctl-owned unbound run — a local run that started but never got proven to any
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
	return p.withDaemon(ctx, func(conn *rpcConn) error {
		if err := p.archivable(ctx, conn, k.ID); err != nil {
			return err
		}
		return conn.call(ctx, "thread/archive", map[string]any{"threadId": k.ID}, nil)
	})
}

var (
	errArchiveRunning        = errors.New("codex session is running; stop it before archiving")
	errArchiveExternalWriter = errors.New("external or unknown Codex writer prevents archive")
)

// archivable re-reads threadID's status on conn, right before Archive
// sends thread/archive on it, and refuses unless the thread is at rest.
// The daemon archives a thread it has loaded by shutting its runtime down,
// so a running turn -- Working or waiting on the user -- must never reach
// thread/archive: Archive would become an implicit Stop. The status is
// read the same way the Observer reads it (see observeThread): a loaded
// thread's native status decides on its own; a notLoaded one is at rest
// only while no other process holds its writer lock. Anything else,
// including a status this version does not know, is refused. The row's
// cached Actions play no part here.
func (p *Provider) archivable(ctx context.Context, conn *rpcConn, threadID string) error {
	t, err := readThread(ctx, conn, threadID)
	switch {
	case err != nil:
		return fmt.Errorf("thread/read %s: %w", threadID, err)
	case t.ID != threadID || t.Status.Type == "":
		return fmt.Errorf("thread/read %s: malformed response", threadID)
	}
	observed := observeThread(t.Status, func() bool { return p.writerAbsent(threadID) })
	switch {
	case observed.Runtime == session.RuntimeExternal:
		return errArchiveExternalWriter
	case observed.Activity == session.ActivityWorking || observed.Activity == session.ActivityNeedsInput:
		return errArchiveRunning
	case observed.Activity == session.ActivityIdle || observed.Activity == session.ActivityFailed:
		return nil
	}
	return fmt.Errorf("codex thread %s has status %q; refusing to archive it", threadID, t.Status.Type)
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

// Unarchive restores k's thread through the shared daemon. Unlike Archive
// it touches no runtime, so it needs no preflight.
func (p *Provider) Unarchive(ctx context.Context, k session.Key) error {
	return p.withDaemon(ctx, func(conn *rpcConn) error {
		return conn.call(ctx, "thread/unarchive", map[string]any{"threadId": k.ID}, nil)
	})
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

// Rename sets k's thread name through the shared daemon. The daemon
// patches thread metadata whether or not a turn is running, so a running
// session can be renamed without interrupting it.
func (p *Provider) Rename(ctx context.Context, k session.Key, name string) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("name is required")
	}
	if err := p.withDaemon(ctx, func(conn *rpcConn) error { return setThreadName(ctx, conn, k.ID, name) }); err != nil {
		return err
	}
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

var (
	errUnboundRun     = errors.New("codex session is still starting and has no bound thread yet")
	errExternalWriter = errors.New("external or unknown Codex writer prevents remote Open")
)

// openPreflightTimeout bounds the daemon status check before Open hands the
// terminal to Codex.
const openPreflightTimeout = 10 * time.Second

// Open implements sessionctl.Opener by running the Codex TUI in the
// foreground as a client of the shared app-server daemon:
//
//	codex --remote unix://<socket> resume <thread ID>
//
// in s.CWD, with agentsctl's own environment, through Foreground: an
// Open-scoped PTY bridged to the caller's terminal. It returns when that
// client exits or the user detaches with Ctrl+], which ends only the client
// process. The client's lifetime is not the thread's: its connection
// closing leaves the thread and any running turn in the daemon, and nothing
// here stops or interrupts them.
//
// Before launching, Open proves s is a canonical Codex thread (see
// openThreadID), ensures the daemon, and checks the thread's current
// status on it (see preflightOpen); the row's Actions are only advisory.
// An explicit --remote never falls back to an embedded app-server, and
// neither does Open: a client failure is returned as is.
func (p *Provider) Open(ctx context.Context, s session.Session, in *os.File, out io.Writer) error {
	threadID, err := p.openThreadID(s)
	if err != nil {
		return err
	}
	if p.Foreground == nil {
		return errors.New("codex foreground launcher is not configured")
	}
	socket, err := p.readySocket(ctx)
	if err != nil {
		return err
	}
	if err := p.preflightOpen(ctx, socket, threadID); err != nil {
		return err
	}
	if err := p.Foreground.Run(ctx, p.path(), remoteResumeArgs(socket, threadID), s.CWD, in, out); err != nil {
		return fmt.Errorf("codex remote resume %s: %w", threadID, err)
	}
	return nil
}

// remoteResumeArgs is the Codex CLI invocation that resumes threadID as a
// client of the app-server on the Unix socket at socket. Codex takes
// everything after "unix://" as the path, so it is passed verbatim.
func remoteResumeArgs(socket, threadID string) []string {
	return []string{"--remote", "unix://" + socket, "resume", threadID}
}

// openThreadID returns the Codex thread ID s names. A row still keyed by a
// provisional run ID has none, and a row whose run is bound to a different
// thread is refused rather than guessed. It reads the current run state,
// not the caller's possibly stale row.
func (p *Provider) openThreadID(s session.Session) (string, error) {
	if s.Key.ID == "" {
		return "", errors.New("codex session has no thread ID")
	}
	if p.Store == nil {
		if s.RunID != "" {
			return "", errors.New("codex run state is not configured")
		}
		return s.Key.ID, nil
	}
	runs, err := p.Store.Runs()
	if err != nil {
		return "", fmt.Errorf("check managed run before open: %w", err)
	}
	runID := s.RunID
	if runID == "" {
		// A row that lost its RunID but is still keyed by a run ID.
		if _, isRun := runs[s.Key.ID]; !isRun {
			return s.Key.ID, nil
		}
		runID = s.Key.ID
	}
	run, ok := runs[runID]
	switch {
	case !ok:
		return "", fmt.Errorf("codex managed run %s is not tracked", runID)
	case awaitingBootstrapBind(run):
		return "", errors.New("codex rename-only session is still starting and cannot be opened until its thread is bound")
	case run.SessionID == "":
		return "", errUnboundRun
	case run.SessionID != s.Key.ID:
		return "", fmt.Errorf("codex managed run %s is bound to thread %s, not %s", runID, run.SessionID, s.Key.ID)
	}
	return run.SessionID, nil
}

// preflightOpen reads threadID's current status from the shared daemon on
// a connection of its own, closed before Open launches anything. A thread
// the daemon has loaded is its own and needs no writer check (the daemon
// holds the lock); a notLoaded thread may be opened only while no other
// process holds its writer lock. Anything it cannot establish fails closed.
func (p *Provider) preflightOpen(ctx context.Context, socket, threadID string) error {
	ctx, cancel := context.WithTimeout(ctx, openPreflightTimeout)
	defer cancel()
	return connectDaemon(ctx, socket, nil, func(conn *rpcConn) error { return p.checkOpenable(ctx, conn, threadID) })
}

func (p *Provider) checkOpenable(ctx context.Context, conn *rpcConn, threadID string) error {
	t, err := readThread(ctx, conn, threadID)
	switch {
	case err != nil:
		return fmt.Errorf("thread/read %s: %w", threadID, err)
	case t.ID != threadID || t.Status.Type == "":
		return fmt.Errorf("thread/read %s: malformed response", threadID)
	case hiddenThread(t):
		return fmt.Errorf("codex thread %s is an internal thread and cannot be opened", threadID)
	case t.Status.Type == statusNotLoaded && !p.writerAbsent(threadID):
		return errExternalWriter
	}
	return nil
}

// reconcile binds each locally-tracked, not-yet-bound legacy managed run
// (one persisted before Dispatch moved to the shared daemon) to at most one
// Codex app-server thread: a candidate thread must be new since the run's
// own pre-dispatch baseline, share its CWD, and be owned (writer lock) by
// the run's own process identity. Zero or multiple candidates never bind.
// The ownership proof is what keeps a thread Dispatch started on the shared
// daemon -- whose writer lock the daemon holds -- from being claimed by a
// legacy run in the same CWD.
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
