package agentview

import "testing"

func TestReplacePromptMaintainsComposerInvariants(t *testing.T) {
	c := Composer{
		Prompt:             "old\nvalue",
		Cursor:             2,
		Stash:              "keep me",
		preferredColumn:    4,
		hasPreferredColumn: true,
	}
	c.ReplacePrompt("日本語\nupdated")
	if c.Prompt != "日本語\nupdated" {
		t.Fatalf("Prompt=%q", c.Prompt)
	}
	if c.Cursor != len([]rune(c.Prompt)) {
		t.Fatalf("Cursor=%d, want rune length %d", c.Cursor, len([]rune(c.Prompt)))
	}
	if c.Stash != "keep me" {
		t.Fatalf("Stash=%q, want unchanged", c.Stash)
	}
	if c.hasPreferredColumn {
		t.Fatal("replacing the prompt must reset vertical navigation state")
	}
}

// TestCursorUpDownPreferredColumnSurvivesShorterLine fixes #14's "long ->
// short -> long" preferred-column requirement: moving down through a
// shorter intermediate line and back onto a longer one restores the
// original column rather than sticking to wherever the short line clamped
// it.
func TestCursorUpDownPreferredColumnSurvivesShorterLine(t *testing.T) {
	c := Composer{Prompt: "longline\nhi\nlongline2"}
	c.Cursor = 8 // end of "longline" (col 8)

	c.CursorDown()
	if got, want := c.Cursor, 8+1+2; got != want { // "hi" clamped to its own EOL (col 2)
		t.Fatalf("after first CursorDown: Cursor=%d, want %d (clamped onto \"hi\")", got, want)
	}

	c.CursorDown()
	want := 8 + 1 + 2 + 1 + 8 // start of "longline2" + preferred col 8
	if c.Cursor != want {
		t.Fatalf("after second CursorDown: Cursor=%d, want %d (preferred column 8 restored)", c.Cursor, want)
	}
}

// TestCursorUpNoopOnFirstLine fixes that Up at the first logical line
// doesn't move the cursor (there is nowhere for it to go).
func TestCursorUpNoopOnFirstLine(t *testing.T) {
	c := Composer{Prompt: "abc\ndef", Cursor: 1}
	c.CursorUp()
	if c.Cursor != 1 {
		t.Fatalf("Cursor=%d, want unchanged 1", c.Cursor)
	}
}

// TestCursorDownNoopOnLastLine mirrors TestCursorUpNoopOnFirstLine for the
// last logical line.
func TestCursorDownNoopOnLastLine(t *testing.T) {
	c := Composer{Prompt: "abc\ndef", Cursor: 5} // "de|f" on the second line
	c.CursorDown()
	if c.Cursor != 5 {
		t.Fatalf("Cursor=%d, want unchanged 5", c.Cursor)
	}
}

// TestCursorAtEOLMovesToAnalogousColumnNotTargetEOL fixes the "cursor at
// EOL" case: leaving from a short line's own EOL must carry that exact
// column onto a longer next line, not force the target line's own EOL.
func TestCursorAtEOLMovesToAnalogousColumnNotTargetEOL(t *testing.T) {
	c := Composer{Prompt: "ab\nabcdef", Cursor: 2} // end of "ab"
	c.CursorDown()
	if want := 2 + 1 + 2; c.Cursor != want { // "abcdef"'s column 2, not its EOL (6)
		t.Fatalf("Cursor=%d, want %d (column 2 into \"abcdef\", not its EOL)", c.Cursor, want)
	}
}

// TestCursorThroughEmptyLogicalLine fixes navigating through a blank
// line in the middle of a multiline prompt and back onto a later line
// that can hold the original preferred column.
func TestCursorThroughEmptyLogicalLine(t *testing.T) {
	c := Composer{Prompt: "abc\n\nxyz", Cursor: 1} // "a|bc"
	c.CursorDown()
	if want := 3 + 1 + 0; c.Cursor != want { // clamped onto the empty line
		t.Fatalf("Cursor=%d, want %d (clamped onto the empty line)", c.Cursor, want)
	}
	c.CursorDown()
	if want := 3 + 1 + 0 + 1 + 1; c.Cursor != want { // preferred column 1 restored in "xyz"
		t.Fatalf("Cursor=%d, want %d (preferred column 1 restored)", c.Cursor, want)
	}
}

// TestCursorUpDownUnicodePromptIsRuneSafe fixes that vertical movement
// indexes by rune, not byte, across multi-byte glyphs.
func TestCursorUpDownUnicodePromptIsRuneSafe(t *testing.T) {
	c := Composer{Prompt: "あいう\nかき"}
	c.Cursor = 3 // end of "あいう" (3 runes)
	c.CursorDown()
	want := 3 + 1 + 2 // clamped onto "かき"'s own EOL (2 runes)
	if c.Cursor != want {
		t.Fatalf("Cursor=%d, want %d", c.Cursor, want)
	}
	c.CursorUp()
	if c.Cursor != 3 {
		t.Fatalf("Cursor=%d, want 3 (preferred column 3 restored in \"あいう\")", c.Cursor)
	}
}

// TestHorizontalEditResetsPreferredColumn fixes that the preferred column
// is a property of a single uninterrupted run of vertical moves: any
// horizontal edit in between must make the next vertical move start fresh
// from the cursor's actual column again, not a stale one.
func TestHorizontalEditResetsPreferredColumn(t *testing.T) {
	c := Composer{Prompt: "longline\nhi\nlongline2"}
	c.Cursor = 8
	c.CursorDown() // preferred column becomes 8, clamped onto "hi" (col 2)
	c.Left()       // an unrelated horizontal edit -- col is now 1, preference must reset
	c.CursorDown()
	want := 8 + 1 + 2 + 1 + 1 // preferred column is now 1 (Left's actual column), not the stale 8
	if c.Cursor != want {
		t.Fatalf("Cursor=%d, want %d (preferred column reset by Left)", c.Cursor, want)
	}
}
