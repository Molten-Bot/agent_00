package workflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

var ErrConflict = errors.New("workflows changed; reload before saving")

type Store struct {
	Path string
	mu   sync.Mutex
}

func (s *Store) Load() (Document, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.load() }
func (s *Store) load() (Document, error) {
	data, err := os.ReadFile(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return Document{Version: Version, Workflows: []Definition{}}, nil
	}
	if err != nil {
		return Document{}, err
	}
	return Decode(data)
}
func (s *Store) Save(d Document) (Document, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	old, err := s.load()
	if err != nil {
		return d, err
	}
	if old.Revision != d.Revision {
		return old, ErrConflict
	}
	if err = d.Validate(); err != nil {
		return old, err
	}
	d.Revision++
	err = writeAtomic(s.Path, d)
	return d, err
}

// Initialize never overwrites an existing document, including an unsupported version.
func (s *Store) Initialize(d Document) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := os.Stat(s.Path); err == nil {
		_, err = s.load()
		return err
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := d.Validate(); err != nil {
		return err
	}
	return writeAtomic(s.Path, d)
}
func writeAtomic(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".workflow-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if _, err = f.Write(append(data, '\n')); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return fmt.Errorf("replace workflow file: %w", err)
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
