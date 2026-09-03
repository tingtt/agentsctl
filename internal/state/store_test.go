package state

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

func TestStorePersistsOwnedMetadata(t *testing.T) {
	s := New(filepath.Join(t.TempDir(), "state.json"))
	if err := s.Update(func(d *Data) error {
		d.ClaudeArchived["c1"] = true
		d.Runs["r1"] = Run{ID: "r1", State: "starting"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	d, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !d.ClaudeArchived["c1"] || d.Runs["r1"].State != "starting" {
		t.Fatalf("data=%+v", d)
	}
}

func TestStoreSerializesConcurrentProcessesOwners(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	a, b := New(path), New(path)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s := a
			if i%2 == 1 {
				s = b
			}
			if err := s.Update(func(d *Data) error { d.Runs[fmt.Sprint(i)] = Run{ID: fmt.Sprint(i)}; return nil }); err != nil {
				t.Errorf("update: %v", err)
			}
		}(i)
	}
	wg.Wait()
	d, err := a.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Runs) != 20 {
		t.Fatalf("runs=%d", len(d.Runs))
	}
}
