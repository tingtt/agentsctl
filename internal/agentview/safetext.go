package agentview

import "unicode/utf8"

// Untrusted text -- a pasted prompt, a rename draft, a session title, an error
// message quoting external data -- is model data, and stays byte-for-byte what
// it is in the model (Composer.Prompt, Rename.Draft, Session.Name, the
// dispatch payload). It must never become a terminal command just because it
// contains ESC, a C0 control (BEL, TAB, CR, ...), or a C1 control (which some
// terminals honor as a single-byte CSI/OSC introducer). So the rendering
// boundary -- the last step before that text is mixed with Agent View's own
// trusted ANSI styling -- encodes it into a printable form. Nothing after that
// boundary (styleText, clipLine, lineCells, ...) may see a raw control rune
// from user text; they keep treating ESC as the start of trusted application
// ANSI only.
//
// The encoding is one rune to one rune, so every rune index computed on the
// model text (Composer.Cursor, Rename.Cursor, reserved command spans) is the
// same index into the displayed text.

// safeRune returns the printable rune displayed in place of r:
//
//   - C0 controls U+0000..U+001F map to their Unicode Control Pictures
//     U+2400..U+241F (ESC -> ␛, TAB -> ␉, LF -> ␊, ...);
//   - DEL U+007F maps to U+2421 (␡);
//   - C1 controls U+0080..U+009F, which have no control picture, map to
//     U+FFFD;
//   - every other rune, including all printable text, is unchanged.
func safeRune(r rune) rune {
	switch {
	case r < 0x20:
		return 0x2400 + r
	case r == 0x7f:
		return 0x2421
	case r >= 0x80 && r < 0xa0:
		return utf8.RuneError
	}
	return r
}

// safeRunes is []rune(text) with every rune passed through safeRune. Invalid
// UTF-8 bytes already decode to one U+FFFD each, matching how the model
// indexes the same text.
func safeRunes(text string) []rune {
	runes := []rune(text)
	for i, r := range runes {
		runes[i] = safeRune(r)
	}
	return runes
}

// safeText returns text encoded for display (see safeRune). Text with nothing
// to encode is returned as is.
func safeText(text string) string {
	for _, r := range text {
		if safeRune(r) != r {
			return string(safeRunes(text))
		}
	}
	return text
}
