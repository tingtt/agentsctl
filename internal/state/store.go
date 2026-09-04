package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

type Run struct {
	ID        string    `json:"id"`
	Provider  string    `json:"provider"`
	SessionID string    `json:"sessionId,omitempty"`
	CWD       string    `json:"cwd"`
	PID       int       `json:"pid,omitempty"`
	StartTime uint64    `json:"startTime,omitempty"`
	UID       uint32    `json:"uid,omitempty"`
	Socket    string    `json:"socket,omitempty"`
	State     string    `json:"state"`
	Error     string    `json:"error,omitempty"`
	Baseline  []string  `json:"baseline,omitempty"`
	StartedAt time.Time `json:"startedAt"`
}

type Data struct {
	ClaudeArchived map[string]bool `json:"claudeArchived,omitempty"`
	// ClaudeNames holds agentsctl-local display-name overrides for Claude
	// sessions, keyed by native Claude session ID. Claude's CLI has no
	// headless/native operation to rename an existing background session in
	// place (confirmed: any flag passed to `claude --bg --resume <id>`,
	// including --name, always forks a new session rather than mutating the
	// original's saved options), so this is a display-only overlay — it
	// never touches Claude's own session state or transcript. See
	// provider/claude's Rename/List and the README's "agentsctl-local
	// rename" note.
	ClaudeNames map[string]string `json:"claudeNames,omitempty"`
	Pinned      map[string]bool   `json:"pinned,omitempty"`
	Runs        map[string]Run    `json:"runs,omitempty"`
	Provider    string            `json:"provider,omitempty"`
}

type Store struct {
	path string
	mu   sync.Mutex
}

func New(path string) *Store  { return &Store{path: path} }
func (s *Store) Path() string { return s.path }

func (s *Store) Load() (Data, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.lock(unix.LOCK_SH)
	if err != nil {
		return Data{}, err
	}
	defer unlock()
	return s.load()
}

func (s *Store) Update(fn func(*Data) error) error {
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
	if d.ClaudeArchived == nil {
		d.ClaudeArchived = map[string]bool{}
	}
	if d.ClaudeNames == nil {
		d.ClaudeNames = map[string]string{}
	}
	if d.Pinned == nil {
		d.Pinned = map[string]bool{}
	}
	if d.Runs == nil {
		d.Runs = map[string]Run{}
	}
	return s.save(d)
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

func (s *Store) load() (Data, error) {
	d := Data{ClaudeArchived: map[string]bool{}, ClaudeNames: map[string]string{}, Pinned: map[string]bool{}, Runs: map[string]Run{}}
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
	if d.ClaudeArchived == nil {
		d.ClaudeArchived = map[string]bool{}
	}
	if d.ClaudeNames == nil {
		d.ClaudeNames = map[string]string{}
	}
	if d.Pinned == nil {
		d.Pinned = map[string]bool{}
	}
	if d.Runs == nil {
		d.Runs = map[string]Run{}
	}
	return d, nil
}

// ListPinned returns a copy of the provider-qualified pinned session keys.
func (s *Store) ListPinned() (map[string]bool, error) {
	d, err := s.Load()
	if err != nil {
		return nil, err
	}
	result := make(map[string]bool, len(d.Pinned))
	for key, pinned := range d.Pinned {
		result[key] = pinned
	}
	return result, nil
}

// TogglePinned atomically changes and returns a session's pinned state.
func (s *Store) TogglePinned(key string) (bool, error) {
	pinned := false
	err := s.Update(func(d *Data) error {
		pinned = !d.Pinned[key]
		if pinned {
			d.Pinned[key] = true
		} else {
			delete(d.Pinned, key)
		}
		return nil
	})
	return pinned, err
}

func (s *Store) save(d Data) error {
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
