//go:build darwin || linux

// Package agentview is the Agent View TUI: the terminal loop, mutable UI
// state, and rendering for the unified session list and composer. It
// depends only on internal/session and internal/sessionctl -- never on a
// concrete provider package (internal/provider/claude,
// internal/provider/codex) or internal/terminal -- so a provider
// capability change or a new provider never requires touching this package (see the
// DesignDoc's "TUI must not know provider implementation details").
package agentview

import (
	"bufio"
	"bytes"
	"errors"
	"io"
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
	// KeyCtrlG, KeyCtrlSlash, KeyCtrlX, KeyCtrlR, KeyCtrlT, KeyCtrlS,
	// KeyCtrlL, and KeyCtrlO are the fixed physical shortcuts this build
	// recognizes. What each means is entirely up to State.Handle.
	KeyCtrlG
	KeyCtrlSlash
	KeyCtrlX
	KeyCtrlR
	KeyCtrlT
	KeyCtrlS
	KeyCtrlL
	KeyCtrlO
	// KeyUnknown is a recognized-but-unbound escape sequence or control
	// byte: physically decoded, but carrying no assigned meaning.
	KeyUnknown
)

// KeyEvent is one physically-decoded terminal input.
type KeyEvent struct {
	Key  Key
	Rune rune // valid only when Key == KeyRune
}

// InputEventKind tells which payload of an InputEvent is meaningful.
type InputEventKind int

const (
	// InputKey is one physical key press (InputEvent.Key).
	InputKey InputEventKind = iota
	// InputPaste is one whole bracketed paste (InputEvent.Paste).
	InputPaste
)

// InputEvent is one decoded terminal input: either a physical key or a
// bracketed paste. A paste is deliberately not a Key -- it is not a physical
// key, and its payload (CR, LF, ESC, control bytes, ...) is content that
// carries no key semantics. See the DesignDoc's terminal bytes -> KeyEvent or
// PasteEvent -> State -> Intent pipeline.
type InputEvent struct {
	Kind InputEventKind
	Key  KeyEvent // valid only when Kind == InputKey
	// Paste is the raw payload between the bracketed paste markers, exactly as
	// the terminal sent it: no marker bytes, no newline normalization.
	Paste string // valid only when Kind == InputPaste
}

func keyInput(ev KeyEvent) InputEvent { return InputEvent{Kind: InputKey, Key: ev} }

func pasteInput(text string) InputEvent { return InputEvent{Kind: InputPaste, Paste: text} }

// Bracketed paste framing (DECSET 2004): the terminal wraps pasted text in
// these markers.
const (
	bracketedPasteBeginCSI = "200~"
	bracketedPasteEnd      = "\x1b[201~"
)

// readInput decodes one InputEvent from r, blocking for at least one byte.
func readInput(r *bufio.Reader) (InputEvent, error) {
	return readInputWithEscapeWait(r, nil)
}

// readTerminalInput is readInput with a real poll-based wait for a bare ESC's
// possible following bytes, distinguishing a standalone Esc press from the
// start of a multi-byte escape sequence racing this read.
func readTerminalInput(r *bufio.Reader, input *os.File) (InputEvent, error) {
	return readInputWithEscapeWait(r, func() (bool, error) {
		fds := []unix.PollFd{{Fd: int32(input.Fd()), Events: unix.POLLIN}}
		n, err := unix.Poll(fds, 30)
		if errors.Is(err, syscall.EINTR) {
			return false, nil
		}
		return n > 0, err
	})
}

// readBracketedPaste reads a paste payload up to and including its end
// marker, after the begin marker has already been consumed. Everything before
// the end marker is content -- never re-fed to the key decoder -- and there is
// no size limit: only the end marker or a read error (including EOF, which
// discards the unterminated paste) ends it, so no read boundary matters.
func readBracketedPaste(r *bufio.Reader) (InputEvent, error) {
	var payload []byte
	for {
		b, err := r.ReadByte()
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF
			}
			return InputEvent{}, err
		}
		payload = append(payload, b)
		if b == bracketedPasteEnd[len(bracketedPasteEnd)-1] && bytes.HasSuffix(payload, []byte(bracketedPasteEnd)) {
			return pasteInput(string(payload[:len(payload)-len(bracketedPasteEnd)])), nil
		}
	}
}

// readInputWithEscapeWait is the single terminal decoding entry point: bytes
// in, one InputEvent out (a physical key, or one whole bracketed paste),
// nothing else. wait (nil in tests that don't care about the standalone-Esc
// race) reports whether more input is ready without blocking indefinitely.
func readInputWithEscapeWait(r *bufio.Reader, wait func() (bool, error)) (InputEvent, error) {
	b, err := r.ReadByte()
	if err != nil {
		return InputEvent{}, err
	}
	switch b {
	case 0x1b:
		if r.Buffered() == 0 {
			if wait == nil {
				return keyInput(KeyEvent{Key: KeyEsc}), nil
			}
			ready, err := wait()
			if err != nil {
				return InputEvent{}, err
			}
			if !ready {
				return keyInput(KeyEvent{Key: KeyEsc}), nil
			}
		}
		next, err := r.ReadByte()
		if err != nil {
			return InputEvent{}, err
		}
		if next == '\r' || next == '\n' {
			// Option+Enter: the Option key follows the classic "meta sends
			// escape" convention, so it arrives as ESC followed by
			// whatever byte that terminal sends for plain Enter.
			return keyInput(KeyEvent{Key: KeyNewline}), nil
		}
		if next == 'O' {
			// SS3-form keys (ESC O <letter>), not just the CSI form. The
			// terminfo entry for TERM=xterm-256color declares
			// kcub1/kcuf1/kcuu1/kcud1 this way, e.g. under
			// DECCKM/application-cursor-key mode.
			final, err := r.ReadByte()
			if err != nil {
				return InputEvent{}, err
			}
			switch final {
			case 'H':
				return keyInput(KeyEvent{Key: KeyHome}), nil
			case 'F':
				return keyInput(KeyEvent{Key: KeyEnd}), nil
			case 'A':
				return keyInput(KeyEvent{Key: KeyUp}), nil
			case 'B':
				return keyInput(KeyEvent{Key: KeyDown}), nil
			case 'C':
				return keyInput(KeyEvent{Key: KeyRight}), nil
			case 'D':
				return keyInput(KeyEvent{Key: KeyLeft}), nil
			}
			return keyInput(KeyEvent{Key: KeyUnknown}), nil
		}
		if next != '[' {
			return keyInput(KeyEvent{Key: KeyUnknown}), nil
		}
		sequence, err := readCSI(r)
		if err != nil {
			return InputEvent{}, err
		}
		switch sequence {
		case bracketedPasteBeginCSI:
			return readBracketedPaste(r)
		case "Z":
			return keyInput(KeyEvent{Key: KeyShiftTab}), nil
		case "A":
			return keyInput(KeyEvent{Key: KeyUp}), nil
		case "B":
			return keyInput(KeyEvent{Key: KeyDown}), nil
		case "C":
			return keyInput(KeyEvent{Key: KeyRight}), nil
		case "D":
			return keyInput(KeyEvent{Key: KeyLeft}), nil
		case "H", "1~", "7~":
			return keyInput(KeyEvent{Key: KeyHome}), nil
		case "F", "4~", "8~":
			return keyInput(KeyEvent{Key: KeyEnd}), nil
		case "3~":
			return keyInput(KeyEvent{Key: KeyDelete}), nil
		}
		return keyInput(KeyEvent{Key: KeyUnknown}), nil
	case '\r':
		return keyInput(KeyEvent{Key: KeyEnter}), nil
	case '\n':
		// Shift+Enter: plain Enter sends CR, Shift+Enter sends a bare LF
		// instead -- the same CR-vs-LF distinction readline/fish/tmux rely
		// on for this key combination.
		return keyInput(KeyEvent{Key: KeyNewline}), nil
	case 0x7f, 0x08:
		return keyInput(KeyEvent{Key: KeyBackspace}), nil
	case 0x0f:
		return keyInput(KeyEvent{Key: KeyCtrlO}), nil
	case 0x14:
		return keyInput(KeyEvent{Key: KeyCtrlT}), nil
	case 0x07:
		return keyInput(KeyEvent{Key: KeyCtrlG}), nil
	case 0x1f:
		return keyInput(KeyEvent{Key: KeyCtrlSlash}), nil
	case 0x18:
		return keyInput(KeyEvent{Key: KeyCtrlX}), nil
	case 0x12:
		return keyInput(KeyEvent{Key: KeyCtrlR}), nil
	case 0x13:
		return keyInput(KeyEvent{Key: KeyCtrlS}), nil
	case 0x0c:
		return keyInput(KeyEvent{Key: KeyCtrlL}), nil
	}
	if b < 0x20 {
		return keyInput(KeyEvent{Key: KeyUnknown}), nil
	}
	if b >= 0x80 {
		if err := r.UnreadByte(); err != nil {
			return InputEvent{}, err
		}
		rn, _, err := r.ReadRune()
		if err != nil {
			return InputEvent{}, err
		}
		return keyInput(KeyEvent{Key: KeyRune, Rune: rn}), nil
	}
	return keyInput(KeyEvent{Key: KeyRune, Rune: rune(b)}), nil
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
