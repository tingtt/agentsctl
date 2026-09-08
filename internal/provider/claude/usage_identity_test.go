package claude

import (
	"os"
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

// TestRotateProbeIdentityRetiresRejectedIDAndPreservesTrust fixes ordinary
// rotation: rotating must mint a brand-new SessionID, retire the old one
// into RetiredSessionIDs, and carry TrustAccepted forward unchanged.
func TestRotateProbeIdentityRetiresRejectedIDAndPreservesTrust(t *testing.T) {
	path := filepath.Join(t.TempDir(), "probe.json")
	orig, err := loadOrCreateProbeIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := markTrustAccepted(path, orig); err != nil {
		t.Fatal(err)
	}

	rotated, err := rotateProbeIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	if rotated.SessionID == orig.SessionID {
		t.Fatal("rotation must mint a new SessionID, not reuse the rejected one")
	}
	if !rotated.TrustAccepted {
		t.Fatal("TrustAccepted must be preserved across rotation")
	}
	if len(rotated.RetiredSessionIDs) != 1 || rotated.RetiredSessionIDs[0] != orig.SessionID {
		t.Fatalf("RetiredSessionIDs=%v, want exactly [%q]", rotated.RetiredSessionIDs, orig.SessionID)
	}

	// And it's durably persisted, not just returned.
	persisted, ok, err := readProbeIdentityIfExists(path)
	if err != nil || !ok {
		t.Fatalf("no identity persisted after rotation: ok=%v err=%v", ok, err)
	}
	if persisted.SessionID != rotated.SessionID {
		t.Fatalf("persisted SessionID=%q, want %q", persisted.SessionID, rotated.SessionID)
	}
}

// TestAppendRetiredSessionIDDoesNotDuplicate fixes that retiring the same
// session ID more than once (e.g. a rotation retried after a partial
// failure) never grows a duplicate entry in the retired list.
func TestAppendRetiredSessionIDDoesNotDuplicate(t *testing.T) {
	retired := appendRetiredSessionID(nil, "a")
	retired = appendRetiredSessionID(retired, "b")
	retired = appendRetiredSessionID(retired, "a")
	if len(retired) != 2 {
		t.Fatalf("retired=%v, want exactly [a b] with no duplicate", retired)
	}
	if retired[0] != "a" || retired[1] != "b" {
		t.Fatalf("retired=%v, want [a b] in insertion order", retired)
	}
}

// TestProbeIdentityOwnsSessionIDIncludesCurrentAndRetired fixes
// probeIdentity.ownsSessionID/allSessionIDs, the ownership primitives
// Provider.List's catalog exclusion and Probe.KnownSessionIDs are built
// on: both the live SessionID and every RetiredSessionIDs entry must
// count as owned, and nothing else does.
func TestProbeIdentityOwnsSessionIDIncludesCurrentAndRetired(t *testing.T) {
	id := probeIdentity{SessionID: "current", RetiredSessionIDs: []string{"old-1", "old-2"}}
	for _, owned := range []string{"current", "old-1", "old-2"} {
		if !id.ownsSessionID(owned) {
			t.Fatalf("ownsSessionID(%q)=false, want true", owned)
		}
	}
	if id.ownsSessionID("someone-elses-session") {
		t.Fatal("ownsSessionID must not report ownership of an unrelated session id")
	}
	if id.ownsSessionID("") {
		t.Fatal("ownsSessionID must not treat an empty id as owned")
	}
	all := id.allSessionIDs()
	if len(all) != 3 || all[0] != "current" || all[1] != "old-1" || all[2] != "old-2" {
		t.Fatalf("allSessionIDs()=%v, want [current old-1 old-2]", all)
	}
}

// TestReadProbeIdentityIfExistsLoadsLegacyFileWithoutRetiredField fixes
// backward compatibility with a probe.json persisted before
// RetiredSessionIDs existed: it must still decode successfully, with
// RetiredSessionIDs simply empty -- no proactive migration write required.
func TestReadProbeIdentityIfExistsLoadsLegacyFileWithoutRetiredField(t *testing.T) {
	path := filepath.Join(t.TempDir(), "probe.json")
	legacy := `{"sessionId":"legacy-id","displayName":"agentsctl usage probe","trustAccepted":true}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	id, ok, err := readProbeIdentityIfExists(path)
	if err != nil || !ok {
		t.Fatalf("legacy probe.json failed to load: ok=%v err=%v", ok, err)
	}
	if id.SessionID != "legacy-id" || !id.TrustAccepted {
		t.Fatalf("id=%+v, want SessionID=legacy-id TrustAccepted=true", id)
	}
	if len(id.RetiredSessionIDs) != 0 {
		t.Fatalf("RetiredSessionIDs=%v, want empty for a legacy file", id.RetiredSessionIDs)
	}
	if !id.ownsSessionID("legacy-id") {
		t.Fatal("a legacy identity must still own its own current session id")
	}
}
