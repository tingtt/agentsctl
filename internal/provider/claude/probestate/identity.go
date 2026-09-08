// Package probestate is the persistence boundary for the Claude usage
// probe's machine-global state: the probe's own session identity
// (probe.json) and its persisted usage snapshot (usage.json). Both files
// are shared across every agentsctl process on the machine.
//
// This package is the ONLY place that reads or writes those two files.
// Every mutation of probe.json goes through IdentityStore's domain
// operations (LoadOrCreate, MarkTrustAccepted, Rotate) -- each one a
// complete read-modify-write transaction under an OS-level advisory lock,
// never a raw read/decide/write a caller assembles itself. Persisted-schema
// concerns (legacy decode compatibility, atomic replace) stay inside this
// package too, so callers in internal/provider/claude never need to know
// the wire shape of either file.
package probestate

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// probeDisplayName is the fixed, human-readable name given to the probe
// session when it's first created -- shown only if a human ever inspects
// Claude's own native session list directly.
const probeDisplayName = "agentsctl usage probe"

// Identity is agentsctl's local record of every Claude usage probe session
// ID it has ever owned -- the source of truth Provider.List uses to
// exclude the probe from the normal session catalog (via
// IdentityStore.KnownSessionIDs) and the probe orchestration addresses on
// every refresh via --session-id (always SessionID, never one of
// RetiredSessionIDs).
//
// SessionID, RetiredSessionIDs, and TrustAccepted have deliberately
// different lifetimes, tracked together here only because they're
// persisted together:
//
//   - SessionID identifies the probe's current, live Claude Code session.
//     Normally reused unchanged across every refresh. But Claude Code CAN
//     permanently reject a specific ID as already in use elsewhere -- when
//     that happens the ID itself is replaced (see IdentityStore.Rotate);
//     the probe never depends on any given ID's conversation surviving, so
//     losing one costs nothing beyond needing a fresh one.
//   - RetiredSessionIDs holds every SessionID a rotation has ever replaced
//     -- Claude Code's own native catalog does not remove a rejected
//     session's row just because agentsctl has stopped addressing it, so
//     without this list a rotated-away ID would resurface in Provider.List
//     as an ordinary user session the moment it stopped being
//     `SessionID`. Ownership of a probe is therefore the full set
//     {SessionID} ∪ RetiredSessionIDs, never just the current one -- see
//     Identity.OwnsSessionID.
//   - TrustAccepted tracks a completely different, longer-lived fact:
//     whether agentsctl has ever answered Claude Code's workspace-trust
//     confirmation dialog for this probe's dedicated *directory*. That
//     dialog is a property of the directory, not of any particular session
//     ID, so IdentityStore.Rotate always carries TrustAccepted forward
//     unchanged: a rotation only ever happens within the same probe
//     directory, so whatever trust state was already established for that
//     directory still applies to the new ID. This is metadata about
//     whether agentsctl has completed that flow once, not the Claude
//     directory's own trust state itself -- Claude Code remembers that in
//     its own local state, not anything this package writes.
//
// DisplayName is decorative only -- identity is never derived from a
// display name or CWD.
type Identity struct {
	SessionID   string `json:"sessionId"`
	DisplayName string `json:"displayName"`
	// TrustAccepted is set once this probe has answered Claude Code's
	// workspace-trust confirmation dialog for its dedicated directory --
	// shown only the very first time any interactive session runs in a
	// directory Claude hasn't seen before. See Identity's own doc comment
	// for why this survives a SessionID rotation unchanged even though a
	// rotation always mints a brand new SessionID.
	TrustAccepted bool `json:"trustAccepted"`
	// RetiredSessionIDs lists every SessionID a prior rotation has
	// replaced, oldest first, deduplicated. An identity persisted before
	// this field existed simply decodes it as nil/empty -- OwnsSessionID
	// and AllSessionIDs treat that exactly like "no retired IDs yet".
	RetiredSessionIDs []string `json:"retiredSessionIds,omitempty"`
}

// OwnsSessionID reports whether sessionID is this identity's current ID or
// one of its retired ones -- the exact-identity ownership test
// Provider.List uses (via IdentityStore.KnownSessionIDs) to exclude every
// row agentsctl has ever addressed as its probe, not just the live one.
// Never derived from CWD or DisplayName.
func (id Identity) OwnsSessionID(sessionID string) bool {
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

// AllSessionIDs returns every session ID this identity owns -- the current
// one (if any) followed by every retired one -- for a caller like
// IdentityStore.KnownSessionIDs that needs the full set at once rather
// than a per-ID membership test.
func (id Identity) AllSessionIDs() []string {
	ids := make([]string, 0, len(id.RetiredSessionIDs)+1)
	if id.SessionID != "" {
		ids = append(ids, id.SessionID)
	}
	return append(ids, id.RetiredSessionIDs...)
}

// appendRetiredSessionID returns retired with sessionID added, unless it's
// already present -- Rotate's own dedup guarantee, kept as a separate pure
// function so it's independently testable: a retired list must never grow
// a duplicate entry no matter how many times the same ID is (attempted to
// be) retired.
func appendRetiredSessionID(retired []string, sessionID string) []string {
	for _, r := range retired {
		if r == sessionID {
			return retired
		}
	}
	return append(append([]string{}, retired...), sessionID)
}

// RotateResult is Rotate's result: Identity is always the identity a
// caller should retry with, and Rotated reports whether this call actually
// minted a new SessionID (true) or found that another process had already
// recovered from the same rejection and is simply handing that process's
// own rotation back (false). A caller's retry behavior is identical either
// way; Rotated is exposed only so a caller or test can distinguish the two
// cases when it matters.
type RotateResult struct {
	Identity Identity
	Rotated  bool
}

// IdentityStore is the Root Owner of one probe.json: every mutation of the
// file at path goes through this type's own domain operations, each one a
// complete read-modify-write transaction under withLock. There is no
// exported way to read the file and separately write a caller-assembled
// result back -- readIfExists/write are private, called only by the
// transaction methods below once they already hold the lock.
type IdentityStore struct {
	path string
}

// NewIdentityStore returns a store for the probe identity persisted at
// path (a probe.json).
func NewIdentityStore(path string) *IdentityStore {
	return &IdentityStore{path: path}
}

// KnownSessionIDs reports every exact Claude session ID this probe has
// ever owned -- its current one and every one a rotation has since retired
// (see Identity's own doc comment) -- so Provider.List can exclude all of
// them from the normal catalog. A pure local-file read (no process
// spawned), deliberately NOT serialized behind the same lock every
// mutation takes: writeFileAtomic's rename already guarantees this always
// observes either the complete pre- or complete post-transaction state,
// never a torn mix, and serializing it behind the lock would only make
// List() contend with, and briefly block on, whatever refresh happens to
// be rotating or creating an identity at that exact moment, for no
// correctness benefit. An identity that has never been created, or that
// can't be read, reports an empty/nil slice -- never guessed.
func (s *IdentityStore) KnownSessionIDs() []string {
	id, ok, err := s.readIfExists()
	if err != nil || !ok {
		return nil
	}
	return id.AllSessionIDs()
}

// Load reads the persisted identity without creating one and without
// acquiring the transaction lock -- the read-only counterpart to
// LoadOrCreate, for a caller (or test) that only wants "is there one, and
// if so what is it" without ever wanting to create one. ok is false for a
// missing file, an empty SessionID, or a decode error.
func (s *IdentityStore) Load() (Identity, bool, error) {
	return s.readIfExists()
}

// LoadOrCreate reads the persisted identity, or creates and persists a
// brand-new one (a fresh random session ID, TrustAccepted: false) if none
// exists yet -- one read-modify-write transaction under withLock, so two
// processes racing a cold start (no probe.json yet) can never each mint
// and persist their own different SessionID; the second to acquire the
// lock simply observes the first's now-persisted identity and reuses it.
// This is the ordinary, expected path a new probe session ID is minted
// on -- once per app-data directory's lifetime, after which every refresh
// reuses the same identity. The only other place a new SessionID is ever
// minted is Rotate, an exceptional replacement for a specific identity
// Claude Code has permanently rejected -- unlike this method, that one
// never creates an identity from nothing; it only ever replaces an
// already-existing one.
func (s *IdentityStore) LoadOrCreate() (Identity, error) {
	var result Identity
	err := s.withLock(func() error {
		id, ok, err := s.readIfExists()
		if err != nil {
			return fmt.Errorf("decode claude usage probe identity: %w", err)
		}
		if ok {
			result = id
			return nil
		}
		uuid, err := newUUIDv4()
		if err != nil {
			return err
		}
		id = Identity{SessionID: uuid, DisplayName: probeDisplayName}
		if err := s.write(id); err != nil {
			return err
		}
		result = id
		return nil
	})
	if err != nil {
		return Identity{}, err
	}
	return result, nil
}

// MarkTrustAccepted marks the LATEST persisted identity as TrustAccepted
// -- called once a probe attempt has attempted to answer the
// workspace-trust dialog, regardless of whether a dialog actually needed
// answering, so no later refresh ever repeats that blind keystroke
// sequence into what might by then be a live chat composer instead.
//
// It deliberately takes no Identity argument to merge into, and reads the
// current persisted identity itself, inside the same withLock transaction
// it writes back under. TrustAccepted is directory-level state (see
// Identity's own doc comment): whoever the LATEST persisted current
// identity is when this call actually runs is exactly who should end up
// marked trusted, regardless of which identity the caller happened to be
// looking at when it decided to call this. Concretely, this matters when
// another process's rotation lands between this caller loading its own
// copy and this call actually running: writing that stale copy back
// would silently resurrect a since-rotated-away SessionID as current
// again, discard whatever RetiredSessionIDs that other process's
// rotation had just recorded, and still leave the genuinely-current
// identity (the rotated one) marked untrusted. Reading-and-writing the
// latest persisted identity under the lock instead can never regress
// SessionID or RetiredSessionIDs; it only
// ever adds the one bit this call means to add, to whichever identity is
// actually current the instant it runs.
func (s *IdentityStore) MarkTrustAccepted() error {
	return s.withLock(func() error {
		id, ok, err := s.readIfExists()
		if err != nil {
			return fmt.Errorf("read claude usage probe identity to mark trust accepted: %w", err)
		}
		if !ok {
			return errors.New("claude usage probe: no identity to mark trust accepted")
		}
		id.TrustAccepted = true
		return s.write(id)
	})
}

// Rotate recovers from rejectedSessionID having been rejected by Claude
// Code as already in use: the only recovery available once that happens,
// since the probe's design of otherwise reusing one persisted identity
// forever has no other way to recover from a rejection that never clears
// on its own. Like LoadOrCreate and MarkTrustAccepted, this is one
// read-modify-write transaction under withLock, not just an atomically-
// written file: the read below, the decision built on it, and the
// resulting write all happen while this call alone holds the lock, so no
// other process's own transaction can interleave with it.
//
// It deliberately re-reads the persisted identity itself (inside the
// lock) rather than taking a caller-supplied Identity as the record to
// rotate from -- for two independent reasons, both requiring the current
// on-disk record, not any particular caller's possibly-outdated copy, to
// be the single source of truth for what happens next:
//
//   - TrustAccepted race: a caller can load its own copy before running an
//     attempt that itself persists a TrustAccepted update (via
//     MarkTrustAccepted) partway through that same attempt -- specifically,
//     a session conflict can surface right after the workspace-trust
//     dialog was just answered, before the attempt otherwise succeeds.
//     Rotating from the caller's now-stale in-memory copy would silently
//     regress a TrustAccepted:true that was already durably persisted
//     moments earlier.
//   - Cross-process race: multiple agentsctl processes can share the same
//     probe.json. If another process already rotated rejectedSessionID
//     away by the time this call runs, blindly minting yet another new
//     SessionID here would be redundant -- worse, it would start a
//     rotation storm under any further contention, and would silently
//     discard whatever that other process already retired into
//     RetiredSessionIDs. Comparing rejectedSessionID against the freshly
//     read persisted current -- not a caller's stale copy of what it
//     believed current to be -- is what makes this comparison meaningful:
//     if they still match, this call is the first to react and genuinely
//     owns the rotation; if they don't, someone already handled it and the
//     persisted identity is simply handed back as-is (Rotated: false), no
//     new UUID minted, no new write. Doing this comparison under the lock
//     is what actually closes the race: two callers racing this same
//     method concurrently used to both be able to read the same
//     current==rejectedSessionID, both decide to rotate, and both write --
//     one write clobbering the other and leaving its own newly-minted
//     SessionID owned by neither `current` nor `RetiredSessionIDs`. Under
//     the lock, the second caller's read always observes the first
//     caller's already-completed write, so at most one of them ever sees
//     current == rejectedSessionID and only one new SessionID is ever
//     minted per rejection.
//
// A genuine rotation (rejectedSessionID matches the freshly read persisted
// current) carries DisplayName and TrustAccepted forward unchanged and
// retires the old current SessionID into RetiredSessionIDs (deduplicated)
// so Provider.List keeps excluding it even after it stops being current.
func (s *IdentityStore) Rotate(rejectedSessionID string) (RotateResult, error) {
	var result RotateResult
	err := s.withLock(func() error {
		current, ok, err := s.readIfExists()
		if err != nil {
			return fmt.Errorf("read claude usage probe identity for rotation: %w", err)
		}
		if !ok {
			return errors.New("claude usage probe: no identity to rotate")
		}
		if current.SessionID != rejectedSessionID {
			// Another process already rotated this exact rejection away,
			// and this read -- taken under the same lock that process's
			// own rotation held -- proves it: see this method's own doc
			// comment's "cross-process race".
			result = RotateResult{Identity: current, Rotated: false}
			return nil
		}
		uuid, err := newUUIDv4()
		if err != nil {
			return err
		}
		rotated := Identity{
			SessionID:         uuid,
			DisplayName:       current.DisplayName,
			TrustAccepted:     current.TrustAccepted,
			RetiredSessionIDs: appendRetiredSessionID(current.RetiredSessionIDs, current.SessionID),
		}
		if err := s.write(rotated); err != nil {
			return err
		}
		result = RotateResult{Identity: rotated, Rotated: true}
		return nil
	})
	if err != nil {
		return RotateResult{}, err
	}
	return result, nil
}

// readIfExists reads the persisted identity without creating one and
// without acquiring withLock's transaction lock -- the primitive every
// locked transaction method above reads through once it already holds the
// lock itself, and also Load/KnownSessionIDs's own unlocked read (see
// their doc comments for why that's safe). ok is false for a missing
// file, an empty SessionID, or a decode error.
func (s *IdentityStore) readIfExists() (Identity, bool, error) {
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return Identity{}, false, nil
	}
	if err != nil {
		return Identity{}, false, err
	}
	var id Identity
	if err := json.Unmarshal(b, &id); err != nil {
		return Identity{}, false, err
	}
	if id.SessionID == "" {
		return Identity{}, false, nil
	}
	return id, true, nil
}

// write persists id atomically (see writeFileAtomic) -- lock-free, called
// only by a transaction method that already holds withLock's lock.
func (s *IdentityStore) write(id Identity) error {
	b, err := json.Marshal(id)
	if err != nil {
		return err
	}
	return writeFileAtomic(s.path, b)
}

// withLock runs fn as one read-modify-write transaction under path's
// exclusive advisory lock (path+".lock"), the same unix.Flock-based
// pattern localstate.Store uses -- chosen for the same reason: it needs no
// new dependency, and the kernel releases it automatically if a process
// dies mid-transaction (never a stale lock file that could wedge every
// future refresh). A single blocking unix.Flock(LOCK_EX) call is also
// sufficient to serialize concurrent callers WITHIN one process, not just
// across processes: each call opens its own file description via a fresh
// os.OpenFile, and flock() contends on open file descriptions, not
// processes, so two goroutines in this same process racing for the lock
// block each other exactly as two separate agentsctl processes would -- no
// additional in-process sync.Mutex is needed for correctness.
func (s *IdentityStore) withLock(fn func() error) error {
	unlock, err := lockFile(s.path)
	if err != nil {
		return fmt.Errorf("lock claude usage probe identity: %w", err)
	}
	defer unlock()
	return fn()
}

// lockFile acquires the exclusive advisory lock guarding path's identity
// transactions, blocking until it's available. A failed acquisition
// (directory creation or open failure; unix.Flock itself only fails for
// reasons other than contention, since no LOCK_NB is used here) returns
// before any mutation is attempted.
func lockFile(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN); _ = f.Close() }, nil
}

// newUUIDv4 generates a random RFC 4122 version-4 UUID -- sufficient for
// --session-id (which only requires a valid UUID, not any particular
// generation scheme) without pulling in an external dependency for what is
// otherwise a 16-byte random value with two fixed nibbles.
func newUUIDv4() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
