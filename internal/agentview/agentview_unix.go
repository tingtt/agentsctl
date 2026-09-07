//go:build darwin || linux

package agentview

import (
	"bufio"
	"context"
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
	// results from reload's background usage fetch, each tagged with the
	// reload cycle that started it (usageGen) -- see refreshUsageAsync.
	// Lazily initialized (nil is a valid zero value): a Runtime built
	// directly for a test that only calls reload/act, with no event loop
	// draining it, never blocks on it either, since UsageStream only ever
	// starts a goroutine for a provider that actually implements
	// UsageSource (see sessionctl.Controller.UsageStream).
	usageCh  chan usageEvent
	usageGen int
}

// usageEvent is one sessionctl.UsageUpdate carried over Runtime.usageCh,
// stamped with the reload cycle (gen) that started the fetch it came from.
type usageEvent struct {
	gen      int
	provider session.ProviderID
	usage    session.Usage
	err      error
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

// Run starts the terminal event loop: raw mode, an initial catalog load,
// then render-and-wait for either the next physical key or a background
// usage update, until IntentQuit or a read error.
//
// Usage is never on this loop's critical path (see reload/
// refreshUsageAsync): the first frame renders as soon as the catalog
// loads, without waiting for any provider's usage, and a usage update
// arriving later triggers its own redraw without consuming or requiring a
// key press.
//
// Input ownership: while Run owns the overview, exactly one key-read
// goroutine is ever outstanding (see startKeyRead) -- it is consumed
// (removing it) before intent handling begins, and no state ever races a
// usage update against handling that intent, since both are only ever
// processed from this one select loop. IntentOpen's provider Open call
// (agentview.Runtime.act) therefore always runs with zero outstanding
// Agent View readers on r.Input, so the real terminal is safe to hand to
// the attached child. The next key read is only started again after
// act() returns control to the overview.
func (r *Runtime) Run(ctx context.Context) error {
	if r.Input == nil {
		r.Input = os.Stdin
	}
	if r.Output == nil {
		r.Output = os.Stdout
	}
	old, err := term.MakeRaw(int(r.Input.Fd()))
	if err != nil {
		return err
	}
	defer term.Restore(int(r.Input.Fd()), old)
	beginTerminal(r.Output)
	defer endTerminal(r.Output)
	r.reload(ctx)
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

func beginTerminal(w io.Writer) { _, _ = io.WriteString(w, "\x1b[?1049h\x1b[?25l") }
func endTerminal(w io.Writer)   { _, _ = io.WriteString(w, "\x1b[0m\x1b[?25h\x1b[?1049l") }

// reload re-fetches the session catalog synchronously -- render-ready the
// moment it returns -- then kicks off a usage refresh in the background
// (see refreshUsageAsync). Usage is deliberately NOT fetched here: a slow
// or hung provider (a Claude usage probe waiting out its own timeout, for
// instance) must never add its latency to catalog loading, since reload
// runs on Run's own critical path for startup, every Ctrl+G/Ctrl+L
// refresh, and every provider action whose sessionctl.Result asks for a
// reload (including returning from a detached session) -- see the
// DesignDoc's Agent View responsiveness guarantee.
//
// State.Usage is never cleared here either: whatever was already known
// keeps rendering until refreshUsageAsync's own updates replace it,
// provider by provider, so a reload never flickers the usage line blank.
func (r *Runtime) reload(ctx context.Context) {
	r.State.StartupCWD = r.CWD
	scope := session.Scope{CurrentDirectory: r.CWD, Directory: r.State.Scope}
	if r.State.Scope == session.ScopeDescendants && r.Worktrees != nil {
		scope.WorktreeDirectories = r.Worktrees(ctx, r.CWD)
	}
	snap := r.Controller.Load(ctx, scope)
	r.State.SetRows(snap.Sessions)
	r.State.Warnings = snap.Warnings
	r.refreshUsageAsync(ctx)
}

// refreshUsageAsync starts one usage-refresh cycle in the background via
// sessionctl.Controller.UsageStream, forwarding each provider's result to
// r.usageCh as it arrives, tagged with this cycle's generation (see
// usageEvent). It returns immediately -- callers (reload) never wait on
// it -- and uses ctx as given (the long-lived, app-scoped context Run was
// called with, not a context bound to this call), so the fetch keeps
// running to completion (or ctx cancellation, e.g. agentsctl exiting) even
// though reload has already returned.
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
// State: IntentNone/IntentRefresh short-circuit (a plain Ctrl+G/Ctrl+L
// refresh is exactly "reload, no operation"), otherwise every operation's
// sessionctl.Result decides Reload vs. local Patch application -- Run's
// loop above never hardcodes a per-intent refresh policy (see
// sessionctl.Result's doc comment).
func (r *Runtime) act(ctx context.Context, x Intent) error {
	if x.Kind == IntentNone {
		return nil
	}
	if x.Kind == IntentRefresh {
		r.reload(ctx)
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

// applyResult reflects one operation's sessionctl.Result: Reload re-runs
// the full catalog load, otherwise a non-nil Patch is applied locally
// (see State.ApplyPatch) with no provider round-trip.
func (r *Runtime) applyResult(ctx context.Context, result sessionctl.Result) {
	if result.Reload {
		r.reload(ctx)
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
