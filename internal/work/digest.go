package work

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
)

// Snapshot canonicalizes a visible Prompt body and returns its stable digest.
func Snapshot(visibleBody string) (body, digest string) {
	body = strings.TrimSpace(strings.ReplaceAll(visibleBody, "\r\n", "\n"))
	sum := sha256.Sum256([]byte(body))
	return body, "sha256:" + hex.EncodeToString(sum[:])
}

// PromptSnapshot parses a Prompt comment and snapshots only its visible body.
func PromptSnapshot(commentBody string) (body, digest string, err error) {
	parsed := Parse(commentBody)
	if parsed.Kind != ValidPrompt {
		if parsed.Err != nil {
			return "", "", parsed.Err
		}
		return "", "", errors.New("comment is not a valid Prompt")
	}
	body, digest = Snapshot(parsed.VisibleBody)
	return body, digest, nil
}
