package agentview

import "strings"

// Binding is one Agent View shortcut's physical key membership and
// footer/help display metadata -- the single source of truth for both
// *which physical Key(s)* a shortcut covers and how it is *displayed*. It
// carries no behavior: what a Key actually means is decided by
// State.Handle (update.go) given the current State, and never by Binding
// or its callers (see the DesignDoc's "Separate terminal protocol decoding
// from UI semantics" -- a Binding is documentation of a physical input
// plus a membership test, not a dispatch table). In particular Binding
// never stores an Intent or a callback: the same Key can mean different
// things in different States (Esc, Ctrl+X), which only State.Handle knows.
//
// This is deliberately not a configurable keybinding system -- there is no
// lookup from Key to behavior here, only a fixed, small list of the
// shortcuts footerText renders and Matches tests membership against. Keys
// lists every physical Key (see input_unix.go) the binding covers, e.g.
// both KeyUp and KeyDown for the combined "↑↓" label.
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

// Matches reports whether key is one of b's physical Keys -- the single
// place "does this shortcut own this Key" is decided. State.Handle uses
// Matches to find which shortcut a KeyEvent belongs to, then decides what
// it means itself; Matches never decides meaning.
func (b Binding) Matches(key Key) bool {
	for _, k := range b.Keys {
		if k == key {
			return true
		}
	}
	return false
}

// Named bindings are the source of truth for each shortcut's physical
// Key(s), label, and description. State.Handle (update.go) references
// these by name instead of the raw Key constants, so a shortcut's physical
// key can never drift between what the footer documents and what Handle
// actually acts on -- there is exactly one definition to change.
var (
	bindingProviderCycle = Binding{Label: "Shift+Tab", Keys: []Key{KeyShiftTab}}
	bindingSubmit        = Binding{Label: "Enter", Desc: "send/open", Keys: []Key{KeyEnter}}
	bindingNewline       = Binding{Label: "Option+Enter/Shift+Enter", Desc: "newline", Keys: []Key{KeyNewline}}
	bindingStash         = Binding{Label: "Ctrl+S", Desc: "stash", Keys: []Key{KeyCtrlS}}
	bindingOpen          = Binding{Label: "Ctrl+O", Keys: []Key{KeyCtrlO}}
	bindingPin           = Binding{Label: "Ctrl+T", Desc: "pin", Keys: []Key{KeyCtrlT}}
	bindingNavigate      = Binding{Label: "↑↓", Keys: []Key{KeyUp, KeyDown}}
	bindingScope         = Binding{Label: "Ctrl+G", Desc: "scope", Keys: []Key{KeyCtrlG}}
	bindingRename        = Binding{Label: "Ctrl+R", Desc: "rename", Keys: []Key{KeyCtrlR}}
	bindingStopArchive   = Binding{Label: "Ctrl+X", Desc: "stop/archive", Keys: []Key{KeyCtrlX}}
	bindingRefresh       = Binding{Label: "Ctrl+L", Desc: "refresh", Keys: []Key{KeyCtrlL}}
	bindingEscape        = Binding{Label: "Esc", Desc: "quit", Keys: []Key{KeyEsc}}
)

// footerLine1 and footerLine2 are the two Agent View footer rows (see
// render.go's View), and are meant to be what a future help view (#14)
// reads too -- composed from the named bindings above, so the footer and
// State.Handle's bound keys are two views of the same definitions rather
// than two independently-hardcoded copies (see keymap_test.go's coverage
// check).
var footerLine1 = []Binding{
	bindingProviderCycle,
	bindingSubmit,
	bindingNewline,
	bindingStash,
	bindingOpen,
	bindingPin,
}

var footerLine2 = []Binding{
	bindingNavigate,
	bindingScope,
	bindingRename,
	bindingStopArchive,
	bindingRefresh,
	bindingEscape,
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
