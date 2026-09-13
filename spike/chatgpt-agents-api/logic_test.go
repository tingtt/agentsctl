package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestFingerprintDoesNotLeakRawID(t *testing.T) {
	rawID := "sess_super_secret_raw_id_123"
	fp := fingerprint(rawID)

	if strings.Contains(fp, rawID) {
		t.Fatalf("fingerprint leaked the raw ID: %q", fp)
	}
	if len(fp) != 12 {
		t.Fatalf("expected a 12-character fingerprint, got %q (len=%d)", fp, len(fp))
	}
	if fingerprint(rawID) != fp {
		t.Fatalf("fingerprint is not deterministic")
	}
	if fingerprint("sess_different_id") == fp {
		t.Fatalf("different IDs produced the same fingerprint")
	}
}

func TestDiffSessionIDs_NewSessionAppears(t *testing.T) {
	before := []string{"A", "B"}
	after := []string{"A", "B", "C"}

	got := diffSessionIDs(before, after)
	want := []string{"C"}
	if !equalStrings(got, want) {
		t.Fatalf("diffSessionIDs = %v, want %v", got, want)
	}
}

func TestControlledCandidate_SingleNewSession(t *testing.T) {
	before := []string{"A", "B"}
	after := []string{"A", "B", "C"}

	id, ok := controlledCandidate(before, after)
	if !ok || id != "C" {
		t.Fatalf("controlledCandidate = (%q, %t), want (\"C\", true)", id, ok)
	}
}

func TestControlledCandidate_AmbiguousWhenMultipleNew(t *testing.T) {
	before := []string{"A", "B"}
	after := []string{"A", "B", "C", "D"}

	id, ok := controlledCandidate(before, after)
	if ok {
		t.Fatalf("controlledCandidate should refuse to guess with 2 new sessions, got id=%q", id)
	}
}

func TestControlledCandidate_AmbiguousWhenNoneNew(t *testing.T) {
	before := []string{"A", "B"}
	after := []string{"A", "B"}

	_, ok := controlledCandidate(before, after)
	if ok {
		t.Fatalf("controlledCandidate should be NOT VERIFIED when nothing new appeared")
	}
}

func TestAccumulatePages_NoDuplicates(t *testing.T) {
	pages := [][]string{{"A", "B"}, {"C", "D"}}

	all, hasDup := accumulatePages(pages)
	want := []string{"A", "B", "C", "D"}
	if !equalStrings(all, want) {
		t.Fatalf("accumulatePages ids = %v, want %v", all, want)
	}
	if hasDup {
		t.Fatalf("accumulatePages reported a duplicate where there was none")
	}
}

func TestAccumulatePages_DetectsDuplicateAcrossPages(t *testing.T) {
	pages := [][]string{{"A", "B"}, {"B", "C"}}

	all, hasDup := accumulatePages(pages)
	if !hasDup {
		t.Fatalf("accumulatePages failed to detect duplicate B across pages")
	}
	want := []string{"A", "B", "C"}
	if !equalStrings(all, want) {
		t.Fatalf("accumulatePages ids = %v, want %v (dedup should keep only the first occurrence)", all, want)
	}
}

func TestMarkerFound_Match(t *testing.T) {
	marker := "agentsctl-agents-api-probe-abc123"
	items := []json.RawMessage{
		json.RawMessage(`{"type":"message","role":"user","content":[{"type":"input_text","text":"unrelated text"}]}`),
		json.RawMessage(`{"type":"message","role":"user","content":[{"type":"input_text","text":"Use this exact marker: agentsctl-agents-api-probe-abc123"}]}`),
	}

	if !markerFound(items, marker) {
		t.Fatalf("markerFound should have matched the marker embedded in the second item")
	}
}

func TestMarkerFound_NoMatch(t *testing.T) {
	marker := "agentsctl-agents-api-probe-abc123"
	items := []json.RawMessage{
		json.RawMessage(`{"type":"message","role":"user","content":[{"type":"input_text","text":"unrelated text"}]}`),
	}

	if markerFound(items, marker) {
		t.Fatalf("markerFound should not match when the marker is absent")
	}
}

func TestMarkerFound_EmptyMarkerNeverMatches(t *testing.T) {
	items := []json.RawMessage{json.RawMessage(`{"type":"message"}`)}
	if markerFound(items, "") {
		t.Fatalf("an empty marker must never report a match")
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
