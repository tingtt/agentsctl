//go:build darwin || linux

package claude

import "testing"

// TestClassifyProbeOutputWeeklyQualifiedText fixes that wording explicitly
// naming "weekly" (confirmed embedded in the installed CLI binary, see
// classifyProbeOutput's doc comment) classifies as the weekly window only.
func TestClassifyProbeOutputWeeklyQualifiedText(t *testing.T) {
	sig := classifyProbeOutput("You have reached your weekly usage limit.")
	if !sig.Weekly || sig.FiveHour {
		t.Fatalf("sig=%+v, want Weekly only", sig)
	}
}

// TestClassifyProbeOutputFiveHourQualifiedText fixes that wording
// explicitly naming the 5-hour/session window classifies as 5h only.
func TestClassifyProbeOutputFiveHourQualifiedText(t *testing.T) {
	sig := classifyProbeOutput("Your 5-hour usage limit has been reached.")
	if !sig.FiveHour || sig.Weekly {
		t.Fatalf("sig=%+v, want FiveHour only", sig)
	}
}

// TestClassifyProbeOutputUnqualifiedTextIsFiveHour fixes the fallback rule:
// a bare "Usage limit reached" with no window-qualifying word is treated
// as the 5-hour/session window, never weekly and never both -- see
// classifyProbeOutput's doc comment for why (the CLI only spells out
// "weekly" when that is the window actually hit).
func TestClassifyProbeOutputUnqualifiedTextIsFiveHour(t *testing.T) {
	sig := classifyProbeOutput("Usage limit reached\r\n")
	if !sig.FiveHour || sig.Weekly {
		t.Fatalf("sig=%+v, want FiveHour only for unqualified text", sig)
	}
}

// TestClassifyProbeOutputBothQualifiersReportsBoth fixes that text
// mentioning both a weekly and a 5-hour/session qualifier reports both
// windows exhausted, not just one.
func TestClassifyProbeOutputBothQualifiersReportsBoth(t *testing.T) {
	sig := classifyProbeOutput("Usage limit reached: your 5-hour session limit and your weekly usage limit have both been reached.")
	if !sig.FiveHour || !sig.Weekly {
		t.Fatalf("sig=%+v, want both FiveHour and Weekly", sig)
	}
}

// TestClassifyProbeOutputHitsYourSessionLimitBanner fixes the real-shaped
// banner text this classifier previously missed entirely (it required a
// "usage limit" substring, which this text never contains): "You've hit
// your session limit · resets 3pm" classifies as the 5-hour/session
// window only. See fiveHourLimitPhrases' doc comment for the evidence
// behind this specific phrase -- not confirmed against a live limit
// event.
func TestClassifyProbeOutputHitsYourSessionLimitBanner(t *testing.T) {
	sig := classifyProbeOutput("You've hit your session limit · resets 3pm")
	if !sig.FiveHour || sig.Weekly {
		t.Fatalf("sig=%+v, want FiveHour only", sig)
	}
}

// TestClassifyProbeOutputHitsYourWeeklyLimitBanner is the weekly-window
// counterpart of TestClassifyProbeOutputHitsYourSessionLimitBanner.
func TestClassifyProbeOutputHitsYourWeeklyLimitBanner(t *testing.T) {
	sig := classifyProbeOutput("You've hit your weekly limit · resets Sep 10")
	if !sig.Weekly || sig.FiveHour {
		t.Fatalf("sig=%+v, want Weekly only", sig)
	}
}

// TestClassifyProbeOutputRejectsUnrelatedLimitWording fixes the false-
// positive risk explicitly guarded against in weeklyLimitPhrases'/
// fiveHourLimitPhrases' doc comments: the bare word "limit" is never
// enough, and phrases that merely contain the substrings "session limit"
// or "weekly limit" without a "hit/reached your" clause (an upsell nudge,
// or the lower-priority-mode reset flow, both confirmed embedded in the
// installed CLI binary) must not be misread as an active exhaustion
// notice, on top of entirely unrelated non-rate-limit uses of the word.
func TestClassifyProbeOutputRejectsUnrelatedLimitWording(t *testing.T) {
	cases := []string{
		"Context limit reached, compacting conversation.",
		"You've exceeded the token limit for this request.",
		"Tool output limit exceeded; truncating result.",
		"for higher session limits every month, upgrade your plan",
		"Reset your session limit now and keep working",
		"Couldn't reset your session limit right now",
	}
	for _, text := range cases {
		if sig := classifyProbeOutput(text); sig.any() {
			t.Fatalf("classifyProbeOutput(%q) = %+v, want no signal for unrelated limit wording", text, sig)
		}
	}
}

// TestClassifyProbeOutputNoLimitWordingReportsNoSignal fixes that ordinary
// probe output (a normal chat reply, or this package's own attach banner)
// never spuriously classifies as a limit.
func TestClassifyProbeOutputNoLimitWordingReportsNoSignal(t *testing.T) {
	sig := classifyProbeOutput("FAKE CLAUDE PROBE ATTACHED\r\nOK\r\n")
	if sig.any() {
		t.Fatalf("sig=%+v, want no signal for ordinary output", sig)
	}
}

// TestClassifyProbeOutputIsCaseInsensitive fixes that matching does not
// depend on the exact casing Claude Code happens to render.
func TestClassifyProbeOutputIsCaseInsensitive(t *testing.T) {
	sig := classifyProbeOutput("USAGE LIMIT REACHED")
	if !sig.any() {
		t.Fatal("classifyProbeOutput must be case-insensitive")
	}
}

// TestProbeOutputCaptureBoundsRetainedBytes fixes that a long-running
// probe session's output can never grow the capture buffer without bound
// -- only the most recent probeOutputCaptureMax bytes are kept.
func TestProbeOutputCaptureBoundsRetainedBytes(t *testing.T) {
	c := &probeOutputCapture{}
	chunk := make([]byte, 1024)
	for i := range chunk {
		chunk[i] = 'a'
	}
	for i := 0; i < probeOutputCaptureMax/len(chunk)+4; i++ {
		c.write(chunk)
	}
	if got := len(c.String()); got > probeOutputCaptureMax {
		t.Fatalf("captured %d bytes, want at most %d", got, probeOutputCaptureMax)
	}
}

// TestProbeOutputCaptureRetainsMostRecentBytes fixes that bounding trims
// from the front, not the back: a marker written last must still be
// present after the buffer has been driven well past its cap.
func TestProbeOutputCaptureRetainsMostRecentBytes(t *testing.T) {
	c := &probeOutputCapture{}
	filler := make([]byte, 1024)
	for i := 0; i < probeOutputCaptureMax/len(filler)+4; i++ {
		c.write(filler)
	}
	c.write([]byte("usage limit reached"))
	got := classifyProbeOutput(c.String())
	if !got.any() {
		t.Fatal("a marker written after filling the capture past its cap must still be observable")
	}
}
