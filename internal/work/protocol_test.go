package work

import (
	"strings"
	"testing"
)

const testDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestParseProtocolClassification(t *testing.T) {
	states := []State{StateStarting, StateRunning, StateCompleted, StateFailed, StateInterrupted, StateStopped, StateAbandoned, StateSuperseded}
	for _, state := range states {
		status := validTestStatus(state)
		body, err := RenderStatus("visible", status)
		if err != nil {
			t.Fatal(err)
		}
		parsed := Parse(body)
		if parsed.Kind != ValidStatus || *parsed.Status != status || parsed.VisibleBody != "visible" {
			t.Fatalf("state %s: parsed=%+v", state, parsed)
		}
	}

	continuation := CommentID(5_872_981_699)
	cases := []struct {
		name string
		body string
		kind ProtocolKind
	}{
		{"discussion", "ordinary discussion", Discussion},
		{"prompt", RenderPrompt("do the work", Prompt{}), ValidPrompt},
		{"continuation", RenderPrompt("continue", Prompt{Continues: &continuation}), ValidPrompt},
		{"missing required", "<!--\nagentsctl-work\ntype: prompt\n-->", MalformedProtocol},
		{"status missing required", "<!--\nagentsctl-work\ntype: status\nversion: 1\nprompt: 1\n-->", MalformedProtocol},
		{"duplicate key", "<!--\nagentsctl-work\ntype: prompt\ntype: status\nversion: 1\n-->", MalformedProtocol},
		{"malformed id", "<!--\nagentsctl-work\ntype: prompt\nversion: 1\ncontinues: nope\n-->", MalformedProtocol},
		{"unterminated marker", "<!--\nagentsctl-work\ntype: prompt\nversion: 1", MalformedProtocol},
		{"duplicate marker", RenderPrompt("first\n\n<!--\nagentsctl-work\ntype: prompt\nversion: 1\n-->", Prompt{}), MalformedProtocol},
		{"not trailing", RenderPrompt("visible", Prompt{}) + "\nafter", MalformedProtocol},
		{"unsupported", "<!--\nagentsctl-work\ntype: prompt\nversion: 2\n-->", UnsupportedProtocol},
		{"unknown type", "<!--\nagentsctl-work\ntype: future\nversion: 1\n-->", UnknownProtocol},
		{"unknown field", "<!--\nagentsctl-work\ntype: prompt\nversion: 1\nfuture-field: value\n-->", ValidPrompt},
		{"fenced example", "```md\n<!--\nagentsctl-work\ntype: prompt\nversion: 1\n-->\n```", Discussion},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Parse(tc.body).Kind; got != tc.kind {
				t.Fatalf("kind=%v, want %v", got, tc.kind)
			}
		})
	}
}

func TestRenderParseRoundTrip(t *testing.T) {
	continues := CommentID(4_000_000_001)
	prompt := Prompt{Continues: &continues}
	promptParsed := Parse(RenderPrompt("  prompt body\r\n", prompt))
	if promptParsed.Kind != ValidPrompt || promptParsed.VisibleBody != "prompt body" || *promptParsed.Prompt.Continues != continues {
		t.Fatalf("prompt round trip: %+v", promptParsed)
	}

	status := validTestStatus(StateRunning)
	status.Turn = "turn-1"
	rendered, err := RenderStatus("status body", status)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rendered, "<!--\nagentsctl-work\n") {
		t.Fatalf("non-canonical marker: %q", rendered)
	}
	parsed := Parse(rendered)
	if parsed.Kind != ValidStatus || parsed.VisibleBody != "status body" || *parsed.Status != status {
		t.Fatalf("status round trip: %+v", parsed)
	}
}

func validTestStatus(state State) Status {
	return Status{Prompt: 5_872_981_699, Dispatch: "dispatch-1", Host: "host-1", Provider: "codex", Session: "session-1", State: state, PromptDigest: testDigest}
}
