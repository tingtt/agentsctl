package terminal

import (
	"errors"
	"io"
)

// childModeResets neutralizes, in order, the physical-terminal modes a
// foreground child can leave behind when it does not get to run its own
// exit cleanup. It covers what the Codex TUI (rust-v0.156.1, through its
// crossterm fork) sets and what its restore_after_exit resets, in the same
// order:
//
//   - pointer and mouse reporting (DisableMouseCapture);
//   - the keyboard-enhancement stack: one pop for the TUI's alternate
//     screen frame, then reset_keyboard_reporting_after_exit's pop and
//     reset (CSI-u / Kitty protocol);
//   - alternate scroll;
//   - modifyOtherKeys, also from reset_keyboard_reporting_after_exit;
//   - focus-change reporting;
//   - the cursor: default user shape and visible.
//
// The child's alternate screen and bracketed paste are covered by the
// transport's own modes (see AlternateScreenLeaveFilter), not here. Each
// reset is idempotent, so sending them after a child that did clean up is
// harmless. The keyboard pops are sent while the transport's alternate
// screen is still selected: every push the child made landed there, and on
// terminals that keep a stack per screen, the user's main-screen stack is
// not touched.
var childModeResets = []string{
	mouseReportingDisable,
	keyboardEnhancementPop,
	alternateScrollDisable,
	keyboardEnhancementPop,
	keyboardEnhancementReset,
	modifyOtherKeysDisable,
	focusReportingDisable,
	cursorStyleDefault,
	cursorShow,
}

const (
	mouseReportingDisable    = "\x1b[?1006l\x1b[?1015l\x1b[?1003l\x1b[?1002l\x1b[?1000l"
	keyboardEnhancementPop   = "\x1b[<1u"
	keyboardEnhancementReset = "\x1b[<u"
	alternateScrollDisable   = "\x1b[?1007l"
	modifyOtherKeysDisable   = "\x1b[>4;0m"
	focusReportingDisable    = "\x1b[?1004l"
	cursorStyleDefault       = "\x1b[0 q"
	cursorShow               = "\x1b[?25h"
)

// resetChildModes writes every reset in childModeResets to out, attempting
// each one even when an earlier one fails, and returns the failures joined.
func resetChildModes(out io.Writer) error {
	var errs []error
	for _, reset := range childModeResets {
		errs = append(errs, WriteFull(out, []byte(reset)))
	}
	return errors.Join(errs...)
}
