package claude

import (
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/tingtt/agentsctl/internal/provider/claude/probestate"
)

// UsageCollectorCommand is the hidden subcommand name cmd/agentsctl/main.go
// dispatches to RunUsageCollector -- the same pattern main.go already uses
// for "daemon" (the Codex supervisor's own re-exec). The dedicated Claude
// probe session's statusLine setting (see writeUsageSettings) points at
// "<agentsctl executable> UsageCollectorCommand --out <path>", so this is
// the entrypoint Claude Code itself invokes, on the probe's own machine,
// every time its statusLine updates.
const UsageCollectorCommand = "claude-usage-collect"

// RunUsageCollector is a Claude Code statusLine command: it reads exactly
// one JSON payload from stdin (see statusLinePayload), extracts the
// rate-limit windows it carries, and atomically persists them to the path
// named by --out (see writeUsageSnapshotAtomic) -- never anything else
// from the payload, and never the raw payload itself (Claude-specific JSON
// shape stays inside this package, never reaching internal/session -- see
// usage.go's toSessionUsage). It intentionally prints nothing of
// consequence to stdout: this statusLine is never actually displayed on a
// real terminal (the probe session's PTY has no human viewer), only
// invoked for its stdin payload.
func RunUsageCollector(args []string, stdin io.Reader) error {
	fs := flag.NewFlagSet(UsageCollectorCommand, flag.ContinueOnError)
	out := fs.String("out", "", "path to write the usage snapshot to")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *out == "" {
		return errors.New(UsageCollectorCommand + ": --out is required")
	}
	raw, err := io.ReadAll(stdin)
	if err != nil {
		return fmt.Errorf("%s: read stdin: %w", UsageCollectorCommand, err)
	}
	snap, err := parseStatusLinePayload(raw)
	if err != nil {
		return fmt.Errorf("%s: parse statusLine payload: %w", UsageCollectorCommand, err)
	}
	if err := probestate.NewSnapshotStore(*out).Save(snap); err != nil {
		return fmt.Errorf("%s: write snapshot: %w", UsageCollectorCommand, err)
	}
	return nil
}
