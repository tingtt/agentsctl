package terminal

import "bytes"
import "testing"

// TestDetachScannerRecognizesEncodedCtrlBracket covers issue #4: iTerm2
// (and any other terminal that honors extended-key-reporting a client
// negotiates for its own key handling, which gets proxied through to the
// real terminal by an Open transport's output copy) sends Ctrl+] as an
// escape sequence instead of the classic literal byte 0x1d. Terminal.app
// doesn't support the extension and keeps sending the literal byte, which
// is why the same build detached cleanly there but silently did nothing in
// iTerm2. A scanner that only recognized the literal byte would forward
// these sequences straight into the child as ordinary, unbound, silently-
// ignored input.
//
// The `ESC [ 27 ; 5 ; 93 ~` (xterm modifyOtherKeys) cases are the real,
// confirmed encoding: captured with a debug byte-dump build against the
// installed `claude` CLI + iTerm2 3.6.11 (see DetachScanner's doc
// comment). The CSI-u ("Kitty keyboard protocol") cases are kept as a
// forward-looking match; no real terminal has been observed sending that
// form for this key.
//
// This test needs no PTY, subprocess, or real terminal: DetachScanner is a
// pure byte-stream decoder, exactly the kind of physical terminal-protocol
// parsing the DesignDoc's input-decoding boundary requires be testable in
// isolation.
func TestDetachScannerRecognizesEncodedCtrlBracket(t *testing.T) {
	cases := []struct {
		name   string
		chunks [][]byte
		detach bool
	}{
		{"literal byte, one chunk", [][]byte{{'a', DetachKey, 'b'}}, true},
		{"modifyOtherKeys form, one chunk (confirmed: iTerm2 3.6.11)", [][]byte{[]byte("hi\x1b[27;5;93~")}, true},
		{"modifyOtherKeys split across two reads", [][]byte{[]byte("hi\x1b[27;5;"), []byte("93~")}, true},
		{"modifyOtherKeys split mid marker", [][]byte{[]byte("\x1b[2"), []byte("7;5;93~")}, true},
		{"modifyOtherKeys without Ctrl held (shift only) is not a match", [][]byte{[]byte("\x1b[27;1;93~")}, false},
		{"modifyOtherKeys for a different key (']' vs 'z'=122) is not a match", [][]byte{[]byte("\x1b[27;5;122~")}, false},
		{"CSI-u plain modifier form, one chunk", [][]byte{[]byte("hi\x1b[93;5u")}, true},
		{"CSI-u event-typed modifier form", [][]byte{[]byte("\x1b[93;5:1u")}, true},
		{"CSI-u with alternate-key-codes prefix", [][]byte{[]byte("\x1b[93:125;5u")}, true},
		{"CSI-u split across two reads", [][]byte{[]byte("hi\x1b[93;"), []byte("5u")}, true},
		{"CSI-u split mid keycode", [][]byte{[]byte("\x1b[9"), []byte("3;5u")}, true},
		{"CSI-u without Ctrl held (shift only) is not a match", [][]byte{[]byte("\x1b[93;2u")}, false},
		{"CSI-u for a different key (']' vs 'a'=97) is not a match", [][]byte{[]byte("\x1b[97;5u")}, false},
		{"CSI-u with no modifier field is not a match", [][]byte{[]byte("\x1b[93u")}, false},
		{"unrelated CSI sequence (e.g. an arrow key) passes through", [][]byte{[]byte("\x1b[A")}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var scanner DetachScanner
			var delivered []byte
			detach := false
			for _, chunk := range tc.chunks {
				before, found := scanner.Feed(chunk)
				delivered = append(delivered, before...)
				if found {
					detach = true
				}
			}
			if detach != tc.detach {
				t.Fatalf("detach=%v, want %v (delivered=%q, pending=%q)", detach, tc.detach, delivered, scanner.pending)
			}
			if tc.detach && bytes.Contains(delivered, []byte{DetachKey}) {
				t.Fatal("detach key reached the delivered/forwarded bytes")
			}
		})
	}
}
