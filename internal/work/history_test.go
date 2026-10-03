package work

import (
	"fmt"
	"math/rand"
	"reflect"
	"testing"
	"time"
)

func TestReconstructDeterministicMixedIssue(t *testing.T) {
	trusted := Trust{Trusted: true, Reason: TrustEstablished}
	untrusted := Trust{Reason: TrustLoginNotAllowed}
	interrupted := validTestStatus(StateInterrupted)
	interrupted.Prompt = 10
	comments := []Comment{
		{ID: 1, Body: "discussion", Trust: trusted},
		{ID: 10, Body: RenderPrompt("root", Prompt{}), Trust: trusted},
		{ID: 11, Body: RenderPrompt("untouched", Prompt{}), Trust: trusted},
		{ID: 12, Body: RenderPrompt("untrusted normal", Prompt{}), Trust: untrusted},
		statusComment(20, interrupted, trusted),
		statusComment(21, statusForPrompt(11, StateSuperseded), trusted),
		statusComment(22, statusForPrompt(999, StateCompleted), trusted),
		{ID: 23, Body: unsupportedStatusBody(11), Trust: trusted},
		{ID: 24, Body: "<!--\nagentsctl-work\ntype: prompt\nversion: 1\ncontinues: nope\n-->", Trust: trusted},
		statusComment(25, statusForPrompt(12, StateCompleted), untrusted),
		continuationComment(30, 20, trusted),
		continuationComment(31, 20, trusted),
		continuationComment(32, 20, untrusted),
	}

	want := Reconstruct(comments)
	for seed := int64(0); seed < 10; seed++ {
		gotInput := append([]Comment(nil), comments...)
		rand.New(rand.NewSource(seed)).Shuffle(len(gotInput), func(i, j int) { gotInput[i], gotInput[j] = gotInput[j], gotInput[i] })
		got := Reconstruct(gotInput)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("seed %d produced order-dependent history\ngot=%+v\nwant=%+v", seed, got, want)
		}
	}

	byID := promptMap(want.Prompts)
	if byID[10].Pending || byID[10].Eligible {
		t.Fatalf("dispatched root state=%+v", byID[10])
	}
	if !byID[11].BlockedByUnsupported || byID[11].Eligible {
		t.Fatalf("unsupported status must block=%+v", byID[11])
	}
	if !byID[12].Eligible || byID[12].AutoTrusted {
		t.Fatalf("untrusted normal Prompt should remain manually eligible=%+v", byID[12])
	}
	if !byID[30].Eligible || !byID[30].CanonicalContinuation {
		t.Fatalf("smallest trusted continuation must be canonical=%+v", byID[30])
	}
	if byID[31].Eligible || byID[31].CanonicalContinuation || byID[32].Eligible {
		t.Fatalf("duplicate continuations must be ineligible: 31=%+v 32=%+v", byID[31], byID[32])
	}
}

func TestEligibilityCases(t *testing.T) {
	trusted := Trust{Trusted: true, Reason: TrustEstablished}
	interrupted := statusForPrompt(1, StateInterrupted)
	cases := []struct {
		name     string
		comments []Comment
		prompt   CommentID
		eligible bool
	}{
		{"untouched", []Comment{{ID: 1, Body: RenderPrompt("p", Prompt{}), Trust: trusted}}, 1, true},
		{"completed", []Comment{{ID: 1, Body: RenderPrompt("p", Prompt{}), Trust: trusted}, statusComment(2, statusForPrompt(1, StateCompleted), trusted)}, 1, false},
		{"active", []Comment{{ID: 1, Body: RenderPrompt("p", Prompt{}), Trust: trusted}, statusComment(2, statusForPrompt(1, StateRunning), trusted)}, 1, false},
		{"unsupported status", []Comment{{ID: 1, Body: RenderPrompt("p", Prompt{}), Trust: trusted}, {ID: 2, Body: unsupportedStatusBody(1), Trust: trusted}}, 1, false},
		{"canonical continuation", []Comment{{ID: 1, Body: RenderPrompt("p", Prompt{}), Trust: trusted}, statusComment(2, interrupted, trusted), continuationComment(3, 2, trusted)}, 3, true},
		{"duplicate continuation", []Comment{{ID: 1, Body: RenderPrompt("p", Prompt{}), Trust: trusted}, statusComment(2, interrupted, trusted), continuationComment(3, 2, trusted), continuationComment(4, 2, trusted)}, 4, false},
		{"invalid continuation", []Comment{continuationComment(3, 999, trusted)}, 3, false},
		{"untrusted continuation target", []Comment{{ID: 1, Body: RenderPrompt("p", Prompt{}), Trust: trusted}, statusComment(2, interrupted, Trust{Reason: TrustLoginNotAllowed}), continuationComment(3, 2, trusted)}, 3, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := promptMap(Reconstruct(tc.comments).Prompts)[tc.prompt].Eligible
			if got != tc.eligible {
				t.Fatalf("eligible=%v, want %v", got, tc.eligible)
			}
		})
	}
}

func TestContinuationValidityIsSeparateFromTrust(t *testing.T) {
	trusted := Trust{Trusted: true, Reason: TrustEstablished}
	comments := []Comment{
		{ID: 1, Body: RenderPrompt("p", Prompt{}), Trust: trusted},
		statusComment(2, statusForPrompt(1, StateInterrupted), Trust{Reason: TrustLoginNotAllowed}),
		continuationComment(3, 2, trusted),
	}
	prompt := promptMap(Reconstruct(comments).Prompts)[3]
	if !prompt.ContinuationValid || prompt.ContinuationTrusted || prompt.Eligible {
		t.Fatalf("continuation relation/trust were conflated: %+v", prompt)
	}
}

func statusForPrompt(prompt CommentID, state State) Status {
	status := validTestStatus(state)
	status.Prompt = prompt
	return status
}

func statusComment(id CommentID, status Status, trust Trust) Comment {
	body, err := RenderStatus("status", status)
	if err != nil {
		panic(err)
	}
	return Comment{ID: id, Body: body, Trust: trust, CreatedAt: time.Unix(int64(id), 0)}
}

func continuationComment(id, target CommentID, trust Trust) Comment {
	return Comment{ID: id, Body: RenderPrompt("continue", Prompt{Continues: &target}), Trust: trust}
}

func unsupportedStatusBody(prompt CommentID) string {
	return "<!--\nagentsctl-work\ntype: status\nversion: 2\nprompt: " + stringID(prompt) + "\n-->"
}

func stringID(id CommentID) string {
	return fmt.Sprintf("%d", id)
}

func promptMap(prompts []PromptHistory) map[CommentID]PromptHistory {
	result := make(map[CommentID]PromptHistory, len(prompts))
	for _, prompt := range prompts {
		result[prompt.Comment.ID] = prompt
	}
	return result
}
