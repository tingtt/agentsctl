package claude

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

// probeIdentity is agentsctl's local record of every Claude usage probe
// session ID it has ever owned -- the source of truth Provider.List uses
// to exclude the probe from the normal session catalog (see
// UsageProbeSource's KnownSessionIDs) and the probe orchestration
// (usage_probe_unix.go) addresses on every refresh via --session-id
// (always SessionID, never one of RetiredSessionIDs).
//
// SessionID, RetiredSessionIDs, and TrustAccepted have deliberately
// different lifetimes, tracked together here only because they're
// persisted together:
//
//   - SessionID identifies the probe's current, live Claude Code session.
//     Normally reused unchanged across every refresh -- addressing an
//     already-known --session-id, with or without resumable history, is
//     not itself an error (verified against the installed CLI: an ID
//     Claude has no saved transcript for, e.g. one whose process had to be
//     force-killed, behaves like a brand new one). But Claude Code CAN
//     permanently reject a specific ID as already in use elsewhere (see
//     errProbeSessionConflict) -- confirmed via a live reproduction of
//     Issue #19's "?% stuck forever" symptom, where the same rejected ID
//     failed identically and indefinitely on every retry. When that
//     happens the ID itself is replaced (see rotateProbeIdentity); the
//     probe never depends on any given ID's conversation surviving (one
//     trivial round trip is all any refresh ever needs), so losing one
//     costs nothing beyond needing a fresh one.
//   - RetiredSessionIDs holds every SessionID a rotation has ever replaced
//     -- Claude Code's own native catalog does not remove a rejected
//     session's row just because agentsctl has stopped addressing it, so
//     without this list a rotated-away ID would resurface in Provider.List
//     as an ordinary user session (pinnable, renameable, stoppable) the
//     moment it stopped being `SessionID`. Ownership of a probe is
//     therefore the full set {SessionID} ∪ RetiredSessionIDs, never just
//     the current one -- see probeIdentity.ownsSessionID.
//   - TrustAccepted tracks a completely different, longer-lived fact:
//     whether agentsctl has ever answered Claude Code's workspace-trust
//     confirmation dialog for this probe's dedicated *directory* (see
//     Probe.refreshOnce). That dialog is a property of the directory, not
//     of any particular session ID -- verified against the installed
//     CLI, Claude Code itself remembers a directory as trusted (in its
//     own local state, not anything agentsctl writes) regardless of
//     which session ID is later used within it. rotateProbeIdentity
//     therefore always carries TrustAccepted forward unchanged: a
//     rotation only ever happens within the same probe directory, so
//     whatever trust state was already established for that directory
//     still applies to the new ID. Getting this wrong would mean
//     resending the blind trust-dialog keystroke sequence into what is,
//     from Claude Code's own perspective, an already-trusted directory's
//     live chat composer.
//
// DisplayName is decorative only (see the DesignDoc's Claude usage probe
// section -- identity is never derived from a display name or CWD).
type probeIdentity struct {
	SessionID   string `json:"sessionId"`
	DisplayName string `json:"displayName"`
	// TrustAccepted is set once this probe has answered Claude Code's
	// workspace-trust confirmation dialog for its dedicated directory --
	// shown only the very first time any interactive session runs in a
	// directory Claude hasn't seen before. refreshOnce only attempts to
	// answer it while this is false, so a later refresh's real prompt is
	// never preceded by a blind, unnecessary keystroke into a live chat
	// composer. See probeIdentity's own doc comment for why this survives
	// a SessionID rotation unchanged (rotateProbeIdentity) even though a
	// rotation always mints a brand new SessionID.
	TrustAccepted bool `json:"trustAccepted"`
	// RetiredSessionIDs lists every SessionID a prior rotation has
	// replaced, oldest first, deduplicated (see appendRetiredSessionID).
	// A probe.json persisted before this field existed simply decodes it
	// as nil/empty -- ownsSessionID and allSessionIDs treat that exactly
	// like "no retired IDs yet", so no proactive migration write is
	// needed; the field is populated naturally the next time a rotation
	// happens. See probeIdentity's own doc comment.
	RetiredSessionIDs []string `json:"retiredSessionIds,omitempty"`
}

// ownsSessionID reports whether sessionID is this probe's current ID or
// one of its retired ones -- the exact-identity ownership test
// Provider.List uses (via Probe.KnownSessionIDs) to exclude every row
// agentsctl has ever addressed as its probe, not just the live one. Never
// derived from CWD or DisplayName (see probeIdentity's own doc comment).
func (id probeIdentity) ownsSessionID(sessionID string) bool {
	if sessionID == "" {
		return false
	}
	if id.SessionID == sessionID {
		return true
	}
	for _, r := range id.RetiredSessionIDs {
		if r == sessionID {
			return true
		}
	}
	return false
}

// allSessionIDs returns every session ID this probe owns -- the current
// one (if any) followed by every retired one -- for a caller like
// Probe.KnownSessionIDs that needs the full set at once rather than a
// per-ID membership test.
func (id probeIdentity) allSessionIDs() []string {
	ids := make([]string, 0, len(id.RetiredSessionIDs)+1)
	if id.SessionID != "" {
		ids = append(ids, id.SessionID)
	}
	return append(ids, id.RetiredSessionIDs...)
}

// appendRetiredSessionID returns retired with sessionID added, unless
// it's already present -- rotateProbeIdentity's own dedup guarantee, kept
// as a separate pure function so it's independently testable: a retired
// list must never grow a duplicate entry no matter how many times the
// same ID is (attempted to be) retired.
func appendRetiredSessionID(retired []string, sessionID string) []string {
	for _, r := range retired {
		if r == sessionID {
			return retired
		}
	}
	return append(append([]string{}, retired...), sessionID)
}

// probeDisplayName is the fixed, human-readable name given to the probe
// session when it's first created -- shown only if a human ever inspects
// Claude's own native session list directly; agentsctl's own Agent View
// never renders this row at all (see Provider.List's KnownSessionIDs
// exclusion).
const probeDisplayName = "agentsctl usage probe"

// readProbeIdentityIfExists reads path's persisted probe identity without
// creating one -- the read-only half of loadOrCreateProbeIdentity, for a
// caller (Probe.KnownSessionIDs) that must never mint a new identity as a
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
// TrustAccepted: false) if none exists yet. This is the ordinary,
// expected path a new probe session ID is minted on -- once per app-data
// directory's lifetime, after which every refresh reuses the same
// identity (see the DesignDoc's "probe session は最大1つだけ存在する"). The
// only other place a new SessionID is ever minted is rotateProbeIdentity,
// an exceptional replacement for a specific identity Claude Code has
// permanently rejected (see probeSessionConflict) -- unlike this
// function, that one never creates an identity from nothing; it only
// ever replaces an already-existing one.
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
// indefinitely -- reproduced twice in a row with no change. Rotating to a
// brand-new identity (see rotateProbeIdentity) and retrying was
// confirmed, against the same real installed CLI, to succeed immediately.
const probeSessionConflictPhrase = "is already in use"

// errProbeSessionConflict is the sentinel Probe.refreshOnce's error wraps
// when probeSessionConflict classifies a refresh's captured output as
// Claude Code rejecting the session ID used -- see
// probeSessionConflictPhrase's doc comment. Kept distinguishable via
// errors.Is (never by re-parsing an error string) specifically so
// Probe.refreshWithRecovery can react to this one failure mode -- and
// only this one -- with an identity rotation and a single bounded retry,
// while every other refreshOnce failure (timeout, parse failure, process
// launch failure, ...) falls straight through to the existing stale-cache
// fallback untouched.
var errProbeSessionConflict = errors.New("claude usage probe: session id rejected as already in use")

// probeSessionConflict reports whether output shows Claude Code rejecting
// this probe's --session-id as already in use elsewhere -- see
// probeSessionConflictPhrase's doc comment. This is a process/session-
// identity-level signal, unrelated to classifyProbeOutput's usage-limit
// wording (a different failure mode entirely, checked separately in
// Probe.refreshOnce).
func probeSessionConflict(output string) bool {
	return strings.Contains(output, probeSessionConflictPhrase)
}

// rotateProbeIdentity replaces path's persisted identity with a freshly
// minted SessionID, carrying DisplayName and -- critically -- TrustAccepted
// forward unchanged (see probeIdentity's own doc comment for why
// TrustAccepted survives a rotation: it belongs to the probe *directory*,
// which a rotation never changes, not to the SessionID being replaced),
// and retires the old current SessionID into RetiredSessionIDs
// (deduplicated -- see appendRetiredSessionID) so Provider.List keeps
// excluding it from the normal catalog even after it stops being current
// (Claude Code's own native catalog does not remove a rejected session's
// row on its own -- see probeIdentity's own doc comment). This is the
// only recovery available once Claude Code has permanently rejected an
// identity's SessionID (see probeSessionConflict): the probe's design of
// otherwise reusing one persisted identity forever has no other way to
// recover from a rejection that never clears on its own.
//
// It deliberately re-reads path itself rather than taking the caller's
// own probeIdentity value as the record to rotate from: a caller like
// Probe.refreshWithRecovery loaded its copy before calling
// Probe.refreshOnce, and refreshOnce can itself persist a TrustAccepted
// update (see markTrustAccepted) partway through that same attempt --
// specifically, a session conflict can surface right after the
// workspace-trust dialog was just answered, before the attempt otherwise
// succeeds. Rotating from the caller's now-stale in-memory copy would
// silently regress a TrustAccepted:true that was already durably
// persisted moments earlier, resending the blind trust-dialog keystroke
// sequence into what Claude Code already considers a trusted directory's
// live chat composer on the very next attempt. Reading the current
// on-disk record instead makes the persisted file -- not any particular
// caller's possibly-outdated copy -- the single source of truth for what
// gets carried forward.
func rotateProbeIdentity(path string) (probeIdentity, error) {
	current, ok, err := readProbeIdentityIfExists(path)
	if err != nil {
		return probeIdentity{}, fmt.Errorf("read claude usage probe identity for rotation: %w", err)
	}
	if !ok {
		return probeIdentity{}, errors.New("claude usage probe: no identity to rotate")
	}
	uuid, err := newUUIDv4()
	if err != nil {
		return probeIdentity{}, err
	}
	rotated := probeIdentity{
		SessionID:         uuid,
		DisplayName:       current.DisplayName,
		TrustAccepted:     current.TrustAccepted,
		RetiredSessionIDs: appendRetiredSessionID(current.RetiredSessionIDs, current.SessionID),
	}
	if err := writeProbeIdentity(path, rotated); err != nil {
		return probeIdentity{}, err
	}
	return rotated, nil
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
