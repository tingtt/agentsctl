package claude

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

// probeIdentity is agentsctl's local record of its one owned Claude usage
// probe session -- the source of truth Provider.List uses to exclude the
// probe from the normal session catalog (see excludeProbe in usage.go) and
// the probe orchestration (usage_probe_unix.go) uses to decide whether to
// start a brand-new Claude conversation (--session-id) or continue the
// existing one (--resume). SessionID is the identity; DisplayName is
// decorative only (see the DesignDoc's Claude usage probe section --
// identity is never derived from a display name or CWD).
type probeIdentity struct {
	SessionID   string `json:"sessionId"`
	DisplayName string `json:"displayName"`
	// Confirmed is set once Provider has observed SessionID actually
	// appear in Claude's own native catalog (`claude agents --json --all`)
	// -- only then is it safe to address the session with --resume;
	// before that, the process that will *create* it must be started with
	// --session-id instead (see refresh in usage_probe_unix.go).
	Confirmed bool `json:"confirmed"`
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
// Confirmed: false) if none exists yet. This is the only place a new
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

// markProbeConfirmed persists id with Confirmed set to true -- called once
// Provider has verified id.SessionID actually appears in Claude's own
// native catalog, so every subsequent refresh addresses it with --resume
// instead of re-attempting --session-id creation.
func markProbeConfirmed(path string, id probeIdentity) error {
	id.Confirmed = true
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
