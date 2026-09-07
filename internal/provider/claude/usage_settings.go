package claude

import (
	"encoding/json"
	"strings"
)

// usageProbeRefreshIntervalSeconds is the probe's dedicated statusLine
// refreshInterval (documented minimum: 1 second) -- it re-invokes the
// collector on a fixed timer in addition to Claude Code's own event-driven
// triggers, so a snapshot keeps getting re-observed (with a fresh
// ObservedAt) even during a quiet stretch with no new API response. It
// does not, by itself, cause a new API request -- only Probe.refresh
// writing a fresh prompt into the session does that (see
// usage_probe_unix.go) -- so this is cheap local bookkeeping, not an
// additional quota cost.
const usageProbeRefreshIntervalSeconds = 60

// usageSettings is the shape of the dedicated settings.json this package
// writes for its own probe session -- a small, explicit subset of Claude
// Code's full settings schema (see the installed CLI's own --settings
// documentation): only statusLine, pointed at this package's own collector
// subcommand. It is written to a file under this package's own app-data
// directory and passed via `claude --settings <path>`, never merged into
// or read from the user's own ~/.claude/settings.json.
type usageSettings struct {
	StatusLine usageStatusLineSetting `json:"statusLine"`
}
type usageStatusLineSetting struct {
	Type            string `json:"type"`
	Command         string `json:"command"`
	RefreshInterval int    `json:"refreshInterval"`
}

// writeUsageSettings (re)writes the probe's dedicated settings.json at
// settingsPath, pointing its statusLine at this same agentsctl executable
// re-invoked as the UsageCollectorCommand hidden subcommand (see
// usage_collector.go), writing to snapshotPath. It is idempotent and cheap
// enough to call on every refresh (see Probe.refresh): the content is a
// deterministic function of exePath/snapshotPath, so a redundant rewrite
// with unchanged inputs produces byte-identical output.
func writeUsageSettings(settingsPath, exePath, snapshotPath string) error {
	command := shellQuote(exePath) + " " + UsageCollectorCommand + " --out " + shellQuote(snapshotPath)
	settings := usageSettings{StatusLine: usageStatusLineSetting{
		Type:            "command",
		Command:         command,
		RefreshInterval: usageProbeRefreshIntervalSeconds,
	}}
	b, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(settingsPath, b)
}

// shellQuote wraps value in single quotes for safe inclusion in the shell
// command line Claude Code's statusLine `command` field runs (the CLI's
// own docs: "The command field runs in a shell") -- exePath/snapshotPath
// both come from this package's own app-data directory resolution, never
// user input, but are quoted anyway since a path can still legitimately
// contain spaces.
func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}
