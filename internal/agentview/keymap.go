package agentview

import "strings"

// Binding is one Agent View shortcut's physical key label and footer/help
// description -- the single source of truth for how a shortcut is
// *displayed*. It carries no behavior: what a Key actually does is decided
// by State.Handle (update.go) given the current State, and never by
// Binding or its callers (see the DesignDoc's "Separate terminal protocol
// decoding from UI semantics" -- a Binding is documentation of a physical
// input, not a dispatch table).
//
// This is deliberately not a configurable keybinding system -- there is no
// lookup from Key to behavior here, only a fixed, small list of the
// shortcuts footerText renders. Keys lists every physical Key (see
// input_unix.go) the binding covers, e.g. both KeyUp and KeyDown for the
// combined "↑↓" label.
type Binding struct {
	Label string
	Desc  string // empty when the binding's meaning is obvious from Label alone
	Keys  []Key
}

// String renders b as it appears in the footer: "Label" alone, or
// "Label Desc" when Desc is set.
func (b Binding) String() string {
	if b.Desc == "" {
		return b.Label
	}
	return b.Label + " " + b.Desc
}

// footerLine1 and footerLine2 are the two Agent View footer rows (see
// render.go's View), and are meant to be what a future help view (#14)
// reads too -- the single place shortcut key labels and descriptions are
// defined, so State.Handle's bound keys and the footer/help text cannot
// drift apart the way two independently-hardcoded strings could (see
// keymap_test.go's coverage check).
var footerLine1 = []Binding{
	{Label: "Shift+Tab", Keys: []Key{KeyShiftTab}},
	{Label: "Enter", Desc: "send/open", Keys: []Key{KeyEnter}},
	{Label: "Option+Enter/Shift+Enter", Desc: "newline", Keys: []Key{KeyNewline}},
	{Label: "Ctrl+S", Desc: "stash", Keys: []Key{KeyCtrlS}},
	{Label: "Ctrl+O", Keys: []Key{KeyCtrlO}},
	{Label: "Ctrl+T", Desc: "pin", Keys: []Key{KeyCtrlT}},
	{Label: "Ctrl+/", Desc: "depth", Keys: []Key{KeyCtrlSlash}},
}

var footerLine2 = []Binding{
	{Label: "↑↓", Keys: []Key{KeyUp, KeyDown}},
	{Label: "Ctrl+G", Desc: "scope", Keys: []Key{KeyCtrlG}},
	{Label: "Ctrl+R", Desc: "rename", Keys: []Key{KeyCtrlR}},
	{Label: "Ctrl+X", Desc: "stop/archive", Keys: []Key{KeyCtrlX}},
	{Label: "Ctrl+L", Desc: "refresh", Keys: []Key{KeyCtrlL}},
	{Label: "Esc", Desc: "quit", Keys: []Key{KeyEsc}},
}

// footerText joins bindings into one footer row, in order, the way
// render.go's View has always displayed them: "Label[ Desc] / Label[
// Desc] / ...".
func footerText(bindings []Binding) string {
	parts := make([]string, len(bindings))
	for i, b := range bindings {
		parts[i] = b.String()
	}
	return strings.Join(parts, " / ")
}
