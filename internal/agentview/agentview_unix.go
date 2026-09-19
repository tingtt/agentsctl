//go:build darwin || linux

package agentview

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/tingtt/agentsctl/internal/session"
	"github.com/tingtt/agentsctl/internal/sessionctl"
	"golang.org/x/term"
)

// Runtime is the Agent View composition root: the terminal event loop
// wiring physical input decoding (input_unix.go), UI state transitions
// (update.go), rendering (render.go), and every provider operation
// through sessionctl.Controller -- never a concrete provider package
// (see the package doc comment). CWD is the directory agentsctl was
// started in, the basis for directory Scope.
type Runtime struct {
	Controller sessionctl.Controller
	State      State
	Input      *os.File
	Output     io.Writer
	CWD        string
	ReadKey    func(*bufio.Reader) (KeyEvent, error)
	// Worktrees discovers the additional ScopeDescendants roots for CWD
	// (see session.Scope.WorktreeDirectories) -- e.g.
	// internal/workspace.Worktrees in production. Nil skips discovery,
	// degrading ScopeDescendants to just CWD's own subtree; Runtime never
	// performs this git/filesystem I/O itself (see the DesignDoc's
	// git/filesystem discovery -> normalized scope roots -> pure session
	// filtering dependency direction).
	Worktrees func(ctx context.Context, dir string) []string

	// usageCh receives incremental sessionctl.Controller.UsageStream
	// results from refreshUsageAsync's background usage fetch, each tagged
	// with the reload cycle that started it (usageGen) -- see
	// refreshUsageAsync. Lazily initialized (nil is a valid zero value): a
	// Runtime built directly for a test that only calls requestReload/act,
	// with no event loop draining it, never blocks on it either, since
	// UsageStream only ever starts a goroutine for a provider that
	// actually implements UsageSource (see sessionctl.Controller.
	// UsageStream).
	usageCh  chan usageEvent
	usageGen int

	// catalogCh receives one catalogEvent per requestReload cycle, from
	// the background goroutine requestReload starts -- see its doc
	// comment. Lazily initialized (nil is a valid zero value) the same way
	// usageCh is.
	catalogCh chan catalogEvent
	// catalogGen is the current reload cycle's generation: eventLoop's
	// catalogCh case drops any catalogEvent whose gen doesn't match, so an
	// older, superseded reload's ProviderSnapshot (e.g. a slow ChatGPT
	// List that lost a race with a second Ctrl+L) can never overwrite a
	// newer cycle's already-applied result.
	catalogGen int
	// catalogCancel cancels the most recently started reload cycle's own
	// child context (see requestReload) -- called again (superseding the
	// previous cycle) each time requestReload runs, and once more when Run
	// returns, so no reload goroutine outlives the overview. It never
	// cancels ctx itself, so the underlying provider/runtime (e.g. the
	// ChatGPT discovery bridge) stays reusable for the next reload.
	catalogCancel context.CancelFunc

	// providerSnapshots is Agent View's provider snapshot store: the
	// latest known sessions + refresh warning for every provider heard
	// from so far, retained across reload cycles (see applyProviderUpdate/
	// recomputeRows) rather than rebuilt from scratch each cycle -- so a
	// provider that hasn't reported yet in the current cycle (e.g. a
	// Refresher still enumerating in the background) never disappears
	// from Rows, and a failed refresh only sets that provider's warning
	// without discarding its previously-known sessions. Only ever touched
	// from the eventLoop goroutine. Lazily initialized the same way
	// catalogCh is.
	providerSnapshots map[session.ProviderID]providerState
	// currentScope is the directory Scope basis (including any resolved
	// worktree directories) most recently computed by requestReload --
	// reused by both the catalogCh and observerCh cases to re-merge
	// providerSnapshots, since an Observer update can arrive independent
	// of any specific reload cycle and must not re-run Worktrees
	// discovery itself (I/O this goroutine never performs). See Run's
	// initial assignment for the value in effect before the first reload
	// cycle's own (worktree-resolved) scope arrives.
	currentScope session.Scope

	// observerCh receives every sessionctl.ObserverUpdate from
	// sessionctl.Controller.Observe -- subscribed exactly once, at Run
	// startup (see Run), independent of catalogGen/reload cycles: a
	// completed ChatGPT refresh (or any other Observer provider) updates
	// Rows the moment it publishes, even with no reload in flight, and is
	// never dropped merely because a newer Ctrl+L started a fresh reload
	// cycle in the meantime (see the DesignDoc's "Observer generations").
	// Set to nil once the underlying channel closes (ctx ended, or no
	// configured provider implements Observer), removing this case from
	// eventLoop's select rather than busy-looping on a closed channel.
	observerCh <-chan sessionctl.ObserverUpdate

	// transient is the targeted-refresh state for providers that still list a
	// transient (Starting) session -- see transient_unix.go.
	transient transientRefresh

	terminal        overviewLifecycle
	runPromptEditor promptEditorRunner
}

// providerState is one provider's retained catalog entry in
// Runtime.providerSnapshots: the latest known sessions and the latest
// refresh warning, tracked independently so a failed refresh (an
// Observer's error update, or a LoadStream provider error) never
// discards previously-known sessions -- see applyProviderUpdate.
type providerState struct {
	sessions []session.Session
	warning  error

	// observerSnapshotSeen reports whether a successful Observer catalog
	// (ProviderUpdate.Err == nil) has ever been applied for this provider
	// -- i.e. whether Observer, rather than List, now owns this
	// provider's rows (see applyLoadSnapshot/applyObserverUpdate). It
	// only ever transitions false -> true, on a successful Observer
	// publication; an Observer *failure* never sets it, so a persisted-
	// cache bootstrap List result can still seed rows even after an
	// initial background refresh fails (see applyLoadSnapshot's doc
	// comment). Meaningless (and never consulted) for a provider that
	// doesn't implement sessionctl.Observer at all.
	observerSnapshotSeen bool
}

// usageEvent is one sessionctl.UsageUpdate carried over Runtime.usageCh,
// stamped with the reload cycle (gen) that started the fetch it came from.
type usageEvent struct {
	gen      int
	provider session.ProviderID
	usage    session.Usage
	err      error
}

// catalogEvent is one provider's LoadStream arrival carried over
// Runtime.catalogCh, stamped with the reload cycle (gen) that started the
// fetch it came from -- see requestReload and eventLoop's catalogCh case.
// requestReload's background goroutine sends one catalogEvent per provider
// arrival from sessionctl.Controller.LoadStream (ps), which eventLoop
// applies into Runtime.providerSnapshots (see applyProviderUpdate) and
// re-merges -- so a fast provider's rows (Claude, Codex) reach State well
// before a slow one finishes, rather than waiting behind it, and a
// provider that already reported in an earlier cycle keeps showing its
// last-known rows until this cycle's own arrival for it replaces them.
// done marks the last event for this generation (every provider has now
// reported, successfully or not) -- only then does eventLoop clear
// State.CatalogLoading and start a usage refresh (see eventLoop's
// catalogCh case). scope is this cycle's resolved directory Scope (see
// Runtime.currentScope), carried on every event so eventLoop never needs
// to re-run Worktrees discovery itself.
type catalogEvent struct {
	gen   int
	ps    sessionctl.ProviderSnapshot
	scope session.Scope
	done  bool
}

// keyResult is one physically-decoded key read (or read error), carried
// over Run's one-shot key-read channel (see startKeyRead).
type keyResult struct {
	ev  KeyEvent
	err error
}

// startKeyRead performs exactly one blocking key read in its own
// goroutine and sends the result to out, then exits -- Run never has more
// than one such goroutine outstanding at a time while it owns the overview
// (see Run's doc comment on input ownership), so IntentOpen handing the
// real terminal to a provider's Open never races a leftover reader still
// consuming bytes meant for the attached child.
func startKeyRead(reader *bufio.Reader, readKeyFn func(*bufio.Reader) (KeyEvent, error), out chan<- keyResult) {
	go func() {
		ev, err := readKeyFn(reader)
		out <- keyResult{ev: ev, err: err}
	}()
}

// Run starts the terminal event loop: raw mode, an initial catalog reload
// request, then render-and-wait for the next physical key, background
// usage update, or background catalog reload result, until IntentQuit or
// a read error.
//
// Neither a provider's catalog List nor its usage probe is ever on this
// loop's critical path (see requestReload/refreshUsageAsync): the first
// frame renders immediately, before the initial reload's provider fetches
// have even started, let alone completed -- Rows stays whatever it already
// was (empty, for a fresh Runtime) until each provider's own catalogEvent
// arrives on r.catalogCh and eventLoop applies it. A slow or hung provider
// (e.g. ChatGPT's multi-page browser-backed discovery walk) therefore
// never blocks keyboard input, composer editing, help, or quit -- nor does
// it hold back a faster provider's rows (Claude, Codex) from appearing and
// staying actionable in the meantime (see catalogEvent's doc comment).
//
// Input ownership: while Run owns the overview, exactly one key-read
// goroutine is ever outstanding (see startKeyRead) -- it is consumed
// (removing it) before intent handling begins, and no state ever races a
// usage or catalog update against handling that intent, since all three
// are only ever processed from this one select loop. IntentOpen's
// provider Open call (agentview.Runtime.act) therefore always runs with
// zero outstanding Agent View readers on r.Input, so the real terminal is
// safe to hand to the attached child. The next key read is only started
// again after act() returns control to the overview.
func (r *Runtime) Run(ctx context.Context) (runErr error) {
	if r.Input == nil {
		r.Input = os.Stdin
	}
	if r.Output == nil {
		r.Output = os.Stdout
	}
	terminal := &overviewTerminal{input: r.Input, output: r.Output}
	if err := terminal.start(); err != nil {
		return err
	}
	r.terminal = terminal
	defer func() {
		r.terminal = nil
		runErr = errors.Join(runErr, terminal.close())
	}()
	// The last reload cycle's own child context is cancelled on the way
	// out regardless of how Run is returning (IntentQuit, a key-read
	// error, ...), so a catalog fetch still in flight (e.g. ChatGPT still
	// paginating) is torn down with the overview rather than left running
	// to completion for a Snapshot nothing will ever apply.
	defer func() {
		if r.catalogCancel != nil {
			r.catalogCancel()
		}
	}()
	// currentScope's synchronous default (no worktree resolution -- that
	// only matters for ScopeDescendants, and a fresh State starts scoped
	// to ScopeSame) covers the narrow race where an Observer publishes
	// before requestReload's own background goroutine has resolved and
	// carried its first (possibly worktree-resolved) scope; that first
	// catalogEvent immediately supersedes it regardless.
	r.currentScope = session.Scope{CurrentDirectory: r.CWD, Directory: r.State.Scope}
	// Observer providers (e.g. ChatGPT) are subscribed exactly once here,
	// for Run's entire lifetime -- never re-subscribed per reload cycle
	// (see observerCh's doc comment). observeCtx is Run's own child
	// context (not ctx itself, and not any per-reload catalogCancel
	// child), cancelled in this defer so the subscription always ends
	// when Run returns, for any reason (IntentQuit, a key-read error, ...)
	// -- not only when the caller eventually cancels ctx.
	observeCtx, observeCancel := context.WithCancel(ctx)
	defer observeCancel()
	r.observerCh = r.Controller.Observe(observeCtx)
	r.requestReload(ctx)
	r.render()
	reader := bufio.NewReader(r.Input)
	readKeyFn := r.ReadKey
	if readKeyFn == nil {
		readKeyFn = func(reader *bufio.Reader) (KeyEvent, error) { return readTerminalKey(reader, r.Input) }
	}
	return r.eventLoop(ctx, reader, readKeyFn)
}

// eventLoop is Run's select loop, factored out so it can be driven
// directly by a test with a fake reader/readKeyFn and no real terminal --
// unlike Run itself, it performs no raw-mode setup (term.MakeRaw), and
// render's own term.GetSize call already tolerates a non-tty r.Input by
// falling back to a fixed size. See Run's doc comment for the input-
// ownership and usage-never-blocks-render guarantees this implements.
func (r *Runtime) eventLoop(ctx context.Context, reader *bufio.Reader, readKeyFn func(*bufio.Reader) (KeyEvent, error)) error {
	keyCh := make(chan keyResult, 1)
	startKeyRead(reader, readKeyFn, keyCh)
	// Targeted refreshes run under this loop's own context, so none outlives
	// the loop (see transientRefresh).
	loopCtx, stopLoop := context.WithCancel(ctx)
	defer stopLoop()
	for {
		select {
		case kr := <-keyCh:
			if kr.err != nil {
				return kr.err
			}
			intent := r.State.Handle(kr.ev)
			if intent.Kind == IntentQuit {
				return nil
			}
			if err := r.act(ctx, intent); err != nil {
				if errors.Is(err, errOverviewTerminalOwnership) {
					return err
				}
				r.State.Error = "error: " + err.Error()
			} else if intent.Kind != IntentNone {
				// A dispatched intent that succeeded (including a plain
				// refresh) means the user has moved on to something that
				// worked -- don't leave a stale error from an earlier
				// failed action on screen.
				r.State.Error = ""
			}
			r.render()
			startKeyRead(reader, readKeyFn, keyCh)
		case upd := <-r.usageCh:
			// A usage update from an older, superseded reload cycle (e.g.
			// two reloads triggered in quick succession) is dropped: it
			// must never overwrite a newer cycle's already-applied
			// result with stale data.
			if upd.gen == r.usageGen {
				r.State.ApplyUsageUpdate(upd.provider, upd.usage, upd.err)
				r.render()
			}
		case update := <-r.catalogCh:
			// An arrival from an older, superseded reload cycle (e.g. a
			// slow ChatGPT List that lost a race with a second Ctrl+L, or a
			// scope change requested before the previous scope's Load
			// finished) is dropped: only the latest requestReload call may
			// ever update State (see requestReload's doc comment) -- this
			// is the correctness half of that guarantee; requestReload's
			// own context cancellation is the save-work half.
			if update.gen != r.catalogGen {
				break
			}
			r.currentScope = update.scope
			// Every provider heard from so far this cycle already
			// applied its own catalogEvent (see catalogEvent's doc
			// comment) -- applied here regardless of update.done, so a
			// fast provider's rows (Claude, Codex) render as soon as they
			// arrive, without waiting on a slower one still in flight.
			// ps.Provider is only empty for the zero-provider-configured
			// case, which has nothing to apply.
			if update.ps.Provider != "" {
				r.applyLoadSnapshot(update.ps.Provider, update.ps.Sessions, update.ps.Err, update.ps.ListOwnsStatus)
			}
			r.recomputeRows()
			if update.done {
				r.State.CatalogLoading = false
				// Usage is deliberately started only once every provider
				// in this cycle has reported, from the final accepted
				// arrival, rather than once per provider arrival or from
				// requestReload itself: a superseded reload's own usage
				// cycle would just be discarded work for state the
				// catalogGen check above already threw away, and starting
				// it on every partial arrival would refetch usage for
				// providers that already reported earlier in the same
				// cycle.
				r.refreshUsageAsync(ctx)
			}
			r.transient.schedule(r)
			r.render()
		case update, ok := <-r.observerCh:
			// Independent of catalogGen/reload cycles by design (see
			// observerCh's doc comment): a completed provider refresh is
			// authoritative the moment it publishes, whether or not a
			// reload cycle is currently in flight, and is never dropped
			// merely because a newer Ctrl+L started one since this
			// subscription began.
			if !ok {
				r.observerCh = nil
				break
			}
			r.applyObserverUpdate(update.Provider, update.Sessions, update.Err, update.Warning)
			r.recomputeRows()
			r.transient.schedule(r)
			r.render()
		case <-r.transient.tick:
			r.transient.refresh(loopCtx, r)
		case ev := <-r.transient.results:
			if r.transient.apply(r, ev) {
				r.render()
			}
		}
	}
}

// render draws the current State to the terminal at its current size,
// falling back to a fixed size if the terminal size can't be read.
func (r *Runtime) render() {
	width, height, sizeErr := term.GetSize(int(r.Input.Fd()))
	if sizeErr != nil {
		width, height = 80, 24
	}
	fmt.Fprint(r.Output, terminalFrame(r.State.View(width, height)))
}

func terminalFrame(view string) string {
	return "\x1b[2J\x1b[H" + normalizeTerminalNewlines(view)
}

func normalizeTerminalNewlines(value string) string {
	var b strings.Builder
	b.Grow(len(value))
	for i := 0; i < len(value); i++ {
		if value[i] == '\n' && (i == 0 || value[i-1] != '\r') {
			b.WriteByte('\r')
		}
		b.WriteByte(value[i])
	}
	return b.String()
}

// requestReload starts one catalog reload cycle in the background and
// returns immediately -- Run's event loop is never blocked on a
// provider's List, however slow (e.g. ChatGPT's multi-page browser-backed
// discovery walk), since requestReload runs on Run's own critical path
// for startup, every Ctrl+/ or Ctrl+L refresh, and every provider action
// whose sessionctl.Result asks for a reload (including returning from a
// detached session) -- see the DesignDoc's Agent View responsiveness
// guarantee.
//
// Each call:
//   - snapshots the scope inputs (CWD, State.Scope, Worktrees) on the
//     calling goroutine, since State is otherwise only ever touched from
//     Run's own event-loop goroutine -- the (possibly slow) Worktrees
//     discovery call itself then runs in the background along with the
//     provider fetch, not here;
//   - cancels the previous reload cycle's own child context (see
//     catalogCancel), so a superseded ChatGPT List/enumeration stops
//     doing work for a Snapshot that will be discarded -- but never
//     touches ctx itself, so the provider/runtime stays reusable for the
//     next reload (e.g. the ChatGPT runtime's own mutex-serialized List
//     unblocks and re-locks for the new cycle once the old call unwinds);
//   - increments catalogGen and stamps every catalogEvent this cycle sends
//     with it, so eventLoop's catalogCh case can drop a stale cycle's
//     Snapshot if a newer one already applied (mirroring the
//     usageGen/usageCh precedent).
//
// Providers are fetched via sessionctl.Controller.LoadStream, not Load:
// each provider's own ProviderSnapshot is applied into
// Runtime.providerSnapshots (see applyProviderUpdate) and sent as its own
// catalogEvent the moment it arrives, rather than collecting every
// provider into one batch behind the slowest -- see catalogEvent's doc
// comment. This is what lets Claude/Codex rows render (and stay
// actionable) without waiting on a slow or erroring ChatGPT.
//
// Until each provider has reported for this cycle, State.Rows keeps
// rendering whatever providerSnapshots already held for that provider
// (see eventLoop's catalogCh case for where a new arrival actually
// replaces it) -- old rows, and the selection/action target they carry,
// stay visible and usable rather than flashing empty. State.CatalogLoading
// is set so the view can render a small "loading sessions…" indicator
// until the whole cycle (every provider) finishes.
//
// Separately, every provider implementing sessionctl.Refresher (e.g.
// ChatGPT) is asked to Refresh -- deliberately with ctx, this call's own
// long-lived argument, not the childCtx this cycle's LoadStream uses:
// unlike a List call, a background refresh has value beyond this one
// reload cycle (see sessionctl.Refresher's doc comment), so a superseded
// reload must never cancel it. Its eventual result arrives later, and
// independent of any reload generation, through r.observerCh (see
// eventLoop's observerCh case) -- Refresh itself is fire-and-forget here.
func (r *Runtime) requestReload(ctx context.Context) {
	r.State.StartupCWD = r.CWD
	r.transient.attempts = 0
	cwd, dirScope, worktrees := r.CWD, r.State.Scope, r.Worktrees

	if r.catalogCh == nil {
		r.catalogCh = make(chan catalogEvent, 1)
	}
	if r.providerSnapshots == nil {
		r.providerSnapshots = make(map[session.ProviderID]providerState)
	}
	if r.catalogCancel != nil {
		r.catalogCancel()
	}
	childCtx, cancel := context.WithCancel(ctx)
	r.catalogCancel = cancel
	r.catalogGen++
	gen := r.catalogGen
	r.State.CatalogLoading = true

	ch := r.catalogCh
	providerCount := len(r.Controller.Providers)
	go func() {
		scope := session.Scope{CurrentDirectory: cwd, Directory: dirScope}
		if dirScope == session.ScopeDescendants && worktrees != nil {
			scope.WorktreeDirectories = worktrees(childCtx, cwd)
		}
		if providerCount == 0 {
			sendCatalogEvent(ch, childCtx, catalogEvent{gen: gen, scope: scope, done: true})
			return
		}
		remaining := providerCount
		// Always fully drains LoadStream (even once superseded --
		// childCtx.Done() only skips the *send* below, never the loop
		// itself) so a still-running provider's own goroutine can never
		// block forever trying to hand off a result nothing reads.
		for ps := range r.Controller.LoadStream(childCtx) {
			remaining--
			sendCatalogEvent(ch, childCtx, catalogEvent{gen: gen, ps: ps, scope: scope, done: remaining == 0})
		}
	}()

	for _, p := range r.Controller.Providers {
		if refresher, ok := p.(sessionctl.Refresher); ok {
			refresher.Refresh(ctx)
		}
	}
}

// applyLoadSnapshot installs one LoadStream arrival (catalogEvent.ps) into
// providerSnapshots. A List failure (err != nil) always surfaces as this
// provider's warning, leaving its previously-known sessions untouched
// (never discarding rows -- see the DesignDoc's last-known-good provider
// snapshot store): a real Source.List error (misconfiguration, browser
// unavailable, ...) is never hidden, regardless of listOwnsStatus.
//
// A successful List (err == nil) never touches warning when listOwnsStatus
// is false (see sessionctl.ProviderSnapshot.ListOwnsStatus's doc comment)
// -- only a subsequent Observer update (see applyObserverUpdate) is
// entitled to replace or clear a warning for a provider that also
// implements sessionctl.Observer. This is what fixes one real regression:
// without it, a routine Ctrl+L (which still calls List for its now-fast
// cached read) would silently erase an unresolved ChatGPT persistence or
// refresh-failure warning the moment the cache read itself merely
// succeeded.
//
// Whether a successful List is even allowed to replace sessions depends
// on the same listOwnsStatus split, plus observerSnapshotSeen for the
// !listOwnsStatus case:
//
//   - listOwnsStatus == true (an ordinary Source-only provider): List
//     always replaces sessions and clears warning, exactly as before any
//     of this Observer machinery existed.
//   - listOwnsStatus == false, observerSnapshotSeen == false (an
//     Observer-capable provider that hasn't yet had a successful Observer
//     publication -- e.g. right after startup, serving a persisted-cache
//     hydration): List is still allowed to seed sessions. This is the
//     restart-bootstrap path -- persisted rows must appear immediately,
//     before any real refresh has completed.
//   - listOwnsStatus == false, observerSnapshotSeen == true (Observer has
//     already published at least one successful full catalog for this
//     provider): List's own result is now stale/non-authoritative and is
//     IGNORED for rows. This fixes the second real regression: Observer
//     publications are independent of any Agent View reload generation
//     (see the DesignDoc's "Observer generations"), so within one Ctrl+L
//     cycle a slower LoadStream List(B) arrival can be delivered AFTER a
//     faster background refresh has already published a newer Observer
//     catalog C -- catalogGen alone does not protect against this, since
//     both B and C are individually valid, current-generation-or-
//     independent-of-generation events. Once Observer owns a provider's
//     rows, no List result -- however "current" -- may roll them back to
//     an older snapshot.
//
// Only ever called from the eventLoop goroutine. Does not itself update
// State -- see recomputeRows.
func (r *Runtime) applyLoadSnapshot(id session.ProviderID, sessions []session.Session, err error, listOwnsStatus bool) {
	st := r.providerSnapshots[id]
	if err != nil {
		st.warning = err
		r.providerSnapshots[id] = st
		return
	}
	if listOwnsStatus {
		st.sessions = sessions
		st.warning = nil
	} else if !st.observerSnapshotSeen {
		st.sessions = sessions
	}
	// else: Observer already owns this provider's rows -- this List
	// result is ignored for both sessions and warning.
	r.providerSnapshots[id] = st
}

// applyObserverUpdate installs one Observer publication (ObserverUpdate)
// into providerSnapshots. Observer is always authoritative for a
// provider's refresh/durability status -- regardless of what that
// provider's own List last reported (see applyLoadSnapshot) -- since it
// is the one capability specifically designed to report background
// refresh outcomes independent of any particular List call:
//
//   - err != nil: a failed refresh. Sessions are left untouched and
//     warning is set to err. Deliberately does NOT set
//     observerSnapshotSeen: an Observer failure never transfers row
//     authority away from List, so a persisted-cache bootstrap List
//     result can still seed rows even after an initial background
//     refresh fails (e.g. right after a restart whose first refresh
//     times out -- the persisted rows must remain List's to seed).
//   - err == nil: a successful result. sessions fully replaces that
//     provider's rows -- valid and usable regardless of warning -- and
//     warning is set to the given non-fatal warning (nil clears it, a
//     non-nil one surfaces it alongside the now-current sessions; see
//     sessionctl.ProviderUpdate's doc comment on why valid Sessions and a
//     Warning can coexist on one update, unlike Sessions and Err). Also
//     sets observerSnapshotSeen, permanently transferring row authority
//     from List to Observer for this provider (see applyLoadSnapshot) --
//     it only ever goes false -> true, never back.
//
// Only ever called from the eventLoop goroutine. Does not itself update
// State -- see recomputeRows.
func (r *Runtime) applyObserverUpdate(id session.ProviderID, sessions []session.Session, err, warning error) {
	st := r.providerSnapshots[id]
	if err != nil {
		st.warning = err
	} else {
		st.sessions = sessions
		st.warning = warning
		st.observerSnapshotSeen = true
	}
	r.providerSnapshots[id] = st
}

// recomputeRows re-derives State.Rows/Warnings from the full
// providerSnapshots store (every provider's latest known sessions and
// warning, not just whichever provider just reported) and r.currentScope,
// through the same pin/scope/order pipeline Load itself uses
// (sessionctl.Controller.MergeSessions) -- called after every
// applyLoadSnapshot/applyObserverUpdate. Iterates r.Controller.Providers
// (a fixed order) rather than ranging the map directly, so row order is
// deterministic across calls regardless of Go's randomized map iteration.
func (r *Runtime) recomputeRows() {
	var sessions []session.Session
	warnings := make(map[session.ProviderID]error)
	for _, p := range r.Controller.Providers {
		st, ok := r.providerSnapshots[p.ID()]
		if !ok {
			continue
		}
		sessions = append(sessions, st.sessions...)
		if st.warning != nil {
			warnings[p.ID()] = st.warning
		}
	}
	r.State.SetRows(r.Controller.MergeSessions(sessions, r.currentScope))
	r.State.Warnings = warnings
}

// sendCatalogEvent delivers ev on ch unless childCtx has already ended -- a superseded
// reload cycle's own catalogEvent is simply dropped rather than blocking
// on (or stale-writing into) a channel eventLoop may no longer be
// draining for this generation.
func sendCatalogEvent(ch chan<- catalogEvent, childCtx context.Context, ev catalogEvent) {
	select {
	case ch <- ev:
	case <-childCtx.Done():
	}
}

// refreshUsageAsync starts one usage-refresh cycle in the background via
// sessionctl.Controller.UsageStream, forwarding each provider's result to
// r.usageCh as it arrives, tagged with this cycle's generation (see
// usageEvent). It returns immediately -- callers never wait on it -- and
// uses ctx as given (the long-lived, app-scoped context Run was called
// with, not a context bound to this call), so the fetch keeps running to
// completion (or ctx cancellation, e.g. agentsctl exiting) even though the
// caller has already returned.
func (r *Runtime) refreshUsageAsync(ctx context.Context) {
	if r.usageCh == nil {
		r.usageCh = make(chan usageEvent, 8)
	}
	r.usageGen++
	gen := r.usageGen
	ch := r.usageCh
	go func() {
		for upd := range r.Controller.UsageStream(ctx) {
			ch <- usageEvent{gen: gen, provider: upd.Provider, usage: upd.Usage, err: upd.Err}
		}
	}()
}

// act carries out intent via the Controller and applies its Result to
// State: IntentNone/IntentRefresh short-circuit (a plain Ctrl+/ or Ctrl+L
// refresh is exactly "request a reload, no operation"), otherwise every
// operation's sessionctl.Result decides Reload vs. local Patch
// application -- Run's loop above never hardcodes a per-intent refresh
// policy (see sessionctl.Result's doc comment). act itself never blocks
// on a reload it requests (see requestReload/applyResult): it returns as
// soon as the operation's own provider call (and, for Reload results, the
// background fetch it starts) is under way.
func (r *Runtime) act(ctx context.Context, x Intent) error {
	if x.Kind == IntentNone {
		return nil
	}
	if x.Kind == IntentRefresh {
		r.requestReload(ctx)
		return nil
	}
	switch x.Kind {
	case IntentDispatch:
		_, result, err := r.Controller.Dispatch(ctx, x.Provider, x.Prompt, r.State.ComposerCWD())
		if err != nil {
			return err
		}
		r.State.Composer.Clear()
		r.applyResult(ctx, result)
	case IntentOpenPromptEditor:
		return r.editPrompt(ctx)
	case IntentOpen:
		row, ok := r.findRow(x.Key)
		if !ok {
			return fmt.Errorf("session %s is no longer in the catalog", x.Key)
		}
		result, err := r.Controller.Open(ctx, row, r.Input, r.Output)
		if err != nil {
			return err
		}
		// A successful Open -- regardless of how it ended (an explicit
		// detach, or the attached client/session exiting on its own) --
		// marks the session as last-attached for title styling. Only an
		// Open that returned an error skips this.
		r.State.MarkAttached(x.Key)
		r.applyResult(ctx, result)
	case IntentStop:
		result, err := r.Controller.Stop(ctx, x.Key)
		if err != nil {
			return err
		}
		r.applyResult(ctx, result)
	case IntentArchive:
		result, err := r.Controller.Archive(ctx, x.Key)
		if err != nil {
			return err
		}
		r.applyResult(ctx, result)
	case IntentRename:
		result, err := r.Controller.Rename(ctx, x.Key, x.Name)
		if err != nil {
			return err
		}
		r.State.Rename.cancel()
		r.applyResult(ctx, result)
	case IntentPin:
		result, err := r.Controller.TogglePin(x.Key)
		if err != nil {
			return err
		}
		r.applyResult(ctx, result)
	}
	return nil
}

// applyResult reflects one operation's sessionctl.Result: Reload starts a
// new background reload cycle (see requestReload) rather than re-running
// the catalog load in place, otherwise a non-nil Patch is applied locally
// (see State.ApplyPatch) with no provider round-trip.
func (r *Runtime) applyResult(ctx context.Context, result sessionctl.Result) {
	if result.Reload {
		r.requestReload(ctx)
		return
	}
	if result.Patch != nil {
		r.State.ApplyPatch(*result.Patch)
	}
}

func (r *Runtime) findRow(key session.Key) (session.Session, bool) {
	for _, row := range r.State.Rows {
		if row.Key == key {
			return row, true
		}
	}
	return session.Session{}, false
}
