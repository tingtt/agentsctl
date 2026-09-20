//go:build darwin || linux

package agentview

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/creack/pty"
	"golang.org/x/term"
)

// decode is a test helper: readInput against a fixed byte buffer, with no
// real terminal or poll wait involved -- exactly the "external CLI,
// filesystem, PTY なしで" guarantee the DesignDoc requires for terminal
// byte decoding.
func decode(t *testing.T, b []byte) KeyEvent {
	t.Helper()
	ev, err := readInput(bufio.NewReader(bytes.NewReader(b)))
	if err != nil {
		t.Fatal(err)
	}
	if ev.Kind != InputKey {
		t.Fatalf("decoded %+v, want a key event", ev)
	}
	return ev.Key
}

func TestDecodeKeyPhysicalIdentitiesCarryNoUXMeaning(t *testing.T) {
	cases := []struct {
		name  string
		bytes []byte
		want  KeyEvent
	}{
		{"CR is Enter", []byte{'\r'}, KeyEvent{Key: KeyEnter}},
		{"LF is Newline (Shift+Enter)", []byte{'\n'}, KeyEvent{Key: KeyNewline}},
		{"ESC CR is Newline (Option+Enter)", []byte{0x1b, '\r'}, KeyEvent{Key: KeyNewline}},
		{"DEL is Backspace", []byte{0x7f}, KeyEvent{Key: KeyBackspace}},
		{"bare ESC is Esc", []byte{0x1b}, KeyEvent{Key: KeyEsc}},
		{"CSI A is Up", []byte{0x1b, '[', 'A'}, KeyEvent{Key: KeyUp}},
		{"CSI B is Down", []byte{0x1b, '[', 'B'}, KeyEvent{Key: KeyDown}},
		{"CSI Z is ShiftTab", []byte{0x1b, '[', 'Z'}, KeyEvent{Key: KeyShiftTab}},
		{"CSI 3~ is Delete", []byte{0x1b, '[', '3', '~'}, KeyEvent{Key: KeyDelete}},
		{"SS3 H is Home", []byte{0x1b, 'O', 'H'}, KeyEvent{Key: KeyHome}},
		{"Ctrl+O", []byte{0x0f}, KeyEvent{Key: KeyCtrlO}},
		{"Ctrl+T", []byte{0x14}, KeyEvent{Key: KeyCtrlT}},
		{"Ctrl+G", []byte{0x07}, KeyEvent{Key: KeyCtrlG}},
		{"Ctrl+/", []byte{0x1f}, KeyEvent{Key: KeyCtrlSlash}},
		{"Ctrl+X", []byte{0x18}, KeyEvent{Key: KeyCtrlX}},
		{"Ctrl+R", []byte{0x12}, KeyEvent{Key: KeyCtrlR}},
		{"Ctrl+S", []byte{0x13}, KeyEvent{Key: KeyCtrlS}},
		{"Ctrl+L", []byte{0x0c}, KeyEvent{Key: KeyCtrlL}},
		{"left brace remains a rune", []byte{'{'}, KeyEvent{Key: KeyRune, Rune: '{'}},
		{"right brace remains a rune", []byte{'}'}, KeyEvent{Key: KeyRune, Rune: '}'}},
		{"printable ascii", []byte{'q'}, KeyEvent{Key: KeyRune, Rune: 'q'}},
		{"printable utf8", []byte("あ"), KeyEvent{Key: KeyRune, Rune: 'あ'}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := decode(t, tc.bytes); got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestDecodeKeyNeverReturnsSemanticNames is an architectural regression
// guard: the decoder's vocabulary (Key) must stay physical. This test
// exists to be an obvious place future contributors would need to touch
// (and reconsider) if a semantic name like "quit" or "scope-cycle" were
// ever added back to Key's const block -- see the DesignDoc's "Separate
// terminal protocol decoding from UI semantics".
func TestDecodeKeyNeverReturnsSemanticNames(t *testing.T) {
	if decode(t, []byte{0x1b}).Key != KeyEsc {
		t.Fatal("Esc must decode to the physical KeyEsc, not a semantic quit/cancel action")
	}
	if decode(t, []byte{0x18}).Key != KeyCtrlX {
		t.Fatal("Ctrl+X must decode to the physical KeyCtrlX, not a semantic stop-or-archive action")
	}
}

const (
	pasteBegin = "\x1b[200~"
	pasteEnd   = "\x1b[201~"
)

// decodeAll reads every event until EOF from a fixed byte stream. A bare ESC
// is always treated as the start of a sequence whose remaining bytes are still
// in flight (the poll-based wait's "ready" answer), so the stream's read
// boundaries -- which a test controls arbitrarily -- never decide whether ESC
// is the Esc key.
func decodeAll(t *testing.T, r io.Reader) []InputEvent {
	t.Helper()
	reader := bufio.NewReader(r)
	var events []InputEvent
	for {
		ev, err := readInputWithEscapeWait(reader, func() (bool, error) { return true, nil })
		if errors.Is(err, io.EOF) {
			return events
		}
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, ev)
	}
}

func TestBracketedPasteIsOneEventWithoutMarkers(t *testing.T) {
	events := decodeAll(t, strings.NewReader(pasteBegin+"first\nsecond"+pasteEnd))
	if len(events) != 1 || events[0].Kind != InputPaste || events[0].Paste != "first\nsecond" {
		t.Fatalf("events=%+v, want exactly one paste of %q", events, "first\nsecond")
	}
}

// The decoder hands the payload over verbatim -- newline normalization is the
// editor boundary's job -- and never turns a payload CR/LF into Enter/Newline.
func TestBracketedPasteNewlinesAreNotKeys(t *testing.T) {
	payload := "a\rb\nc\r\nd"
	events := decodeAll(t, strings.NewReader(pasteBegin+payload+pasteEnd))
	if len(events) != 1 || events[0].Kind != InputPaste || events[0].Paste != payload {
		t.Fatalf("events=%+v, want one verbatim paste of %q", events, payload)
	}
}

func TestBracketedPasteKeepsUTF8Intact(t *testing.T) {
	payload := "日本語\nこんにちは"
	// One byte per read splits every multi-byte rune across reads.
	events := decodeAll(t, iotest.OneByteReader(strings.NewReader(pasteBegin+payload+pasteEnd)))
	if len(events) != 1 || events[0].Paste != payload {
		t.Fatalf("events=%+v, want %q", events, payload)
	}
}

func TestBracketedPasteTreatsEscapesAndControlsAsContent(t *testing.T) {
	payload := "up\x1b[A del\x1b[3~ ctrl\x18\x07 esc\x1b end\x1b[200~ nested \x1b[20"
	events := decodeAll(t, strings.NewReader(pasteBegin+payload+pasteEnd))
	if len(events) != 1 || events[0].Kind != InputPaste || events[0].Paste != payload {
		t.Fatalf("events=%+v, want the whole payload as one paste %q", events, payload)
	}
}

func TestBracketedPasteIsIndependentOfReadBoundaries(t *testing.T) {
	stream := "x" + pasteBegin + "a\x1b[201 b\r\nc" + pasteEnd + "\r"
	want := []InputEvent{
		keyInput(KeyEvent{Key: KeyRune, Rune: 'x'}),
		pasteInput("a\x1b[201 b\r\nc"),
		keyInput(KeyEvent{Key: KeyEnter}),
	}
	readers := map[string]func() io.Reader{
		"whole":    func() io.Reader { return strings.NewReader(stream) },
		"one byte": func() io.Reader { return iotest.OneByteReader(strings.NewReader(stream)) },
		"half":     func() io.Reader { return iotest.HalfReader(strings.NewReader(stream)) },
		"data err": func() io.Reader { return iotest.DataErrReader(strings.NewReader(stream)) },
	}
	for name, newReader := range readers {
		t.Run(name, func(t *testing.T) {
			if got := decodeAll(t, newReader()); !reflect.DeepEqual(got, want) {
				t.Fatalf("events=%+v, want %+v", got, want)
			}
		})
	}
	// Every two-way split of the marker bytes decodes identically.
	for i := 1; i < len(stream); i++ {
		got := decodeAll(t, io.MultiReader(strings.NewReader(stream[:i]), strings.NewReader(stream[i:])))
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("split at %d: events=%+v, want %+v", i, got, want)
		}
	}
}

func TestBracketedPasteThenPlainEnter(t *testing.T) {
	events := decodeAll(t, strings.NewReader(pasteBegin+"one\ntwo"+pasteEnd+"\r"))
	want := []InputEvent{pasteInput("one\ntwo"), keyInput(KeyEvent{Key: KeyEnter})}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events=%+v, want %+v", events, want)
	}
}

func TestEmptyBracketedPasteIsAnEmptyPasteEvent(t *testing.T) {
	events := decodeAll(t, strings.NewReader(pasteBegin+pasteEnd))
	if !reflect.DeepEqual(events, []InputEvent{pasteInput("")}) {
		t.Fatalf("events=%+v", events)
	}
}

// A paste larger than any plausible fixed cap still arrives as one event.
func TestBracketedPasteHasNoSmallSizeLimit(t *testing.T) {
	payload := strings.Repeat("line of pasted text\r\n", 200_000)
	events := decodeAll(t, strings.NewReader(pasteBegin+payload+pasteEnd))
	if len(events) != 1 || events[0].Paste != payload {
		t.Fatalf("got %d events; large paste was not preserved", len(events))
	}
}

func TestUnterminatedBracketedPasteEndsWithAnErrorNotAHang(t *testing.T) {
	_, err := readInput(bufio.NewReader(strings.NewReader(pasteBegin + "never ends\x1b[201")))
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("err=%v, want io.ErrUnexpectedEOF", err)
	}
}

func TestReadTerminalInputDecodesPasteWithPollWait(t *testing.T) {
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	defer slave.Close()
	if _, err := term.MakeRaw(int(slave.Fd())); err != nil {
		t.Fatal(err)
	}
	// The begin marker and payload reach the reader in separate writes.
	go func() {
		_, _ = master.WriteString(pasteBegin[:2])
		_, _ = master.WriteString(pasteBegin[2:] + "first\r\nsecond\n日本語")
		_, _ = master.WriteString(pasteEnd + "\r")
	}()
	reader := bufio.NewReader(slave)
	first, err := readTerminalInput(reader, slave)
	if err != nil {
		t.Fatal(err)
	}
	if first != pasteInput("first\r\nsecond\n日本語") {
		t.Fatalf("first=%+v", first)
	}
	second, err := readTerminalInput(reader, slave)
	if err != nil {
		t.Fatal(err)
	}
	if second != keyInput(KeyEvent{Key: KeyEnter}) {
		t.Fatalf("second=%+v", second)
	}
}
