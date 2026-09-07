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

// classifyProbeOutput scans a probe session's raw terminal output for
// Claude Code's own usage-limit wording, confined entirely to this
// package so no normalized-state consumer (Agent View, #20) ever needs to
// interpret Claude-specific display text (see the DesignDoc's Usage
// capability and Issue #19's "限定判定は表示文言への ad-hoc な依存を最小限にし").
//
// The phrases matched below are confirmed present as embedded UI strings
// in the installed CLI binary (2.1.263, via `strings <binary>`): a bare
// "Usage limit reached" alongside distinctly weekly-qualified wording
// ("you have reached your weekly usage limit") -- the CLI only spells out
// "weekly" when that is the window actually hit, so an unqualified
// message is treated as the shorter 5-hour/session window. This mapping
// was NOT observed against a live limit event: reproducing one would have
// required deliberately exhausting a real quota, which was avoided (see
// the PR description's "Claude behavior observed" section). It is
// exercised only by this package's own regression tests, which drive this
// function directly against fixed strings, and by the fake-CLI-driven
// probe tests (see the extended fake CLI's limit_banner.txt fixture).
func classifyProbeOutput(output string) probeLimitSignal {
	lower := strings.ToLower(output)
	if !strings.Contains(lower, "usage limit") {
		return probeLimitSignal{}
	}
	weekly := strings.Contains(lower, "weekly")
	fiveHour := strings.Contains(lower, "5-hour") || strings.Contains(lower, "five-hour") || strings.Contains(lower, "session limit")
	if !weekly && !fiveHour {
		// An unqualified "Usage limit reached" -- see this function's doc
		// comment for why that is treated as the 5-hour/session window.
		fiveHour = true
	}
	return probeLimitSignal{FiveHour: fiveHour, Weekly: weekly}
}

// probeOutputCaptureMax bounds probeOutputCapture's retained bytes: only
// the most recent output matters (a limit banner, if any, appears near
// the point refresh gives up waiting), so a long-settling probe session
// can never grow this without bound.
const probeOutputCaptureMax = 16 * 1024

// probeOutputCapture is a bounded, concurrency-safe accumulator for a
// probe session's raw terminal output -- written continuously by
// captureUntilClosed while Probe.refresh's own wait loop repeatedly
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
