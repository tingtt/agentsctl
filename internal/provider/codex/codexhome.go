package codex

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// resolveCodexHome locates the Codex home directory the way the Codex CLI
// itself does (find_codex_home): a non-empty CODEX_HOME must name an
// existing directory and is canonicalized; otherwise it is ~/.codex, which
// need not exist yet. getenv and userHome are injected so the resolution is
// testable without touching the real environment.
func resolveCodexHome(getenv func(string) string, userHome func() (string, error)) (string, error) {
	if dir := getenv("CODEX_HOME"); dir != "" {
		info, err := os.Stat(dir)
		if err != nil {
			return "", fmt.Errorf("CODEX_HOME %q: %w", dir, err)
		}
		if !info.IsDir() {
			return "", fmt.Errorf("CODEX_HOME %q is not a directory", dir)
		}
		abs, err := filepath.Abs(dir)
		if err != nil {
			return "", err
		}
		return filepath.EvalSymlinks(abs)
	}
	home, err := userHome()
	if err != nil {
		return "", err
	}
	if home == "" {
		return "", errors.New("user home directory is unknown")
	}
	return filepath.Join(home, ".codex"), nil
}

// controlSocketPath is the shared app-server daemon's default control
// socket under codexHome.
func controlSocketPath(codexHome string) string {
	return filepath.Join(codexHome, "app-server-control", "app-server-control.sock")
}

// writerLockPath is the thread writer-lock file Codex keeps under
// codexHome for threadID.
func writerLockPath(codexHome, threadID string) string {
	return filepath.Join(codexHome, "thread-writer-locks", threadID+".lock")
}
