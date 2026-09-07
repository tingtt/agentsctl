package terminal

import "strconv"
import "strings"

// DetachKey is the classic literal detach byte (Ctrl+], 0x1d). Recognizing
// it (and the escape-sequence encodings some terminals substitute for it --
// see DetachScanner) in the outer terminal's raw byte stream is how a
// provider's Open transport (provider/claude, internal/supervisor) knows
// to end the interactive view and return to Agent View, without stopping
// the underlying provider session/process (see the DesignDoc's Attach/
// Detach lifecycle separation).
const DetachKey byte = 0x1d

// DetachScanner recognizes DetachKey in a raw outer-terminal byte stream --
// whether it arrives as the classic C0 byte or as one of two escape-
// sequence encodings some terminals substitute for it instead.
//
// This is real, not hypothetical, and was captured directly from the
// installed `claude` CLI + iTerm2 3.6.11 with a debug byte-dump build
// (issue #4): an attached client (Claude, but the same negotiation can
// happen with any client) negotiates xterm's `modifyOtherKeys` extended-
// key-reporting mode for its own enhanced key handling by writing an
// escape sequence to its pty, which an Open transport's output copy
// proxies verbatim to the real outer terminal. The outer terminal has no
// way to know that sequence "really" came from a nested child rather than
// agentsctl itself, so a terminal that honors it (confirmed: iTerm2, which
// replied with its own `modifyOtherKeys`-style report right after)
// switches its own key encoding in response -- meaning it starts sending
// Ctrl+] as `ESC [ 27 ; 5 ; 93 ~` (27 = the modifyOtherKeys marker, 5 =
// 1+Ctrl, 93 = ']') instead of the literal byte. Terminal.app doesn't
// support the extension and keeps sending the classic byte regardless,
// which is why the same build detaches cleanly there but not in iTerm2.
// The newer Kitty keyboard protocol / CSI-u convention (`ESC [ 93 ; 5 u`)
// encodes the same information differently and is recognized too, as a
// forward-looking match for terminals/clients that negotiate that
// convention instead -- unconfirmed against any real terminal so far,
// unlike the modifyOtherKeys form above.
//
// A scanner that only ever looked for the literal byte forwards either
// escape form straight into the child as ordinary (unbound, silently
// ignored) input instead of detaching -- matching the exact "Ctrl+] does
// nothing at all" symptom reported against iTerm2.
//
// One scanner instance must be used for an entire Open session (not
// recreated per read): either sequence can arrive split across separate
// Read() calls, and the scanner holds back a possibly-incomplete trailing
// escape sequence across Feed calls rather than risk forwarding partial
// escape bytes to the child or splitting a real match in two.
type DetachScanner struct {
	pending []byte
}

// Feed processes one new chunk of raw terminal input and returns the bytes
// that should be forwarded to the child right now (deliver) and whether
// the detach key was found in this chunk (detach). Everything from the
// detach key onward in this chunk (including any bytes after it) is
// intentionally dropped, not delivered, when detach is true.
func (d *DetachScanner) Feed(chunk []byte) (deliver []byte, detach bool) {
	buf := append(d.pending, chunk...)
	d.pending = nil
	for i := 0; i < len(buf); i++ {
		if buf[i] == DetachKey {
			return buf[:i], true
		}
		if buf[i] != 0x1b {
			continue
		}
		n, complete := scanEscape(buf[i:])
		if !complete {
			// Possibly-incomplete escape sequence at the tail: hold it
			// back for the next read instead of forwarding partial bytes
			// or risking a match split across two chunks.
			d.pending = append(d.pending, buf[i:]...)
			return buf[:i], false
		}
		if isDetachEscape(buf[i : i+n]) {
			return buf[:i], true
		}
		i += n - 1 // -1 to offset the loop's i++
	}
	return buf, false
}

// scanEscape reports how many leading bytes of buf (which starts with ESC)
// form one complete escape sequence, and whether a complete sequence was
// actually found (false means buf is truncated so far and more input is
// needed). Only the CSI form (ESC '[' ... final-byte in 0x40-0x7E, which
// covers CSI-u) is parsed structurally; any other escape form is treated
// as complete after its second byte so it is never held back indefinitely.
func scanEscape(buf []byte) (n int, complete bool) {
	if len(buf) < 2 {
		return 0, false
	}
	if buf[1] != '[' {
		return 2, true
	}
	for i := 2; i < len(buf); i++ {
		if buf[i] >= 0x40 && buf[i] <= 0x7e {
			return i + 1, true
		}
	}
	return 0, false
}

// isDetachEscape reports whether seq is a complete escape sequence
// encoding Ctrl+']' via either convention terminals use to report keys
// that would otherwise be ambiguous or swallowed as a control character.
// See DetachScanner's doc comment for which of these is actually confirmed
// against a real terminal.
func isDetachEscape(seq []byte) bool {
	return isCtrlBracketModifyOtherKeys(seq) || isCtrlBracketCSIu(seq)
}

// isCtrlBracketModifyOtherKeys reports whether seq is a complete xterm
// `modifyOtherKeys` sequence encoding Ctrl+']': `ESC [ 27 ; <modifier> ;
// <keycode> ~`, where 27 is a fixed marker (not part of the key itself),
// <keycode> is the Unicode codepoint of the base key (93 = ']'), and
// <modifier> is 1 + the sum of active modifier bits (Shift=1, Alt=2,
// Ctrl=4, ...). This is the form confirmed against the installed `claude`
// CLI + iTerm2 3.6.11 -- see DetachScanner's doc comment.
func isCtrlBracketModifyOtherKeys(seq []byte) bool {
	if len(seq) < 4 || seq[0] != 0x1b || seq[1] != '[' || seq[len(seq)-1] != '~' {
		return false
	}
	parts := strings.Split(string(seq[2:len(seq)-1]), ";")
	if len(parts) != 3 || parts[0] != "27" || parts[2] != "93" {
		return false
	}
	mod, err := strconv.Atoi(parts[1])
	if err != nil || mod < 1 {
		return false
	}
	return (mod-1)&0x4 != 0
}

// isCtrlBracketCSIu reports whether seq is a complete CSI-u ("Kitty
// keyboard protocol") sequence encoding Ctrl+']' (unicode codepoint 93),
// in either the plain (`93;5u`) or event-typed (`93;5:1u`) modifier form,
// and tolerating an alternate-key-codes suffix on the key-code field
// itself (`93:125;5u`). The modifier field is 1 + the sum of active
// modifier bits (Shift=1, Alt=2, Ctrl=4, ...); Ctrl being held is
// (mod-1)&4 != 0. Unconfirmed against a real terminal so far -- see
// DetachScanner's doc comment -- kept as a forward-looking match.
func isCtrlBracketCSIu(seq []byte) bool {
	if len(seq) < 4 || seq[0] != 0x1b || seq[1] != '[' || seq[len(seq)-1] != 'u' {
		return false
	}
	body := string(seq[2 : len(seq)-1])
	parts := strings.SplitN(body, ";", 2)
	keyCode := parts[0]
	if idx := strings.IndexByte(keyCode, ':'); idx >= 0 {
		keyCode = keyCode[:idx]
	}
	if keyCode != "93" {
		return false
	}
	if len(parts) < 2 {
		return false // no modifier field at all -- not Ctrl+]
	}
	modField := parts[1]
	if idx := strings.IndexByte(modField, ':'); idx >= 0 {
		modField = modField[:idx]
	}
	mod, err := strconv.Atoi(modField)
	if err != nil || mod < 1 {
		return false
	}
	return (mod-1)&0x4 != 0
}
