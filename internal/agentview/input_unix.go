//go:build darwin || linux

// Package agentview is the Agent View TUI: the terminal loop, mutable UI
// state, and rendering for the unified session list and composer. It
// depends only on internal/session and internal/sessionctl -- never on a
// concrete provider package (internal/provider/claude,
// internal/provider/codex), internal/supervisor, or internal/terminal --
// so a provider capability change, a supervisor implementation detail, or
// a new provider never requires touching this package (see the
// DesignDoc's "TUI must not know provider implementation details").
package agentview

import (
	"bufio"
	"errors"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// Key identifies one physically-decoded input: a control key, a cursor/
// editing key, or a printable rune. It carries no UX meaning -- what Esc
// or Ctrl+X *does* depends on the current State (see update.go), not on
// how the terminal byte stream is decoded. This is the boundary the
// DesignDoc's "Separate terminal protocol decoding from UI semantics"
// requires: decodeKey below only ever turns bytes into one of these
// physical identities.
type Key int

const (
	KeyRune Key = iota
	KeyEnter
	// KeyNewline is Option+Enter or Shift+Enter -- terminals encode a
	// request to insert a literal newline into the composer rather than
	// submit, distinctly from plain Enter (see decodeKey).
	KeyNewline
	KeyBackspace
	KeyDelete
	KeyHome
	KeyEnd
	KeyLeft
	KeyRight
	KeyUp
	KeyDown
	KeyShiftTab
	KeyEsc
	// KeyCtrlG, KeyCtrlX, KeyCtrlR, KeyCtrlT, KeyCtrlS, KeyCtrlL, KeyCtrlO,
	// and KeyCtrlSlash are the fixed physical shortcuts this build
	// recognizes. What each means is entirely up to State.Handle.
	KeyCtrlG
	KeyCtrlX
	KeyCtrlR
	KeyCtrlT
	KeyCtrlS
	KeyCtrlL
	KeyCtrlO
	KeyCtrlSlash
	// KeyUnknown is a recognized-but-unbound escape sequence or control
	// byte: physically decoded, but carrying no assigned meaning.
	KeyUnknown
)

// KeyEvent is one physically-decoded terminal input.
type KeyEvent struct {
	Key  Key
	Rune rune // valid only when Key == KeyRune
}

// readKey decodes one KeyEvent from r, blocking for at least one byte.
func readKey(r *bufio.Reader) (KeyEvent, error) {
	return readKeyWithEscapeWait(r, nil)
}

// readTerminalKey is readKey with a real poll-based wait for a bare ESC's
// possible following bytes, distinguishing a standalone Esc press from the
// start of a multi-byte escape sequence racing this read.
func readTerminalKey(r *bufio.Reader, input *os.File) (KeyEvent, error) {
	return readKeyWithEscapeWait(r, func() (bool, error) {
		fds := []unix.PollFd{{Fd: int32(input.Fd()), Events: unix.POLLIN}}
		n, err := unix.Poll(fds, 30)
		if errors.Is(err, syscall.EINTR) {
			return false, nil
		}
		return n > 0, err
	})
}

// readKeyWithEscapeWait is the single physical-decoding entry point:
// terminal bytes in, KeyEvent out, nothing else. wait (nil in tests that
// don't care about the standalone-Esc race) reports whether more input is
// ready without blocking indefinitely.
func readKeyWithEscapeWait(r *bufio.Reader, wait func() (bool, error)) (KeyEvent, error) {
	b, err := r.ReadByte()
	if err != nil {
		return KeyEvent{}, err
	}
	switch b {
	case 0x1b:
		if r.Buffered() == 0 {
			if wait == nil {
				return KeyEvent{Key: KeyEsc}, nil
			}
			ready, err := wait()
			if err != nil {
				return KeyEvent{}, err
			}
			if !ready {
				return KeyEvent{Key: KeyEsc}, nil
			}
		}
		next, err := r.ReadByte()
		if err != nil {
			return KeyEvent{}, err
		}
		if next == '\r' || next == '\n' {
			// Option+Enter: the Option key follows the classic "meta sends
			// escape" convention, so it arrives as ESC followed by
			// whatever byte that terminal sends for plain Enter.
			return KeyEvent{Key: KeyNewline}, nil
		}
		if next == 'O' {
			// SS3-form keys (ESC O <letter>), not just the CSI form. The
			// terminfo entry for TERM=xterm-256color declares
			// kcub1/kcuf1/kcuu1/kcud1 this way, e.g. under
			// DECCKM/application-cursor-key mode.
			final, err := r.ReadByte()
			if err != nil {
				return KeyEvent{}, err
			}
			switch final {
			case 'H':
				return KeyEvent{Key: KeyHome}, nil
			case 'F':
				return KeyEvent{Key: KeyEnd}, nil
			case 'A':
				return KeyEvent{Key: KeyUp}, nil
			case 'B':
				return KeyEvent{Key: KeyDown}, nil
			case 'C':
				return KeyEvent{Key: KeyRight}, nil
			case 'D':
				return KeyEvent{Key: KeyLeft}, nil
			}
			return KeyEvent{Key: KeyUnknown}, nil
		}
		if next != '[' {
			return KeyEvent{Key: KeyUnknown}, nil
		}
		sequence, err := readCSI(r)
		if err != nil {
			return KeyEvent{}, err
		}
		switch sequence {
		case "Z":
			return KeyEvent{Key: KeyShiftTab}, nil
		case "A":
			return KeyEvent{Key: KeyUp}, nil
		case "B":
			return KeyEvent{Key: KeyDown}, nil
		case "C":
			return KeyEvent{Key: KeyRight}, nil
		case "D":
			return KeyEvent{Key: KeyLeft}, nil
		case "H", "1~", "7~":
			return KeyEvent{Key: KeyHome}, nil
		case "F", "4~", "8~":
			return KeyEvent{Key: KeyEnd}, nil
		case "3~":
			return KeyEvent{Key: KeyDelete}, nil
		}
		return KeyEvent{Key: KeyUnknown}, nil
	case '\r':
		return KeyEvent{Key: KeyEnter}, nil
	case '\n':
		// Shift+Enter: plain Enter sends CR, Shift+Enter sends a bare LF
		// instead -- the same CR-vs-LF distinction readline/fish/tmux rely
		// on for this key combination.
		return KeyEvent{Key: KeyNewline}, nil
	case 0x7f, 0x08:
		return KeyEvent{Key: KeyBackspace}, nil
	case 0x0f:
		return KeyEvent{Key: KeyCtrlO}, nil
	case 0x14:
		return KeyEvent{Key: KeyCtrlT}, nil
	case 0x07:
		return KeyEvent{Key: KeyCtrlG}, nil
	case 0x18:
		return KeyEvent{Key: KeyCtrlX}, nil
	case 0x12:
		return KeyEvent{Key: KeyCtrlR}, nil
	case 0x13:
		return KeyEvent{Key: KeyCtrlS}, nil
	case 0x0c:
		return KeyEvent{Key: KeyCtrlL}, nil
	case 0x1f:
		// Ctrl+/ (and, on terminals that conflate the two physical keys,
		// Ctrl+_) universally arrives as the C0 code 0x1F (US, Unit
		// Separator) rather than the naively-computed '/' & 0x1f = 0x0F --
		// confirmed against macOS Terminal.app and iTerm2, both xterm-
		// compatible.
		return KeyEvent{Key: KeyCtrlSlash}, nil
	}
	if b < 0x20 {
		return KeyEvent{Key: KeyUnknown}, nil
	}
	if b >= 0x80 {
		if err := r.UnreadByte(); err != nil {
			return KeyEvent{}, err
		}
		rn, _, err := r.ReadRune()
		if err != nil {
			return KeyEvent{}, err
		}
		return KeyEvent{Key: KeyRune, Rune: rn}, nil
	}
	return KeyEvent{Key: KeyRune, Rune: rune(b)}, nil
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
