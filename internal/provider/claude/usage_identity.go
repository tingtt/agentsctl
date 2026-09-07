package claude

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

// probeIdentity is agentsctl's local record of its one owned Claude usage
// probe session -- the source of truth Provider.List uses to exclude the
// probe from the normal session catalog (see UsageProbeSource's
// KnownSessionID) and the probe orchestration (usage_probe_unix.go)
// addresses on every refresh via --session-id. SessionID is the identity;
// DisplayName is decorative only (see the DesignDoc's Claude usage probe
// section -- identity is never derived from a display name or CWD).
//
// Every refresh uses --session-id (never --resume): verified against the
// installed CLI that re-addressing an existing session ID via
// --session-id never errors, whether or not that ID has any resumable
// history (a session ID Claude has no saved transcript for -- e.g. one
// whose process had to be force-killed -- behaves exactly like a brand
// new one). The probe never depends on conversation continuity for its
// own purpose (one trivial round trip per refresh), so there is nothing
// to lose either way; this sidesteps needing to track or verify whether a
// given refresh's session survives to be resumable, and the earlier
// "confirm this session appeared in the native catalog, then switch to
// --resume" design this replaced never actually needed to run.
type probeIdentity struct {
	SessionID   string `json:"sessionId"`
	DisplayName string `json:"displayName"`
	// TrustAccepted is set once this probe has answered Claude Code's
	// workspace-trust confirmation dialog for its dedicated directory
	// (see Probe.refresh in usage_probe_unix.go) -- shown only the
	// very first time any interactive session runs in a directory Claude
	// hasn't seen before, and, once accepted, remembered by Claude Code
	// itself (in its own local state, not anything agentsctl writes) for
	// every later session in that same directory. Refresh only attempts
	// to answer it while this is false, so a later refresh's real prompt
	// is never preceded by a blind, unnecessary keystroke into a live
	// chat composer.
	TrustAccepted bool `json:"trustAccepted"`
}

// probeDisplayName is the fixed, human-readable name given to the probe
// session when it's first created -- shown only if a human ever inspects
// Claude's own native session list directly; agentsctl's own Agent View
// never renders this row at all (see Provider.List's KnownSessionID
// exclusion).
const probeDisplayName = "agentsctl usage probe"

// readProbeIdentityIfExists reads path's persisted probe identity without
// creating one -- the read-only half of loadOrCreateProbeIdentity, for a
// caller (Probe.KnownSessionID) that must never mint a new identity as a
// side effect of a read. ok is false for a missing file, an empty
// SessionID, or a decode error -- never treated as a fatal condition by
// callers that only want "is there one, and if so what is it".
func readProbeIdentityIfExists(path string) (probeIdentity, bool, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return probeIdentity{}, false, nil
	}
	if err != nil {
		return probeIdentity{}, false, err
	}
	var id probeIdentity
	if err := json.Unmarshal(b, &id); err != nil {
		return probeIdentity{}, false, err
	}
	if id.SessionID == "" {
		return probeIdentity{}, false, nil
	}
	return id, true, nil
}

// loadOrCreateProbeIdentity reads path's persisted probe identity, or
// creates and persists a brand-new one (a fresh random session ID,
// TrustAccepted: false) if none exists yet. This is the only place a new
// probe session ID is ever minted -- called at most once per app-data
// directory's lifetime, after which every refresh reuses the same
// identity (see the DesignDoc's "probe session は最大1つだけ存在する").
func loadOrCreateProbeIdentity(path string) (probeIdentity, error) {
	if id, ok, err := readProbeIdentityIfExists(path); err != nil {
		return probeIdentity{}, fmt.Errorf("decode claude usage probe identity: %w", err)
	} else if ok {
		return id, nil
	}
	uuid, err := newUUIDv4()
	if err != nil {
		return probeIdentity{}, err
	}
	id := probeIdentity{SessionID: uuid, DisplayName: probeDisplayName}
	if err := writeProbeIdentity(path, id); err != nil {
		return probeIdentity{}, err
	}
	return id, nil
}

// markTrustAccepted persists id with TrustAccepted set to true -- called
// once refresh (usage_probe_unix.go) has attempted to answer the
// workspace-trust dialog, regardless of whether a dialog actually needed
// answering, so no later refresh ever repeats that blind keystroke
// sequence into what might by then be a live chat composer instead.
func markTrustAccepted(path string, id probeIdentity) error {
	id.TrustAccepted = true
	return writeProbeIdentity(path, id)
}

// writeProbeIdentity persists id atomically (see writeFileAtomic).
func writeProbeIdentity(path string, id probeIdentity) error {
	b, err := json.Marshal(id)
	if err != nil {
		return err
	}
	return writeFileAtomic(path, b)
}

// probeSessionConflictPhrase is the Claude CLI's own error text when a
// --session-id is rejected as already connected elsewhere -- confirmed
// against the installed CLI (2.1.263) via a live reproduction of Issue
// #19's follow-up "claude ?% / ?% それ自体が全く更新されない" symptom: a real
// installed agentsctl's persisted probe session ID had become permanently
// rejected ("Error: Session ID <uuid> is already in use."), printed to
// the probe's own terminal output immediately on startup (well within the
// settle delay, before any prompt is ever sent), and every subsequent
// refresh attempt using that same session ID failed identically and
// indefinitely -- reproduced twice in a row with no change. Minting a
// brand-new identity (see discardProbeIdentity) and retrying was
// confirmed, against the same real installed CLI, to succeed immediately.
const probeSessionConflictPhrase = "is already in use"

// probeSessionConflict reports whether output shows Claude Code rejecting
// this probe's --session-id as already in use elsewhere -- see
// probeSessionConflictPhrase's doc comment. This is a process/session-
// identity-level signal, unrelated to classifyProbeOutput's usage-limit
// wording (a different failure mode entirely, checked separately in
// Probe.refresh).
func probeSessionConflict(output string) bool {
	return strings.Contains(output, probeSessionConflictPhrase)
}

// discardProbeIdentity removes path's persisted identity so the next
// loadOrCreateProbeIdentity call mints a fresh session ID -- the only
// recovery available once Claude Code has permanently rejected the
// current one (see probeSessionConflict): the probe's design of reusing
// one persisted identity forever (see probeIdentity's own doc comment)
// has no other way to recover from a rejection that never clears on its
// own. A missing file is not an error: discarding an identity that's
// already gone (e.g. a concurrent refresh already discarded it) is a
// no-op.
func discardProbeIdentity(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// newUUIDv4 generates a random RFC 4122 version-4 UUID -- sufficient for
// --session-id (which only requires a valid UUID, not any particular
// generation scheme) without pulling in an external dependency for what
// is otherwise a 16-byte random value with two fixed nibbles.
func newUUIDv4() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
