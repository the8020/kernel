// Package records persists small workload registry documents atomically.
package records

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

type Store struct {
	mu   sync.RWMutex
	root string
}

func New(root string) (*Store, error) {
	if root == "" {
		return nil, errors.New("record root is required")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return nil, err
	}
	return &Store{root: root}, nil
}
func (s *Store) Save(id string, value any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !safeID(id) {
		return errors.New("unsafe record ID")
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	temporary, err := os.CreateTemp(s.root, ".record-*.tmp")
	if err != nil {
		return err
	}
	name := temporary.Name()
	defer os.Remove(name)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(name, s.path(id))
}
func (s *Store) Load(id string, output any) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !safeID(id) {
		return errors.New("unsafe record ID")
	}
	data, err := os.ReadFile(s.path(id))
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	return decoder.Decode(output)
}
func (s *Store) IDs() ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return nil, err
	}
	var result []string
	for _, entry := range entries {
		if !entry.IsDir() && filepath.Ext(entry.Name()) == ".json" {
			result = append(result, entry.Name()[:len(entry.Name())-len(".json")])
		}
	}
	sort.Strings(result)
	return result, nil
}
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !safeID(id) {
		return errors.New("unsafe record ID")
	}
	err := os.Remove(s.path(id))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// Quarantine removes one unreadable record from the live registry while
// retaining its exact bytes for diagnosis. A quarantined document is never
// returned by IDs and therefore cannot block unrelated workload recovery.
func (s *Store) Quarantine(id string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !safeID(id) {
		return "", errors.New("unsafe record ID")
	}
	root := filepath.Join(s.root, "quarantine")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return "", err
	}
	placeholder, err := os.CreateTemp(root, id+"-*.json")
	if err != nil {
		return "", err
	}
	target := placeholder.Name()
	if closeErr := placeholder.Close(); closeErr != nil {
		_ = os.Remove(target)
		return "", closeErr
	}
	if err := os.Remove(target); err != nil {
		return "", err
	}
	if err := os.Rename(s.path(id), target); err != nil {
		return "", err
	}
	return target, nil
}

func (s *Store) path(id string) string { return filepath.Join(s.root, id+".json") }
func safeID(id string) bool {
	if id == "" {
		return false
	}
	for _, character := range id {
		if !((character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || character == '-' || character == '_') {
			return false
		}
	}
	return true
}
