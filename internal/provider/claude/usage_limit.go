//go:build darwin || linux

package claude

import (
	"os"
	"strings"
	"sync"
)

// probeLimitSignal is the Claude provider boundary's classification of a
// probe session's terminal output into per-window usage-limit evidence
// (see classifyProbeOutput). The two fields are independent: a single
// probe attempt can report the 5h window, the weekly window, or both as
// exhausted.
type probeLimitSignal struct {
	FiveHour bool
	Weekly   bool
}

// any reports whether either window was classified as exhausted.
func (s probeLimitSignal) any() bool { return s.FiveHour || s.Weekly }

// weeklyLimitPhrases and fiveHourLimitPhrases are lowercase substrings
// that, found in a probe session's terminal output, positively identify
// which rate-limit window Claude Code is reporting as exhausted. Each
// entry is a specific multi-word clause -- a qualifier plus "limit", or a
// full "hit/reached your <window> limit" clause -- never the bare word
// "limit" on its own: the installed CLI (2.1.263) also uses "limit" for
// several unrelated concepts this package must never misclassify as a
// rate limit (context-window compaction's "Context limit reached", tool
// output truncation's "output limit", model context's "token limit" --
// none of which contain any phrase below).
//
// "hit your session limit" / "hit your weekly limit" (and the "reached
// your ..." variants) are NOT confirmed against a live limit event --
// reproducing one would require deliberately exhausting a real quota,
// which was avoided (see the PR description's "Claude behavior observed"
// section). They are inferred from a confirmed real pattern: `strings` on
// the installed CLI binary shows a template array of message prefixes
// ("You've hit your", "You've reached your") combined elsewhere with
// limit-type suffixes -- "session limit", "weekly limit", "fast limit",
// and "monthly spend limit" are all attested as suffixes used with that
// same prefix family (e.g. the fully-formed, directly-embedded "You've
// hit your monthly spend limit." proves the template composes this way;
// "session limit"/"weekly limit" appear as bare fragments in the same
// binary, matching the same "<kind> limit" shape). Bare "session limit"
// or "weekly limit" alone are deliberately NOT used as standalone
// triggers: the same binary also embeds unrelated messages containing
// those exact words (an upsell nudge, "for higher session limits every
// month", and the "lower-priority mode" flow's "Reset your session limit
// now" / "Couldn't reset your session limit right now") that must not be
// misread as an active exhaustion notice -- requiring the "hit your"/
// "reached your" clause excludes all of those.
var weeklyLimitPhrases = []string{
	"hit your weekly limit",
	"reached your weekly limit",
	"reached your weekly usage limit", // compat: older "you have reached your weekly usage limit" wording, also embedded in the same binary.
	"weekly usage limit",
}
var fiveHourLimitPhrases = []string{
	"hit your session limit",
	"reached your session limit",
	"5-hour",
	"five-hour",
}

// genericLimitPhrases are older/compat wordings that don't specify which
// window was hit -- see classifyProbeOutput's doc comment for why an
// unqualified match here is treated as the 5-hour/session window.
var genericLimitPhrases = []string{
	"usage limit reached",
	"usage limit",
}

// classifyProbeOutput scans a probe session's raw terminal output for
// Claude Code's own usage-limit wording, confined entirely to this
// package so no normalized-state consumer (Agent View, #20) ever needs to
// interpret Claude-specific display text (see the DesignDoc's Usage
// capability and Issue #19's "限定判定は表示文言への ad-hoc な依存を最小限にし").
// See weeklyLimitPhrases/fiveHourLimitPhrases/genericLimitPhrases for the
// exact phrases and what evidence each is based on. An unqualified
// generic match (neither weekly- nor 5h-specific wording present) is
// treated as the 5-hour/session window, matching the CLI's own pattern of
// only spelling out "weekly" when that is the window actually hit. It is
// exercised only by this package's own regression tests, which drive this
// function directly against fixed strings, and by the fake-CLI-driven
// probe tests (see the extended fake CLI's limit_banner.txt fixture).
func classifyProbeOutput(output string) probeLimitSignal {
	lower := strings.ToLower(output)
	weekly := containsAny(lower, weeklyLimitPhrases)
	fiveHour := containsAny(lower, fiveHourLimitPhrases)
	if !weekly && !fiveHour && containsAny(lower, genericLimitPhrases) {
		fiveHour = true
	}
	return probeLimitSignal{FiveHour: fiveHour, Weekly: weekly}
}

// containsAny reports whether s contains any of phrases.
func containsAny(s string, phrases []string) bool {
	for _, p := range phrases {
		if strings.Contains(s, p) {
			return true
		}
	}
	return false
}

// probeOutputCaptureMax bounds probeOutputCapture's retained bytes: only
// the most recent output matters (a limit banner, if any, appears near
// the point refresh gives up waiting), so a long-settling probe session
// can never grow this without bound.
const probeOutputCaptureMax = 16 * 1024

// probeOutputCapture is a bounded, concurrency-safe accumulator for a
// probe session's raw terminal output -- written continuously by
// captureUntilClosed while Probe.refreshOnce's own wait loop repeatedly
// inspects it via classifyProbeOutput.
type probeOutputCapture struct {
	mu  sync.Mutex
	buf []byte
}

func (c *probeOutputCapture) write(p []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.buf = append(c.buf, p...)
	if len(c.buf) > probeOutputCaptureMax {
		c.buf = c.buf[len(c.buf)-probeOutputCaptureMax:]
	}
}

func (c *probeOutputCapture) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return string(c.buf)
}

// captureUntilClosed reads f until EOF/error, accumulating into capture
// instead of discarding -- mirrors drainUntilClosed's "must keep reading
// or the child blocks on its own writes" role for every other PTY-
// attached transient session in this package that has no real terminal
// watching it, just keeping what it reads instead of throwing it away.
func captureUntilClosed(f *os.File, capture *probeOutputCapture) {
	buf := make([]byte, 4096)
	for {
		n, err := f.Read(buf)
		if n > 0 {
			capture.write(buf[:n])
		}
		if err != nil {
			return
		}
	}
}
