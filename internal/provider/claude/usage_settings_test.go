package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWriteUsageSettingsPointsStatusLineAtCollector fixes the generated
// settings.json shape: a "command"-type statusLine whose command re-
// invokes the given executable as the UsageCollectorCommand hidden
// subcommand, writing to the given snapshot path -- and never touches
// anything else (in particular, it must be a small, standalone file, not
// merged with any other setting).
func TestWriteUsageSettingsPointsStatusLineAtCollector(t *testing.T) {
	dir := t.TempDir()
	settingsPath := filepath.Join(dir, "settings.json")
	exePath := filepath.Join(dir, "agentsctl")
	snapshotPath := filepath.Join(dir, "usage.json")
	if err := writeUsageSettings(settingsPath, exePath, snapshotPath); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	var got usageSettings
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.StatusLine.Type != "command" {
		t.Fatalf("StatusLine.Type=%q, want %q", got.StatusLine.Type, "command")
	}
	if !strings.Contains(got.StatusLine.Command, exePath) {
		t.Fatalf("command=%q, want it to reference the executable %q", got.StatusLine.Command, exePath)
	}
	if !strings.Contains(got.StatusLine.Command, UsageCollectorCommand) {
		t.Fatalf("command=%q, want it to invoke %q", got.StatusLine.Command, UsageCollectorCommand)
	}
	if !strings.Contains(got.StatusLine.Command, snapshotPath) {
		t.Fatalf("command=%q, want it to reference the snapshot path %q", got.StatusLine.Command, snapshotPath)
	}
	if got.StatusLine.RefreshInterval <= 0 {
		t.Fatalf("RefreshInterval=%d, want a positive refresh interval so idle sessions still re-observe", got.StatusLine.RefreshInterval)
	}
}

// TestWriteUsageSettingsQuotesPathsWithSpaces fixes that a path containing
// a space (a plausible app-data directory on some systems) survives the
// shell command line intact rather than being split into extra arguments.
func TestWriteUsageSettingsQuotesPathsWithSpaces(t *testing.T) {
	dir := t.TempDir()
	settingsPath := filepath.Join(dir, "settings.json")
	exePath := filepath.Join(dir, "Application Support", "agentsctl")
	snapshotPath := filepath.Join(dir, "Application Support", "usage.json")
	if err := writeUsageSettings(settingsPath, exePath, snapshotPath); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	var got usageSettings
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.StatusLine.Command, "'"+exePath+"'") {
		t.Fatalf("command=%q, want the space-containing exe path single-quoted", got.StatusLine.Command)
	}
}
