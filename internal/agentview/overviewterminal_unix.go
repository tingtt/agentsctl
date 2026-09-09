//go:build darwin || linux

package agentview

import (
	"errors"
	"io"
	"os"

	"golang.org/x/term"
)

// overviewTerminal owns the terminal state used by Agent View's overview.
// It preserves the state that existed before Agent View started so ownership
// can be temporarily handed to a foreground child and later reacquired.
type overviewTerminal struct {
	input    *os.File
	output   io.Writer
	original *term.State
	active   bool
}

func (t *overviewTerminal) start() error {
	if t.active {
		return nil
	}
	previous, err := term.MakeRaw(int(t.input.Fd()))
	if err != nil {
		return err
	}
	if t.original == nil {
		t.original = previous
	}
	if err := beginTerminal(t.output); err != nil {
		return errors.Join(err, term.Restore(int(t.input.Fd()), previous))
	}
	t.active = true
	return nil
}

// suspend releases every terminal mode owned by the overview. A successful
// return means a foreground child can safely inherit the terminal.
func (t *overviewTerminal) suspend() error {
	if !t.active {
		return nil
	}
	terminalErr := endTerminal(t.output)
	restoreErr := term.Restore(int(t.input.Fd()), t.original)
	if restoreErr == nil {
		t.active = false
	}
	return errors.Join(terminalErr, restoreErr)
}

// resume reacquires raw mode and the overview-specific terminal modes after a
// foreground child exits.
func (t *overviewTerminal) resume() error {
	return t.start()
}

// close restores the terminal state from before Agent View started. It is safe
// to call after either an active overview or a failed handoff.
func (t *overviewTerminal) close() error {
	if t.original == nil {
		return nil
	}
	var terminalErr error
	if t.active {
		terminalErr = endTerminal(t.output)
	}
	restoreErr := term.Restore(int(t.input.Fd()), t.original)
	if restoreErr == nil {
		t.active = false
	}
	return errors.Join(terminalErr, restoreErr)
}

func beginTerminal(w io.Writer) error {
	_, err := io.WriteString(w, "\x1b[?1049h\x1b[?25l")
	return err
}

func endTerminal(w io.Writer) error {
	_, err := io.WriteString(w, "\x1b[0m\x1b[?25h\x1b[?1049l")
	return err
}
