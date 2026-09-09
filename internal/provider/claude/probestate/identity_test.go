package probestate

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// TestLoadOrCreateCreatesOnceThenReuses fixes the "at most one probe
// session" guarantee at the identity layer: a missing identity file gets a
// freshly generated one, and every subsequent load returns that exact
// same SessionID rather than minting a new one.
func TestLoadOrCreateCreatesOnceThenReuses(t *testing.T) {
	s := NewIdentityStore(filepath.Join(t.TempDir(), "probe.json"))
	first, err := s.LoadOrCreate()
	if err != nil {
		t.Fatal(err)
	}
	if first.SessionID == "" {
		t.Fatal("created identity has no SessionID")
	}
	if first.TrustAccepted {
		t.Fatal("a freshly created identity must not already be TrustAccepted")
	}
	second, err := s.LoadOrCreate()
	if err != nil {
		t.Fatal(err)
	}
	if second.SessionID != first.SessionID {
		t.Fatalf("SessionID changed across loads: %q -> %q", first.SessionID, second.SessionID)
	}
}

// TestMarkTrustAcceptedPersistsAcrossLoads fixes that TrustAccepted, once
// set, survives a reload -- the signal the probe's refresh uses to skip
// the workspace-trust dialog answer on every run after the first.
func TestMarkTrustAcceptedPersistsAcrossLoads(t *testing.T) {
	s := NewIdentityStore(filepath.Join(t.TempDir(), "probe.json"))
	id, err := s.LoadOrCreate()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MarkTrustAccepted(); err != nil {
		t.Fatal(err)
	}
	reloaded, err := s.LoadOrCreate()
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

// TestRotateRetiresRejectedIDAndPreservesTrust fixes ordinary,
// single-process rotation: rejectedSessionID matching the freshly read
// persisted current must mint a brand-new SessionID, retire the old one
// into RetiredSessionIDs, carry TrustAccepted forward unchanged, and
// report Rotated:true.
func TestRotateRetiresRejectedIDAndPreservesTrust(t *testing.T) {
	s := NewIdentityStore(filepath.Join(t.TempDir(), "probe.json"))
	orig, err := s.LoadOrCreate()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MarkTrustAccepted(); err != nil {
		t.Fatal(err)
	}

	recovery, err := s.Rotate(orig.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if !recovery.Rotated {
		t.Fatal("want Rotated=true for an ordinary single-process rotation")
	}
	if recovery.Identity.SessionID == orig.SessionID {
		t.Fatal("rotation must mint a new SessionID, not reuse the rejected one")
	}
	if !recovery.Identity.TrustAccepted {
		t.Fatal("TrustAccepted must be preserved across rotation")
	}
	if len(recovery.Identity.RetiredSessionIDs) != 1 || recovery.Identity.RetiredSessionIDs[0] != orig.SessionID {
		t.Fatalf("RetiredSessionIDs=%v, want exactly [%q]", recovery.Identity.RetiredSessionIDs, orig.SessionID)
	}

	// And it's durably persisted, not just returned.
	persisted, ok, err := s.Load()
	if err != nil || !ok {
		t.Fatalf("no identity persisted after rotation: ok=%v err=%v", ok, err)
	}
	if persisted.SessionID != recovery.Identity.SessionID {
		t.Fatalf("persisted SessionID=%q, want %q", persisted.SessionID, recovery.Identity.SessionID)
	}
}

// TestRotateIsIdempotentAcrossProcesses fixes the cross-process race two
// agentsctl processes sharing the same probe.json can hit: if the
// persisted current identity has already moved on from rejectedSessionID
// (another process's own rotation got there first), Rotate must hand that
// already-rotated identity back as-is -- no new SessionID minted, no new
// write, and the retired list left exactly as that other process's
// rotation set it.
func TestRotateIsIdempotentAcrossProcesses(t *testing.T) {
	s := NewIdentityStore(filepath.Join(t.TempDir(), "probe.json"))
	orig, err := s.LoadOrCreate()
	if err != nil {
		t.Fatal(err)
	}

	// Simulate another process already recovering from the exact same
	// rejection this process is about to react to.
	external, err := s.Rotate(orig.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if !external.Rotated {
		t.Fatal("setup: the simulated external rotation should itself have rotated")
	}

	// This process still believes orig is current and reacts to the same
	// rejection -- Rotate must notice the persisted current has already
	// moved on.
	recovery, err := s.Rotate(orig.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if recovery.Rotated {
		t.Fatal("want Rotated=false when another process already recovered from this exact rejection")
	}
	if recovery.Identity.SessionID != external.Identity.SessionID {
		t.Fatalf("got SessionID=%q, want the already-rotated %q -- no redundant SessionID should be minted", recovery.Identity.SessionID, external.Identity.SessionID)
	}
	if len(recovery.Identity.RetiredSessionIDs) != 1 || recovery.Identity.RetiredSessionIDs[0] != orig.SessionID {
		t.Fatalf("RetiredSessionIDs=%v, want exactly [%q] (unchanged by the redundant call)", recovery.Identity.RetiredSessionIDs, orig.SessionID)
	}

	// And nothing was written to disk by the redundant call: the persisted
	// identity is still exactly what the external rotation left.
	persisted, ok, err := s.Load()
	if err != nil || !ok {
		t.Fatalf("no identity persisted: ok=%v err=%v", ok, err)
	}
	if persisted.SessionID != external.Identity.SessionID {
		t.Fatalf("persisted SessionID=%q, want the external rotation's %q untouched", persisted.SessionID, external.Identity.SessionID)
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

// TestIdentityOwnsSessionIDIncludesCurrentAndRetired fixes
// Identity.OwnsSessionID/AllSessionIDs, the ownership primitives
// Provider.List's catalog exclusion and IdentityStore.KnownSessionIDs are
// built on: both the live SessionID and every RetiredSessionIDs entry
// must count as owned, and nothing else does.
func TestIdentityOwnsSessionIDIncludesCurrentAndRetired(t *testing.T) {
	id := Identity{SessionID: "current", RetiredSessionIDs: []string{"old-1", "old-2"}}
	for _, owned := range []string{"current", "old-1", "old-2"} {
		if !id.OwnsSessionID(owned) {
			t.Fatalf("OwnsSessionID(%q)=false, want true", owned)
		}
	}
	if id.OwnsSessionID("someone-elses-session") {
		t.Fatal("OwnsSessionID must not report ownership of an unrelated session id")
	}
	if id.OwnsSessionID("") {
		t.Fatal("OwnsSessionID must not treat an empty id as owned")
	}
	all := id.AllSessionIDs()
	if len(all) != 3 || all[0] != "current" || all[1] != "old-1" || all[2] != "old-2" {
		t.Fatalf("AllSessionIDs()=%v, want [current old-1 old-2]", all)
	}
}

// TestLoadLoadsLegacyFileWithoutRetiredField fixes backward compatibility
// with a probe.json persisted before RetiredSessionIDs existed: it must
// still decode successfully, with RetiredSessionIDs simply empty -- no
// proactive migration write required.
func TestLoadLoadsLegacyFileWithoutRetiredField(t *testing.T) {
	path := filepath.Join(t.TempDir(), "probe.json")
	legacy := `{"sessionId":"legacy-id","displayName":"agentsctl usage probe","trustAccepted":true}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	id, ok, err := NewIdentityStore(path).Load()
	if err != nil || !ok {
		t.Fatalf("legacy probe.json failed to load: ok=%v err=%v", ok, err)
	}
	if id.SessionID != "legacy-id" || !id.TrustAccepted {
		t.Fatalf("id=%+v, want SessionID=legacy-id TrustAccepted=true", id)
	}
	if len(id.RetiredSessionIDs) != 0 {
		t.Fatalf("RetiredSessionIDs=%v, want empty for a legacy file", id.RetiredSessionIDs)
	}
	if !id.OwnsSessionID("legacy-id") {
		t.Fatal("a legacy identity must still own its own current session id")
	}
}

// TestRotateConcurrentCallsProduceExactlyOneWinner fixes the cross-process
// rotation race directly at the identity-transaction layer: an atomic
// rename alone only stops a reader from seeing a half-written file, it
// does not serialize the read-decide-write sequence rotation performs, so
// two callers that both observe current==rejectedSessionID before either
// writes could, without a transaction lock around the whole sequence, each
// mint their own new SessionID and each write -- forking probe identity
// ownership (the second write's SessionID ends up owned by neither
// `current` nor `RetiredSessionIDs` of the version that actually
// survives). Many concurrent callers, released together via a barrier to
// maximize the chance of that race actually happening absent the fix, must
// converge on exactly one rotation and one winning identity.
func TestRotateConcurrentCallsProduceExactlyOneWinner(t *testing.T) {
	s := NewIdentityStore(filepath.Join(t.TempDir(), "probe.json"))
	orig, err := s.LoadOrCreate()
	if err != nil {
		t.Fatal(err)
	}

	const n = 8
	var ready sync.WaitGroup
	ready.Add(n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make([]RotateResult, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ready.Done()
			<-start
			results[i], errs[i] = s.Rotate(orig.SessionID)
		}(i)
	}
	ready.Wait()
	close(start)
	wg.Wait()

	rotatedCount := 0
	var winner string
	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d: %v", i, err)
		}
		if results[i].Rotated {
			rotatedCount++
			winner = results[i].Identity.SessionID
		}
	}
	if rotatedCount != 1 {
		t.Fatalf("rotatedCount=%d, want exactly 1 (no forked rotation)", rotatedCount)
	}
	for i, r := range results {
		if r.Identity.SessionID != winner {
			t.Fatalf("caller %d returned SessionID=%q, want every caller to converge on the same winner %q", i, r.Identity.SessionID, winner)
		}
	}

	persisted, ok, err := s.Load()
	if err != nil || !ok {
		t.Fatalf("no identity persisted after concurrent rotation: ok=%v err=%v", ok, err)
	}
	if persisted.SessionID != winner {
		t.Fatalf("persisted SessionID=%q, want the winner %q", persisted.SessionID, winner)
	}
	if len(persisted.RetiredSessionIDs) != 1 || persisted.RetiredSessionIDs[0] != orig.SessionID {
		t.Fatalf("RetiredSessionIDs=%v, want exactly [%q] -- a fork would show up here as an extra, unowned SessionID lost from both current and retired", persisted.RetiredSessionIDs, orig.SessionID)
	}
}

// TestLoadOrCreateConcurrentColdStartConverges fixes the cold-start half
// of the same cross-process race: with no probe.json yet, concurrent
// callers must mint and persist exactly one identity, not one each --
// every caller has to return the same SessionID the winner actually wrote.
func TestLoadOrCreateConcurrentColdStartConverges(t *testing.T) {
	s := NewIdentityStore(filepath.Join(t.TempDir(), "probe.json"))

	const n = 8
	var ready sync.WaitGroup
	ready.Add(n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make([]Identity, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ready.Done()
			<-start
			results[i], errs[i] = s.LoadOrCreate()
		}(i)
	}
	ready.Wait()
	close(start)
	wg.Wait()

	want := results[0].SessionID
	if want == "" {
		t.Fatal("no SessionID minted")
	}
	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d: %v", i, err)
		}
		if results[i].SessionID != want {
			t.Fatalf("caller %d SessionID=%q, want %q -- every concurrent cold-start caller must converge on one identity, not mint its own", i, results[i].SessionID, want)
		}
	}

	persisted, ok, err := s.Load()
	if err != nil || !ok {
		t.Fatalf("no identity persisted: ok=%v err=%v", ok, err)
	}
	if persisted.SessionID != want {
		t.Fatalf("persisted SessionID=%q, want %q", persisted.SessionID, want)
	}
}

// TestMarkTrustAcceptedCannotRollBackARotationItDidNotKnowAbout fixes the
// other half of the cross-process invariant: MarkTrustAccepted does not
// take a caller-supplied identity to write back, specifically because a
// caller can be holding an identity that's already been rotated away by
// another process by the time this call actually runs. Writing that stale
// copy back would be able to resurrect a rejected SessionID as current
// again, drop the RetiredSessionIDs entry another process's rotation had
// just recorded, and still leave the genuinely-current (rotated) identity
// untrusted -- this proves none of that can happen: whatever identity is
// LATEST persisted when MarkTrustAccepted actually runs is the one that
// ends up marked trusted, and only that one bit changes.
func TestMarkTrustAcceptedCannotRollBackARotationItDidNotKnowAbout(t *testing.T) {
	s := NewIdentityStore(filepath.Join(t.TempDir(), "probe.json"))
	orig, err := s.LoadOrCreate()
	if err != nil {
		t.Fatal(err)
	}
	if orig.TrustAccepted {
		t.Fatalf("orig=%+v, want a freshly created identity to start untrusted", orig)
	}

	// Simulate another process's rotation landing between some caller
	// loading `orig` and that caller getting around to calling
	// MarkTrustAccepted -- exactly the shape of the race described above.
	recovery, err := s.Rotate(orig.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if !recovery.Rotated {
		t.Fatal("setup: rotation should have happened")
	}

	if err := s.MarkTrustAccepted(); err != nil {
		t.Fatal(err)
	}

	persisted, ok, err := s.Load()
	if err != nil || !ok {
		t.Fatalf("no identity persisted: ok=%v err=%v", ok, err)
	}
	if persisted.SessionID != recovery.Identity.SessionID {
		t.Fatalf("current SessionID=%q, want the rotated %q -- MarkTrustAccepted must not resurrect a rotated-away session id", persisted.SessionID, recovery.Identity.SessionID)
	}
	if len(persisted.RetiredSessionIDs) != 1 || persisted.RetiredSessionIDs[0] != orig.SessionID {
		t.Fatalf("RetiredSessionIDs=%v, want exactly [%q] -- MarkTrustAccepted must not drop a rotation's retired id", persisted.RetiredSessionIDs, orig.SessionID)
	}
	if !persisted.TrustAccepted {
		t.Fatal("TrustAccepted must become true on the latest (rotated) identity")
	}
}
