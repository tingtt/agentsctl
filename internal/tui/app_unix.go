//go:build darwin || linux

package tui

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/tingtt/agentsctl/internal/provider/codex"
	attachpty "github.com/tingtt/agentsctl/internal/pty"
	"github.com/tingtt/agentsctl/internal/session"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

type App struct {
	Catalog    session.Catalog
	Model      Model
	Input      *os.File
	Output     io.Writer
	CWD        string
	ClaudePath string
	Socket     string
	ReadInput  func(*bufio.Reader) (string, error)
	// AttachFunc, when set, overrides attach()'s real Claude/Codex PTY
	// dispatch — a test seam for exercising act()'s ActionAttach path
	// (specifically, when it does and doesn't call Model.MarkAttached)
	// without a real attach client process or Codex supervisor socket.
	// Production leaves this nil, in which case attach() dispatches to
	// the real attachpty.AttachClaude/AttachCodex.
	AttachFunc func(ctx context.Context, p session.Provider, row session.Session) error
}

func (a *App) Run(ctx context.Context) error {
	if a.Input == nil {
		a.Input = os.Stdin
	}
	if a.Output == nil {
		a.Output = os.Stdout
	}
	old, err := term.MakeRaw(int(a.Input.Fd()))
	if err != nil {
		return err
	}
	defer term.Restore(int(a.Input.Fd()), old)
	beginTerminal(a.Output)
	defer endTerminal(a.Output)
	a.refresh(ctx)
	reader := bufio.NewReader(a.Input)
	readInput := a.ReadInput
	if readInput == nil {
		readInput = func(reader *bufio.Reader) (string, error) { return readTerminalKey(reader, a.Input) }
	}
	for {
		width, height, sizeErr := term.GetSize(int(a.Input.Fd()))
		if sizeErr != nil {
			width, height = 80, 24
		}
		fmt.Fprint(a.Output, terminalFrame(a.Model.View(width, height)))
		key, err := readInput(reader)
		if err != nil {
			return err
		}
		action := a.Model.Update(key)
		if action.Kind == ActionQuit {
			return nil
		}
		if err := a.act(ctx, action); err != nil {
			a.Model.Error = "error: " + err.Error()
		} else if action.Kind != ActionNone {
			// A dispatched action that succeeded (including a plain
			// Ctrl+L/Ctrl+G refresh) means the user has moved on to
			// something that worked — don't leave a stale error from an
			// earlier failed action on screen.
			a.Model.Error = ""
		}
		// Pin/unpin never touches provider (remote) state, so — unlike
		// every other action — it must not trigger a full catalog
		// refresh: that refresh re-runs Available()+List() for every
		// provider, which is what made pin/unpin feel like a ~0.5s
		// operation even though the local state-file write it actually
		// depends on takes low-single-digit milliseconds. act() already
		// applies the toggled Pinned state (and re-sorts/reselects)
		// directly on the Model's existing rows via Model.ApplyPin.
		if action.Kind != ActionNone && action.Kind != ActionPin {
			a.refresh(ctx)
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

func (a *App) refresh(ctx context.Context) {
	snap := a.Catalog.Load(ctx, session.Scope{CurrentDirectory: a.CWD, Directory: a.Model.Scope})
	a.Model.SetRows(snap.Sessions)
	a.Model.Warnings = snap.Warnings
}
func (a *App) act(ctx context.Context, x Action) error {
	if x.Kind == ActionNone || x.Kind == ActionRefresh {
		return nil
	}
	p, err := a.Catalog.Provider(x.Provider)
	if err != nil {
		return err
	}
	switch x.Kind {
	case ActionDispatch:
		if err := p.Available(); err != nil {
			return err
		}
		if _, err := p.Dispatch(ctx, x.Prompt, a.CWD); err != nil {
			return err
		}
		a.Model.Prompt = ""
		a.Model.PromptCursor = 0
	case ActionAttach:
		if x.Session == nil || !x.Session.Capabilities.Attach {
			return capabilityError(x.Session, "attach")
		}
		if err := a.attach(ctx, p, *x.Session); err != nil {
			return err
		}
		// A successful attach — regardless of how it ended (an explicit
		// Ctrl+] detach, or the attached client/session exiting on its
		// own) — marks the session as last-attached for title styling.
		// Only an attach that returned an error (invalid provider,
		// PrepareAttach failure, client startup failure, ...) skips this.
		a.Model.MarkAttached(x.Session.Key)
		return nil
	case ActionStop:
		if x.Session == nil || !x.Session.Capabilities.Stop {
			return capabilityError(x.Session, "stop")
		}
		return p.Stop(ctx, x.Session.Key)
	case ActionArchive:
		if x.Session == nil || !x.Session.Capabilities.Archive {
			return capabilityError(x.Session, "archive")
		}
		return p.Archive(ctx, x.Session.Key)
	case ActionRename:
		if x.SessionKey == nil {
			return errors.New("no session selected")
		}
		if strings.TrimSpace(x.Name) == "" {
			return errors.New("name must not be empty")
		}
		if err := p.Rename(ctx, *x.SessionKey, x.Name); err != nil {
			return err
		}
		a.Model.clearRename()
	case ActionPin:
		if x.Session == nil {
			return errors.New("no session selected")
		}
		// Persist synchronously first: on error, the Model's rows are left
		// untouched (still consistent with the actual persisted state), so
		// there is nothing to roll back. Only apply the local row update —
		// no provider refresh — once persistence has actually succeeded.
		pinned, err := a.Catalog.TogglePin(x.Session.Key)
		if err != nil {
			return err
		}
		a.Model.ApplyPin(x.Session.Key, pinned)
	}
	return nil
}
func (a *App) attach(ctx context.Context, p session.Provider, row session.Session) error {
	if a.AttachFunc != nil {
		return a.AttachFunc(ctx, p, row)
	}
	switch row.Key.Provider {
	case session.ProviderClaude:
		return attachpty.AttachClaude(ctx, a.ClaudePath, row.Key.ID, a.Input, a.Output, 2*time.Second)
	case session.ProviderCodex:
		cp, ok := p.(*codex.Provider)
		if !ok {
			return errors.New("invalid Codex attach strategy")
		}
		run, err := cp.PrepareAttach(ctx, row)
		if err != nil {
			return err
		}
		return attachpty.AttachCodex(ctx, a.Socket, run, a.Input, a.Output)
	default:
		return errors.New("unknown provider")
	}
}
func capabilityError(s *session.Session, action string) error {
	if s == nil {
		return errors.New("no session selected")
	}
	reason := s.Capabilities.Reason
	if reason == "" {
		reason = "action is unavailable in the current state"
	}
	return fmt.Errorf("cannot %s: %s", action, reason)
}
func readKey(r *bufio.Reader) (string, error) {
	return readKeyWithEscapeWait(r, nil)
}

func readTerminalKey(r *bufio.Reader, input *os.File) (string, error) {
	return readKeyWithEscapeWait(r, func() (bool, error) {
		fds := []unix.PollFd{{Fd: int32(input.Fd()), Events: unix.POLLIN}}
		n, err := unix.Poll(fds, 30)
		if errors.Is(err, syscall.EINTR) {
			return false, nil
		}
		return n > 0, err
	})
}

func readKeyWithEscapeWait(r *bufio.Reader, wait func() (bool, error)) (string, error) {
	b, err := r.ReadByte()
	if err != nil {
		return "", err
	}
	switch b {
	case 0x1b:
		if r.Buffered() == 0 {
			if wait == nil {
				return "quit", nil
			}
			ready, err := wait()
			if err != nil {
				return "", err
			}
			if !ready {
				return "quit", nil
			}
		}
		next, err := r.ReadByte()
		if err != nil {
			return "", err
		}
		if next == 'O' {
			// SS3-form keys (ESC O <letter>), not just the CSI form (ESC
			// [ <letter>) handled below. This is not a hypothetical: the
			// terminfo entry for TERM=xterm-256color (a common default,
			// including on macOS terminals) declares kcub1/kcuf1/kcuu1/
			// kcud1 (Left/Right/Up/Down) as \EOD/\EOC/\EOA/\EOB — the
			// same SS3 family already handled here for khome/kend
			// (\EOH/\EOF). Terminals that send arrows this way (e.g.
			// when DECCKM/application-cursor-key mode is active) would
			// otherwise have their Left/Right/Up/Down silently dropped.
			final, err := r.ReadByte()
			if err != nil {
				return "", err
			}
			switch final {
			case 'H':
				return "home", nil
			case 'F':
				return "end", nil
			case 'A':
				return "up", nil
			case 'B':
				return "down", nil
			case 'C':
				return "right", nil
			case 'D':
				return "left", nil
			}
			return "", nil
		}
		if next != '[' {
			return "", nil
		}
		sequence, err := readCSI(r)
		if err != nil {
			return "", err
		}
		switch sequence {
		case "Z":
			return "shift+tab", nil
		case "A":
			return "up", nil
		case "B":
			return "down", nil
		case "C":
			return "right", nil
		case "D":
			return "left", nil
		case "H", "1~", "7~":
			return "home", nil
		case "F", "4~", "8~":
			return "end", nil
		case "3~":
			return "delete", nil
		}
		return "", nil
	case '\r', '\n':
		return "enter", nil
	case 0x7f, 0x08:
		return "backspace", nil
	case 0x0f:
		return "open", nil
	case 0x14:
		return "pin", nil
	case 0x07:
		// Ctrl+G (C0 code 0x07, BEL) cycles the session-list directory
		// scope: cwd -> cwd/** -> all -> cwd. Ctrl+A previously toggled a
		// two-state version of this and is no longer bound to anything.
		return "scope-cycle", nil
	case 0x18:
		return "stop-or-archive", nil
	case 0x12:
		return "rename", nil
	case 0x13:
		return "stash", nil
	case 0x0c:
		return "refresh", nil
	case 0x1f:
		// Ctrl+/ (and, on terminals that conflate the two physical keys,
		// Ctrl+_) universally arrives as the C0 code 0x1F (US, Unit
		// Separator) rather than the naively-computed '/' & 0x1f = 0x0F —
		// confirmed against macOS Terminal.app and iTerm2, both xterm-
		// compatible. This is the same convention readline's C-_ (undo)
		// and tmux's default C-/ binding rely on.
		return "depth-cycle", nil
	}
	if b < 0x20 {
		return "", nil
	}
	if b >= 0x80 {
		if err := r.UnreadByte(); err != nil {
			return "", err
		}
		rn, _, err := r.ReadRune()
		if err != nil {
			return "", err
		}
		return string(rn), nil
	}
	return string([]byte{b}), nil
}

func readCSI(r *bufio.Reader) (string, error) {
	var sequence []byte
	for len(sequence) < 64 {
		b, err := r.ReadByte()
		if err != nil {
			return "", err
		}
		sequence = append(sequence, b)
		if b >= 0x40 && b <= 0x7e {
			return string(sequence), nil
		}
	}
	return "", errors.New("terminal escape sequence is too long")
}
