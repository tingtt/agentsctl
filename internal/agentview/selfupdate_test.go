package agentview

import (
	"strings"
	"testing"

	"github.com/tingtt/agentsctl/internal/selfupdate"
)

func availableUpdate(goAvailable bool) selfupdate.Availability {
	return selfupdate.Availability{Current: "v1.0.0", Latest: "v1.1.0", GoAvailable: goAvailable}
}

func stateWithUpdate(goAvailable bool) State {
	s := NewState()
	s.ApplyUpdateAvailable(availableUpdate(goAvailable))
	return s
}

func submit(s *State, prompt string) Intent {
	s.Composer.ReplacePrompt(prompt)
	return s.Handle(KeyEvent{Key: KeyEnter})
}

func notificationLine(view string) (string, bool) {
	for _, line := range strings.Split(view, "\n") {
		if strings.HasPrefix(line, "! ") {
			return line, true
		}
	}
	return "", false
}

func TestUpdateIsHighlightedLikeRename(t *testing.T) {
	tests := []struct {
		prompt string
		want   string
	}{
		{"/update", "ccccccc"},
		{"/update ", "ccccccc"},
		{"/update-foo", ""},
		{"/updatefoo", ""},
		{"/update日本語", ""},
		{"/Update", ""},
		{"hello /update", ""},
	}
	for _, tt := range tests {
		t.Run(tt.prompt, func(t *testing.T) {
			span, ok := reservedCommandSpan(tt.prompt)
			if ok != (tt.want != "") {
				t.Fatalf("reservedCommandSpan(%q) ok = %v", tt.prompt, ok)
			}
			rows := composerLines(tt.prompt, len([]rune(tt.prompt)), "", 40)
			got, _ := paintClasses(rows[0])
			got = strings.TrimRight(strings.ReplaceAll(got, "X", "w"), "w")
			if got != tt.want {
				t.Fatalf("classes = %q, want %q (span %v)", got, tt.want, span)
			}
		})
	}
}

func TestUpdateCommandRoutesLocally(t *testing.T) {
	for _, prompt := range []string{"/update", "  /update", "/update\n", "\n/update  "} {
		s := stateWithUpdate(true)
		intent := submit(&s, prompt)
		if intent.Kind != IntentUpdate || intent.Version != "v1.1.0" {
			t.Fatalf("submit(%q) = %+v, want IntentUpdate v1.1.0", prompt, intent)
		}
	}
}

// TestUpdateCommandNeverDispatches covers every reason /update cannot start
// an update: none may fall back to a provider prompt.
func TestUpdateCommandNeverDispatches(t *testing.T) {
	updating := stateWithUpdate(true)
	updating.Updating = true
	tests := []struct {
		name    string
		state   State
		prompt  string
		wantErr string
	}{
		{"arguments", stateWithUpdate(true), "/update foo", "no arguments"},
		{"arguments on later line", stateWithUpdate(true), "/update\nfoo", "no arguments"},
		{"check not finished or nothing newer", NewState(), "/update", "No update available"},
		{"without Go", stateWithUpdate(false), "/update", selfupdate.ReleasesURL},
		{"already updating", updating, "/update", "already in progress"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := tt.state
			intent := submit(&s, tt.prompt)
			if intent.Kind != IntentNone {
				t.Fatalf("intent = %+v, want none (never IntentDispatch)", intent)
			}
			if !strings.Contains(s.Error, tt.wantErr) {
				t.Fatalf("Error = %q, want it to contain %q", s.Error, tt.wantErr)
			}
			if s.Composer.Prompt != tt.prompt {
				t.Fatalf("prompt = %q, want it kept for editing", s.Composer.Prompt)
			}
		})
	}
}

func TestUpdateLookalikesAreProviderPrompts(t *testing.T) {
	for _, prompt := range []string{"/update-foo", "/updates", "please /update", "/Update"} {
		s := stateWithUpdate(true)
		if intent := submit(&s, prompt); intent.Kind != IntentDispatch || intent.Prompt != prompt {
			t.Fatalf("submit(%q) = %+v, want IntentDispatch", prompt, intent)
		}
	}
}

func TestUpdateNoticeText(t *testing.T) {
	tests := []struct {
		name  string
		state func() State
		want  string
	}{
		{"with Go", func() State { return stateWithUpdate(true) },
			"! agentsctl update available (v1.0.0 -> v1.1.0): /update to update"},
		{"without Go", func() State { return stateWithUpdate(false) },
			"! agentsctl update available (v1.0.0 -> v1.1.0): https://github.com/tingtt/agentsctl/releases"},
		{"updating", func() State { s := stateWithUpdate(true); s.Updating = true; return s },
			"! updating agentsctl (v1.0.0 -> v1.1.0)..."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := tt.state()
			got, ok := notificationLine(s.View(120, 30))
			if !ok || strings.TrimRight(got, " ") != tt.want {
				t.Fatalf("notification = %q, %v; want %q", got, ok, tt.want)
			}
		})
	}
}

func TestNoUpdateNoticeWithoutAvailability(t *testing.T) {
	s := NewState()
	if line, ok := notificationLine(s.View(120, 30)); ok {
		t.Fatalf("unexpected notification %q", line)
	}
}

// TestErrorTakesPriorityOverUpdateNoticeWithoutDiscardingIt fixes the
// notification area priority: Error > update notice > nothing.
func TestErrorTakesPriorityOverUpdateNoticeWithoutDiscardingIt(t *testing.T) {
	s := stateWithUpdate(true)
	s.Error = "No session selected"
	line, _ := notificationLine(s.View(120, 30))
	if !strings.Contains(line, "No session selected") || strings.Contains(s.View(120, 30), "update available") {
		t.Fatalf("error must hide the update notice, got %q", line)
	}

	s.Error = ""
	line, ok := notificationLine(s.View(120, 30))
	if !ok || !strings.Contains(line, "update available (v1.0.0 -> v1.1.0): /update to update") {
		t.Fatalf("update notice must reappear once Error is cleared, got %q", line)
	}
}
