package codex

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	base "github.com/tingtt/agentsctl/internal/provider"
	"github.com/tingtt/agentsctl/internal/session"
	"golang.org/x/sys/unix"
)

// ForegroundClient runs an interactive client process on the caller's
// terminal for the duration of one Open. It returns nil when the process
// exits successfully or the user detaches from it (Ctrl+]), after which
// the process has been ended and reaped; any other end is an error.
// terminal.ForegroundPTY is the production implementation.
type ForegroundClient interface {
	Run(ctx context.Context, path string, args []string, cwd string, in *os.File, out io.Writer) error
}

type Provider struct {
	Path   string
	API    AppServer
	Runner base.Runner
	Daemon DaemonLifecycle
	// Foreground runs the interactive Codex TUI client on the caller's
	// terminal (see Open). It is separate from Runner, which only captures
	// command output.
	Foreground ForegroundClient
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
	rows := p.sessionRows(threads, archived, func(t Thread, writerFree func() bool) observation {
		return observeThread(t.Status, writerFree)
	})
	if !archived {
		// Keep the Observer's catalog in step with the newer one List just
		// fetched, so its snapshots never lag behind what List returned.
		p.syncObservedCatalog(fetch, threads)
	}
	return rows, nil
}

// sessionRows normalizes a native thread catalog into session rows, one per
// visible thread, keyed by its canonical thread ID. It is shared by List and
// the Observer snapshot so both build rows the same way; only
// Activity/Runtime come from observe, the caller's source of runtime
// observation. It supplies advisory Open, Stop, and Archive availability;
// each operation re-checks its own safety conditions against the daemon.
func (p *Provider) sessionRows(threads []Thread, archived bool, observe func(Thread, func() bool) observation) []session.Session {
	rows := make([]session.Session, 0, len(threads))
	for _, t := range threads {
		if hiddenThread(t) {
			continue
		}
		observed := observe(t, func() bool { return p.writerAbsent(t.ID) })
		actions := session.Actions{session.ActionRename: {Available: true}, session.ActionArchive: archiveAvailability(observed)}
		// An Unknown observation stays openable; Open establishes the
		// status itself.
		if observed.Runtime == session.RuntimeExternal {
			actions[session.ActionOpen] = session.Availability{Reason: errExternalWriter.Error()}
		} else {
			actions[session.ActionOpen] = session.Availability{Available: true}
		}
		switch {
		case observed.Runtime == session.RuntimeExternal:
			actions[session.ActionStop] = session.Availability{Reason: "external or unknown Codex writer cannot be stopped safely"}
		case observed.Activity == session.ActivityWorking || observed.Activity == session.ActivityNeedsInput:
			actions[session.ActionStop] = session.Availability{Available: true}
		case observed.Activity == session.ActivityIdle || observed.Activity == session.ActivityFailed:
			actions[session.ActionStop] = session.Availability{Reason: "session is not running"}
		default:
			actions[session.ActionStop] = session.Availability{Reason: "Codex activity is unknown"}
		}
		rows = append(rows, session.Session{Key: session.Key{Provider: session.ProviderCodex, ID: t.ID}, Name: value(t.Name), Summary: value(t.Preview), CWD: t.CWD, CreatedAt: time.Unix(t.CreatedAt, 0), UpdatedAt: time.Unix(t.UpdatedAt, 0), Activity: observed.Activity, Runtime: observed.Runtime, Archived: archived, Actions: actions})
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

// Stop interrupts the exact shared-daemon turn that is active on k's thread
// when the operation resolves it, then waits until the daemon reports the
// thread inactive and confirms that same turn ended. It never interrupts a
// turn other than the one it resolved, and a thread the daemon has not
// loaded is refused while another process holds its writer lock.
func (p *Provider) Stop(ctx context.Context, k session.Key) error {
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

// Archive archives k's thread with native thread/archive on the shared
// daemon, and only once the daemon itself confirmed the thread is at rest
// (see archivable).
func (p *Provider) Archive(ctx context.Context, k session.Key) error {
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
	return p.withDaemon(ctx, func(conn *rpcConn) error { return setThreadName(ctx, conn, k.ID, name) })
}

var errExternalWriter = errors.New("external or unknown Codex writer prevents remote Open")

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
// Before launching, Open ensures the daemon and checks the thread's current
// status on it (see preflightOpen); the row's Actions are only advisory.
// An explicit --remote never falls back to an embedded app-server, and
// neither does Open: a client failure is returned as is.
func (p *Provider) Open(ctx context.Context, s session.Session, in *os.File, out io.Writer) error {
	threadID := s.Key.ID
	if threadID == "" {
		return errors.New("codex session has no thread ID")
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

// writerAbsent reports whether no process holds threadID's writer lock.
// Its only use is a thread the shared daemon reports notLoaded: a free lock
// means the thread is dormant, a held one means a runtime outside the daemon
// is writing it (see observeThread). It never identifies which process holds
// the lock. Anything it cannot establish (unknown Codex home, an untrusted
// lock file, an I/O error) reports false: fail closed.
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
