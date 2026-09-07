package claude

import (
	"path/filepath"
	"testing"
)

// TestLoadOrCreateProbeIdentityCreatesOnceThenReuses fixes the "at most
// one probe session" guarantee at the identity layer: a missing identity
// file gets a freshly generated one, and every subsequent load returns
// that exact same SessionID rather than minting a new one.
func TestLoadOrCreateProbeIdentityCreatesOnceThenReuses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "probe.json")
	first, err := loadOrCreateProbeIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	if first.SessionID == "" {
		t.Fatal("created identity has no SessionID")
	}
	if first.TrustAccepted {
		t.Fatal("a freshly created identity must not already be TrustAccepted")
	}
	second, err := loadOrCreateProbeIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	if second.SessionID != first.SessionID {
		t.Fatalf("SessionID changed across loads: %q -> %q", first.SessionID, second.SessionID)
	}
}

// TestMarkTrustAcceptedPersistsAcrossLoads fixes that TrustAccepted, once
// set, survives a reload -- the signal usage_probe_unix.go's refresh uses
// to skip the workspace-trust dialog answer on every run after the first.
func TestMarkTrustAcceptedPersistsAcrossLoads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "probe.json")
	id, err := loadOrCreateProbeIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := markTrustAccepted(path, id); err != nil {
		t.Fatal(err)
	}
	reloaded, err := loadOrCreateProbeIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.TrustAccepted {
		t.Fatal("TrustAccepted did not survive a reload")
	}
	if reloaded.SessionID != id.SessionID {
		t.Fatalf("SessionID changed after marking trust accepted: %q -> %q", id.SessionID, reloaded.SessionID)
	}
}

// TestNewUUIDv4LooksLikeAValidUUID fixes the shape --session-id requires
// ("must be a valid UUID" per the installed CLI's own --help): the
// well-known 8-4-4-4-12 hex grouping with the version/variant nibbles set.
func TestNewUUIDv4LooksLikeAValidUUID(t *testing.T) {
	id, err := newUUIDv4()
	if err != nil {
		t.Fatal(err)
	}
	if len(id) != 36 {
		t.Fatalf("len=%d, want 36: %q", len(id), id)
	}
	for _, i := range []int{8, 13, 18, 23} {
		if id[i] != '-' {
			t.Fatalf("id=%q, want '-' at index %d", id, i)
		}
	}
	if id[14] != '4' {
		t.Fatalf("id=%q, want version nibble '4' at index 14", id)
	}
}
