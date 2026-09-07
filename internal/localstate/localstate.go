// Package localstate is the Root Owner of agentsctl's local persisted
// state: pin metadata, the Claude archive overlay, the legacy Claude
// rename-name fallback, and Codex managed-run records (see the DesignDoc's
// "Native state and local overlays" -- this is supplemental state only,
// never a substitute for provider-native state).
//
// The raw JSON schema (data, run in schema.go) is private. Every other
// package reaches persisted state exclusively through Store's
// domain-meaning operations (pins.go, claude.go, runs.go) -- never a raw
// read-modify-write against the schema itself (see the DesignDoc's
// "Encapsulate local persistence schema").
package localstate

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/sys/unix"
)

// Store is the single Root Owner of one state.json file. It is safe for
// concurrent use both within one process (mu) and across processes (the
// advisory file lock every Load/update takes): multiple agentsctl
// processes (a TUI plus the Codex supervisor daemon) share one Store path.
type Store struct {
	path string
	mu   sync.Mutex
}

func New(path string) *Store  { return &Store{path: path} }
func (s *Store) Path() string { return s.path }

// update performs one exclusive-locked read-modify-write transaction: fn
// observes and mutates a private snapshot, and the result is atomically
// persisted only if fn returns nil. This is the single mutation primitive
// every domain operation in pins.go/claude.go/runs.go is built on; it is
// deliberately unexported so no package outside localstate can perform a
// raw schema mutation (see the package doc comment).
func (s *Store) update(fn func(*data) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.lock(unix.LOCK_EX)
	if err != nil {
		return err
	}
	defer unlock()
	d, err := s.load()
	if err != nil {
		return err
	}
	if err := fn(&d); err != nil {
		return err
	}
	d.normalize()
	return s.save(d)
}

// view performs one shared-locked read, for a domain operation that only
// needs to observe current state.
func (s *Store) view() (data, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.lock(unix.LOCK_SH)
	if err != nil {
		return data{}, err
	}
	defer unlock()
	return s.load()
}

func (s *Store) lock(mode int) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(s.path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), mode); err != nil {
		f.Close()
		return nil, err
	}
	return func() { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN); _ = f.Close() }, nil
}

func (s *Store) load() (data, error) {
	d := emptyData()
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return d, nil
	}
	if err != nil {
		return d, fmt.Errorf("read state: %w", err)
	}
	if err := json.Unmarshal(b, &d); err != nil {
		return d, fmt.Errorf("decode state: %w", err)
	}
	d.normalize()
	return d, nil
}

func (s *Store) save(d data) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".state-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, s.path)
}
