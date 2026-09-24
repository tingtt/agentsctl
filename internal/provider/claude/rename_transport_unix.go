//go:build darwin || linux

package claude

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
	"unicode"
)

// sendClaudeRename starts Claude's native, in-place session rename and
// returns as soon as the `/rename <name>` command has been durably handed
// to a live, transient `claude attach <id>` client -- no real terminal is
// involved, unlike Open. It deliberately does not wait for the rename to
// be confirmed or for the client to exit: the caller (Provider.Rename) is
// expected to start polling `claude agents --json --all` for confirmation
// immediately, and to call the returned cleanup func concurrently with
// that polling rather than after it -- see cleanup's own doc comment for
// why ending the client is a separate, unawaited-by-rename-correctness
// step.
//
// The command is written only after ready returns nil, never after a fixed
// "settle" delay. ready is the caller's signal that the session's worker
// REPL is live (Provider.waitSessionLive): attaching to a stopped session
// makes Claude respawn its worker, and input that reaches the worker
// before its REPL mounts goes to Claude's early-input capture, which
// buffers CR as a newline instead of submitting (issue #75, reproduced
// with claude 2.1.281). Once ready has passed, writing immediately is
// reliable: startClaudeAttachRaw puts the pty's slave into raw mode before
// the child process even starts, so a write landing before the client has
// gotten around to reading is safely queued by the kernel rather than
// lost. Real-CLI measurement (disposable sessions, working mid-tool-call,
// completed, and stopped) had `claude agents --json --all` reflect the new
// name typically within 300-350ms of the write for an already-live
// session, with no dropped input and no interrupted background execution.
// An earlier design used fixed 1.5s/1.2s waits before/after writing, which
// added ~2.7s to every rename and still would not have covered a slow
// worker respawn.
//
// If ready fails, the client has already been started, so the returned
// cleanup is non-nil alongside the error and nothing has been written.
//
// `claude --bg --resume <id> --name <name>` was not used for this: that
// flag combination forks a new session rather than mutating the original
// (see the doc comment on Provider.Rename).
func sendClaudeRename(ctx context.Context, path, id, name string, ready func(context.Context) error) (cleanup func(context.Context, time.Duration) error, err error) {
	if path == "" {
		path = "claude"
	}
	if err := validateRenameName(name); err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, path, "attach", id)
	child, err := startClaudeAttachRaw(cmd)
	if err != nil {
		return nil, err
	}
	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()
	// This headless caller has no real terminal to forward output to, and
	// an unread pty buffer can make the child block on its own writes (the
	// same reason Open always runs an output copy loop concurrently) -- so
	// the client's output is drained for its entire transient lifetime,
	// starting immediately, and thrown away unread. The goroutine exits on
	// its own once child is closed (by cleanup) and every Read on it
	// starts failing.
	go drainUntilClosed(child)
	cleanup = func(cleanupCtx context.Context, timeout time.Duration) error {
		defer child.Close()
		return detachClaudeClient(cleanupCtx, cmd, child, wait, timeout)
	}
	if err := ready(ctx); err != nil {
		return cleanup, err
	}
	// The full remaining line (including internal spaces) is the argument:
	// verified against the installed CLI with names containing spaces and
	// Japanese text, neither of which needed quoting. '\r' is the key the
	// live composer treats as Enter when no keyboard protocol has been
	// negotiated (this headless client answers none of Claude's terminal
	// queries), and it submits only because ready has already passed --
	// before that, the same byte is buffered as a newline (see above). '\n'
	// does not submit even in a live composer: it stays in the input.
	// validateRenameName above has already rejected any '\r'/'\n'/other
	// control byte inside name itself, so this is the only place in the
	// constructed input that a command separator can appear.
	if _, err := child.Write([]byte("/rename " + name + "\r")); err != nil {
		_ = child.Close()
		return nil, fmt.Errorf("send /rename to claude attach client: %w", err)
	}
	// Whether the client was already dead when the write above landed (a
	// write to a still-open pty master can succeed even with no live
	// slave-side reader) is deliberately not checked here with a bounded
	// wait: cmd.Wait() delivering to wait is a goroutine-scheduling race
	// that a short timeout can't resolve without either being too short to
	// help or adding a fixed delay to every successful call. Instead this
	// failure mode is left to the caller's own native-catalog confirmation
	// (Provider.confirmRenamed) to catch -- a client that never processed
	// the command leaves no trace in `claude agents --json --all`, and that
	// check is authoritative for rename success/failure anyway.
	return cleanup, nil
}

// drainUntilClosed reads and discards f's output until a Read fails (which
// happens once f is closed), for callers with no real terminal to forward
// output to but that still need the pty's output buffer kept empty so its
// writer never blocks.
func drainUntilClosed(f *os.File) {
	buf := make([]byte, 4096)
	for {
		if _, err := f.Read(buf); err != nil {
			return
		}
	}
}

// validateRenameName is the last line of defense against PTY command
// injection via a session name: it runs immediately before sendClaudeRename
// assembles the literal bytes written to the child's pty, independently of
// whatever validation the caller (Provider.Rename) already did. Rejecting
// every Unicode control character -- not just '\r'/'\n' -- blocks '\r'
// (submits the /rename command early and turns the remainder of name into
// a brand-new prompt the live agent will actually execute -- reproduced
// against the installed CLI: a name containing "evil\rhi there" renamed the
// session to "evil" and then had the agent answer "hi there" as a real
// chat turn), ESC (could start an escape sequence a well-behaved client
// might interpret as a key), and every other C0/C1 control byte, while
// leaving ordinary Unicode -- including Japanese text and plain spaces,
// both verified against the installed CLI -- untouched.
func validateRenameName(name string) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("name is required")
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return fmt.Errorf("name must not contain control characters")
		}
	}
	return nil
}
