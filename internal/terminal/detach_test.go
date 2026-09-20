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

// feedAll runs chunks through one scanner and returns everything it
// delivered plus whether (and at which chunk) it reported detach. A chunk
// after detach is not fed, matching pumpAttachInput, which stops at detach.
func feedAll(chunks [][]byte) (delivered []byte, detach bool, scanner *DetachScanner) {
	scanner = &DetachScanner{}
	for _, chunk := range chunks {
		before, found := scanner.Feed(chunk)
		delivered = append(delivered, before...)
		if found {
			return delivered, true, scanner
		}
	}
	return delivered, false, scanner
}

// splitEvery cuts b into consecutive chunks of at most size bytes.
func splitEvery(b []byte, size int) [][]byte {
	var out [][]byte
	for len(b) > 0 {
		n := min(size, len(b))
		out = append(out, append([]byte(nil), b[:n]...))
		b = b[n:]
	}
	return out
}

const (
	beginMarker = "\x1b[200~"
	endMarker   = "\x1b[201~"
)

// pastePayloads are bracketed-paste bodies containing bytes a scanner that
// ignored paste framing would take for agentsctl's own detach command.
var pastePayloads = map[string]string{
	"literal detach byte":    "abc \x1d xyz",
	"modifyOtherKeys Ctrl+]": "abc \x1b[27;5;93~ xyz",
	"CSI-u Ctrl+]":           "abc \x1b[93;5u xyz",
	"CR LF and UTF-8":        "日本語\r\n二行目\rthird\n" + "\xe3\x81\x82",
	"unterminated CSI":       "ends with \x1b[",
	"nested begin marker":    "a" + beginMarker + "b",
}

// TestDetachScannerForwardsBracketedPasteVerbatim pins that bytes inside a
// bracketed paste are never interpreted as agentsctl control input: the
// scanner recognizes only the begin/end framing, and forwards the whole
// stream unchanged whatever the read boundaries are.
func TestDetachScannerForwardsBracketedPasteVerbatim(t *testing.T) {
	for name, payload := range pastePayloads {
		input := []byte("pre " + beginMarker + payload + endMarker + " post")
		for size := 1; size <= len(input); size++ {
			delivered, detach, scanner := feedAll(splitEvery(input, size))
			if detach {
				t.Fatalf("%s: chunk size %d: paste payload was taken for a detach", name, size)
			}
			// Feed may hold back a trailing partial escape sequence; a
			// following ordinary byte flushes it, as in real input.
			tail, _ := scanner.Feed([]byte("."))
			delivered = append(delivered, tail...)
			if want := string(input) + "."; string(delivered) != want {
				t.Fatalf("%s: chunk size %d: delivered %q, want %q", name, size, delivered, want)
			}
		}
	}
}

// TestDetachScannerSplitsEveryBoundaryOfPasteMarkers cuts a paste at every
// pair of byte positions, so both markers straddle reads in every way.
func TestDetachScannerSplitsEveryBoundaryOfPasteMarkers(t *testing.T) {
	input := []byte(beginMarker + "a\x1d\x1b[93;5u" + endMarker)
	for i := 0; i <= len(input); i++ {
		for j := i; j <= len(input); j++ {
			chunks := [][]byte{input[:i], input[i:j], input[j:]}
			delivered, detach, scanner := feedAll(chunks)
			if detach {
				t.Fatalf("split at %d,%d: detach inside paste", i, j)
			}
			tail, _ := scanner.Feed([]byte("."))
			delivered = append(delivered, tail...)
			if want := string(input) + "."; string(delivered) != want {
				t.Fatalf("split at %d,%d: delivered %q, want %q", i, j, delivered, want)
			}
		}
	}
}

// TestDetachScannerDetachesAfterPasteEnds pins that paste state does not
// outlive the end marker, however the marker or the detach is split.
func TestDetachScannerDetachesAfterPasteEnds(t *testing.T) {
	detaches := map[string]string{
		"literal":         "\x1d",
		"modifyOtherKeys": "\x1b[27;5;93~",
		"CSI-u":           "\x1b[93;5u",
	}
	for name, detachSeq := range detaches {
		input := []byte("x" + beginMarker + "a\x1db" + endMarker + detachSeq + "dropped")
		prefix := "x" + beginMarker + "a\x1db" + endMarker
		for size := 1; size <= len(input); size++ {
			delivered, detach, _ := feedAll(splitEvery(input, size))
			if !detach {
				t.Fatalf("%s: chunk size %d: no detach after paste end", name, size)
			}
			if string(delivered) != prefix {
				t.Fatalf("%s: chunk size %d: delivered %q, want %q", name, size, delivered, prefix)
			}
		}
	}
}

// TestDetachScannerDetachesBeforePasteBegins pins that a detach in normal
// state still wins over a paste that follows it in the same read.
func TestDetachScannerDetachesBeforePasteBegins(t *testing.T) {
	delivered, detach, _ := feedAll([][]byte{[]byte("abc \x1d xyz" + beginMarker + "p" + endMarker)})
	if !detach || string(delivered) != "abc " {
		t.Fatalf("delivered=%q detach=%v", delivered, detach)
	}
}
