package agentview

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/tingtt/agentsctl/internal/selfupdate"
)

// updateCommand is the agentsctl-owned composer command that installs the
// advertised release. It is never a provider prompt: once its token is
// recognized (see handleUpdateCommand), the prompt is handled locally
// whatever follows it.
const updateCommand = "/update"

// updateNotice renders the persistent update-availability notice for the
// composer-top notification area, if any. Error takes display priority over
// it (see View); this only decides its own text.
func (s State) updateNotice() (string, bool) {
	a := s.UpdateAvailable
	if a == nil {
		return "", false
	}
	transition := fmt.Sprintf("(%s -> %s)", a.Current, a.Latest)
	switch {
	case s.Updating:
		return "updating agentsctl " + transition + "...", true
	case a.GoAvailable:
		return "agentsctl update available " + transition + ": " + updateCommand + " to update", true
	default:
		return "agentsctl update available " + transition + ": " + selfupdate.ReleasesURL, true
	}
}

// ApplyUpdateAvailable records a newer release found by the update check.
func (s *State) ApplyUpdateAvailable(a selfupdate.Availability) {
	s.UpdateAvailable = &a
}

// handleUpdateCommand resolves prompt as /update when its first token is
// exactly updateCommand (see reservedCommandSpan): handled is then true and
// the prompt never reaches a provider, whatever the outcome. Anything that
// prevents an automatic update is reported through Error and leaves the
// prompt in the composer. The IntentUpdate it returns carries the version the
// notice currently advertises, so the installed version cannot differ from
// the displayed one.
func (s *State) handleUpdateCommand(prompt string) (intent Intent, handled bool) {
	span, ok := reservedCommandSpan(prompt)
	if !ok {
		return Intent{}, false
	}
	runes := []rune(prompt)
	if string(runes[span.start:span.end]) != updateCommand {
		return Intent{}, false
	}
	switch {
	case strings.TrimFunc(string(runes[span.end:]), unicode.IsSpace) != "":
		s.Error = updateCommand + " takes no arguments"
	case s.Updating:
		s.Error = "Update already in progress"
	case s.UpdateAvailable == nil:
		s.Error = "No update available"
	case !s.UpdateAvailable.GoAvailable:
		s.Error = "Cannot update automatically without Go: " + selfupdate.ReleasesURL
	default:
		return Intent{Kind: IntentUpdate, Version: s.UpdateAvailable.Latest}, true
	}
	return Intent{}, true
}
