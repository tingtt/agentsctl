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
}

// Run starts the terminal event loop: raw mode, an initial catalog load,
// then read-decode-handle-act-render until IntentQuit or a read error.
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
	reader := bufio.NewReader(r.Input)
	readKeyFn := r.ReadKey
	if readKeyFn == nil {
		readKeyFn = func(reader *bufio.Reader) (KeyEvent, error) { return readTerminalKey(reader, r.Input) }
	}
	for {
		width, height, sizeErr := term.GetSize(int(r.Input.Fd()))
		if sizeErr != nil {
			width, height = 80, 24
		}
		fmt.Fprint(r.Output, terminalFrame(r.State.View(width, height)))
		ev, err := readKeyFn(reader)
		if err != nil {
			return err
		}
		intent := r.State.Handle(ev)
		if intent.Kind == IntentQuit {
			return nil
		}
		if err := r.act(ctx, intent); err != nil {
			r.State.Error = "error: " + err.Error()
		} else if intent.Kind != IntentNone {
			// A dispatched intent that succeeded (including a plain
			// refresh) means the user has moved on to something that
			// worked -- don't leave a stale error from an earlier failed
			// action on screen.
			r.State.Error = ""
		}
	}
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

func (r *Runtime) reload(ctx context.Context) {
	scope := session.Scope{CurrentDirectory: r.CWD, Directory: r.State.Scope}
	if r.State.Scope == session.ScopeDescendants && r.Worktrees != nil {
		scope.WorktreeDirectories = r.Worktrees(ctx, r.CWD)
	}
	snap := r.Controller.Load(ctx, scope)
	r.State.SetRows(snap.Sessions)
	r.State.Warnings = snap.Warnings
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
		_, result, err := r.Controller.Dispatch(ctx, x.Provider, x.Prompt, r.CWD)
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
