package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

// fingerprint returns a 12-hex-character SHA-256 digest of a raw ID, so diagnostics and
// logs never carry a raw Agents API session/turn ID or ChatGPT conversation ID.
func fingerprint(rawID string) string {
	sum := sha256.Sum256([]byte(rawID))
	return hex.EncodeToString(sum[:])[:12]
}

// diffSessionIDs returns the IDs present in after but not in before, in after's order.
func diffSessionIDs(before, after []string) []string {
	seen := make(map[string]bool, len(before))
	for _, id := range before {
		seen[id] = true
	}
	var newIDs []string
	for _, id := range after {
		if !seen[id] {
			newIDs = append(newIDs, id)
		}
	}
	return newIDs
}

// controlledCandidate resolves a single, unambiguous new-session candidate from a
// before/after ID diff. It refuses to guess: any count other than exactly one new
// session is NOT VERIFIED, per the task's requirement that a same-time correlation with
// more than one new session is insufficient identity evidence.
func controlledCandidate(before, after []string) (id string, ok bool) {
	newIDs := diffSessionIDs(before, after)
	if len(newIDs) != 1 {
		return "", false
	}
	return newIDs[0], true
}

// accumulatePages merges cursor-paginated ID pages in the order supplied, and reports
// whether any ID appeared in more than one page (a duplicate would indicate the cursor
// did not advance, or advanced but revisited an entry).
func accumulatePages(pages [][]string) (all []string, hasDuplicates bool) {
	seen := make(map[string]bool)
	for _, page := range pages {
		for _, id := range page {
			if seen[id] {
				hasDuplicates = true
				continue
			}
			seen[id] = true
			all = append(all, id)
		}
	}
	return all, hasDuplicates
}

// markerFound reports whether any raw session item JSON contains the literal marker
// text. The controlled marker is a plain alphanumeric-and-hyphen token (see the task's
// "agentsctl-agents-api-probe-<nonce>" convention), so it requires no JSON-string
// unescaping to search for: a substring match on the raw encoded bytes is exact and
// avoids modeling the full session-item union type just to reach into message content.
func markerFound(items []json.RawMessage, marker string) bool {
	if marker == "" {
		return false
	}
	for _, raw := range items {
		if strings.Contains(string(raw), marker) {
			return true
		}
	}
	return false
}
