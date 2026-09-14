// Package chatgpt integrates ChatGPT Project conversations through an
// authenticated terminal-browser session.
package chatgpt

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/BurntSushi/toml"
)

const configName = ".agentsctl.toml"

var projectIDPattern = regexp.MustCompile(`^g-p-[A-Za-z0-9_-]+$`)

// Config is the validated local association with one ChatGPT Project.
// Root is the directory containing the applicable .agentsctl.toml and is
// therefore the logical CWD of every conversation returned by the provider.
type Config struct {
	ProjectID string
	Root      string
}

type configFile struct {
	ChatGPT struct {
		ProjectID string `toml:"project_id"`
	} `toml:"chatgpt"`
}

// Discover searches start and its ancestors for the nearest
// .agentsctl.toml. The nearest file establishes the project boundary: when
// it has no [chatgpt] table, ChatGPT is not configured and discovery does
// not inherit a more distant ancestor's association.
//
// The boolean reports whether the nearest file explicitly configures
// ChatGPT. It is also true for an unreadable or malformed nearest file so
// callers can surface the error as an isolated ChatGPT provider warning.
func Discover(start string) (Config, bool, error) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return Config{}, false, fmt.Errorf("resolve config search directory: %w", err)
	}
	for {
		path := filepath.Join(dir, configName)
		contents, readErr := os.ReadFile(path)
		if readErr == nil {
			return decodeConfig(path, contents)
		}
		if !errors.Is(readErr, os.ErrNotExist) {
			return Config{}, true, fmt.Errorf("read %s: %w", path, readErr)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return Config{}, false, nil
		}
		dir = parent
	}
}

func decodeConfig(path string, contents []byte) (Config, bool, error) {
	var file configFile
	metadata, err := toml.Decode(string(contents), &file)
	if err != nil {
		return Config{}, true, fmt.Errorf("parse %s: %w", path, err)
	}
	if !metadata.IsDefined("chatgpt") {
		return Config{}, false, nil
	}
	projectID := strings.TrimSpace(file.ChatGPT.ProjectID)
	if projectID == "" {
		return Config{}, true, fmt.Errorf("%s: [chatgpt].project_id is required", path)
	}
	if !projectIDPattern.MatchString(projectID) {
		return Config{}, true, fmt.Errorf("%s: [chatgpt].project_id must be a g-p-... Project ID", path)
	}
	return Config{ProjectID: projectID, Root: filepath.Dir(path)}, true, nil
}
