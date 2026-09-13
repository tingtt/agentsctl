package chatgpt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDiscoverReturnsUnconfiguredWhenNoConfigExists(t *testing.T) {
	config, configured, err := Discover(t.TempDir())
	if err != nil || configured || config != (Config{}) {
		t.Fatalf("config=%+v configured=%t err=%v", config, configured, err)
	}
}

func TestDiscoverUsesNearestConfigDirectoryAsLogicalCWD(t *testing.T) {
	root := t.TempDir()
	writeConfig(t, root, "[chatgpt]\nproject_id = \"g-p-project_1\"\n")
	child := filepath.Join(root, "src", "child")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}

	config, configured, err := Discover(child)
	if err != nil {
		t.Fatal(err)
	}
	if !configured || config.ProjectID != "g-p-project_1" || config.Root != root {
		t.Fatalf("config=%+v configured=%t", config, configured)
	}
}

func TestDiscoverRejectsMissingOrInvalidProjectID(t *testing.T) {
	for _, contents := range []string{
		"[chatgpt]\n",
		"[chatgpt]\nproject_id = \"   \"\n",
		"[chatgpt]\nproject_id = \"project name\"\n",
	} {
		t.Run(strings.ReplaceAll(contents, "\n", "_"), func(t *testing.T) {
			root := t.TempDir()
			writeConfig(t, root, contents)
			_, configured, err := Discover(root)
			if !configured || err == nil || !strings.Contains(err.Error(), "project_id") {
				t.Fatalf("configured=%t err=%v", configured, err)
			}
		})
	}
}

func TestDiscoverNearestConfigDoesNotInheritParentChatGPTSection(t *testing.T) {
	root := t.TempDir()
	writeConfig(t, root, "[chatgpt]\nproject_id = \"g-p-parent\"\n")
	child := filepath.Join(root, "child")
	if err := os.Mkdir(child, 0o755); err != nil {
		t.Fatal(err)
	}
	writeConfig(t, child, "[other]\nenabled = true\n")

	config, configured, err := Discover(child)
	if err != nil || configured || config != (Config{}) {
		t.Fatalf("config=%+v configured=%t err=%v", config, configured, err)
	}
}

func TestDiscoverReportsMalformedNearestConfigAsConfiguredError(t *testing.T) {
	root := t.TempDir()
	writeConfig(t, root, "[chatgpt\n")
	_, configured, err := Discover(root)
	if !configured || err == nil || !strings.Contains(err.Error(), "parse") {
		t.Fatalf("configured=%t err=%v", configured, err)
	}
}

func writeConfig(t *testing.T, dir, contents string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, configName), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}
