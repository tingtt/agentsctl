//go:build darwin || linux

package agentview

import (
	"bufio"
	"bytes"
	"testing"
)

// decode is a test helper: readKey against a fixed byte buffer, with no
// real terminal or poll wait involved -- exactly the "external CLI,
// filesystem, PTY なしで" guarantee the DesignDoc requires for terminal
// byte decoding.
func decode(t *testing.T, b []byte) KeyEvent {
	t.Helper()
	ev, err := readKey(bufio.NewReader(bytes.NewReader(b)))
	if err != nil {
		t.Fatal(err)
	}
	return ev
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
		{"Ctrl+X", []byte{0x18}, KeyEvent{Key: KeyCtrlX}},
		{"Ctrl+R", []byte{0x12}, KeyEvent{Key: KeyCtrlR}},
		{"Ctrl+S", []byte{0x13}, KeyEvent{Key: KeyCtrlS}},
		{"Ctrl+L", []byte{0x0c}, KeyEvent{Key: KeyCtrlL}},
		{"Ctrl+/", []byte{0x1f}, KeyEvent{Key: KeyCtrlSlash}},
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
