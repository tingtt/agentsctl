package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// usageProbeRefreshIntervalSeconds is the probe's dedicated statusLine
// refreshInterval (documented minimum: 1 second) -- it re-invokes the
// collector on a fixed timer in addition to Claude Code's own event-driven
// triggers, so a snapshot keeps getting re-observed (with a fresh
// ObservedAt) even during a quiet stretch with no new API response. It
// does not, by itself, cause a new API request -- only Probe.refreshOnce
// writing a fresh prompt into the session does that (see
// usage_probe_unix.go) -- so this is cheap local bookkeeping, not an
// additional quota cost.
//
// Kept short (rather than e.g. 60s) based on real-CLI measurement:
// waitForFreshSnapshot needs the collector to actually run again after the
// probe's own prompt gets its API response, and relying solely on Claude
// Code's event-driven triggers to do that promptly was not reliably
// observed within a bounded window in testing -- a short timer interval is
// what actually got the post-response snapshot collected quickly and
// consistently.
const usageProbeRefreshIntervalSeconds = 5

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
// enough to call on every refresh (see Probe.refreshOnce): the content is a
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
	return writeSettingsFileAtomic(settingsPath, b)
}

// writeSettingsFileAtomic writes data to path via a temp-file-plus-rename
// swap, so a concurrent reader never observes a partially-written
// settings.json. This package's other persisted state (the probe identity
// and usage snapshot) has its own copy of this same pattern inside
// probestate, which owns those files' mutation authority; settings.json
// carries no such ownership concern (it is rewritten idempotently on every
// refresh attempt, never read-modify-written), so it keeps this small,
// self-contained helper rather than reaching into probestate for it.
func writeSettingsFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
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
