package codex

import (
	"strings"
	"unicode"
)

// renameBootstrapPrompt is the only text a rename-only new session ever
// sends to the model. It is fixed on purpose: the requested session name is
// user-controlled and must never be embedded in a model instruction (see
// parseRenameOnly, Provider.Dispatch).
//
// Codex does not expose a listed/resumable thread until the first model
// turn, so a rename-only session still runs one minimal bootstrap turn
// before the native rename (see Provider.Dispatch).
const renameBootstrapPrompt = "Wait for the next user prompt. Do not perform any task."

// parseRenameOnly recognizes a new-session composer input that consists of
// nothing but a single `/rename <name>` command. It is deliberately not a
// slash-command framework: it answers only whether this whole input is the
// rename command, and with which name.
//
//   - Surrounding whitespace of the whole input is ignored, as is the
//     whitespace around name; whitespace inside name is preserved.
//   - The command word must end at whitespace or at the end of input, so
//     `/renamex foo` is an ordinary prompt.
//   - Input that still spans several lines after trimming is an ordinary
//     prompt, never a rename: `/rename foo\nimplement it` is a task.
//
// isRename reports whether the input is the rename command; name is empty
// when the command carries no name, which the caller must reject rather
// than forward `/rename` to the model as a prompt.
func parseRenameOnly(prompt string) (name string, isRename bool) {
	trimmed := strings.TrimSpace(prompt)
	if strings.ContainsAny(trimmed, "\r\n") {
		return "", false
	}
	rest, ok := strings.CutPrefix(trimmed, "/rename")
	if !ok {
		return "", false
	}
	if rest != "" {
		if r := []rune(rest)[0]; !unicode.IsSpace(r) {
			return "", false
		}
	}
	return strings.TrimSpace(rest), true
}
