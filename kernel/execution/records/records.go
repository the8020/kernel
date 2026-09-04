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
	mu      sync.RWMutex
	root    string
	records map[string][]byte
	ids     map[string]bool
	locks   [64]sync.Mutex
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
	store := &Store{root: root, records: map[string][]byte{}, ids: map[string]bool{}}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		id := entry.Name()[:len(entry.Name())-len(".json")]
		store.ids[id] = true
		if data, readErr := os.ReadFile(store.path(id)); readErr == nil {
			store.records[id] = data
		}
	}
	return store, nil
}
func (s *Store) Save(id string, value any) error {
	if !safeID(id) {
		return errors.New("unsafe record ID")
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	lock := s.recordLock(id)
	lock.Lock()
	defer lock.Unlock()
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
	if err := os.Rename(name, s.path(id)); err != nil {
		return err
	}
	s.mu.Lock()
	s.records[id] = append([]byte(nil), data...)
	s.ids[id] = true
	s.mu.Unlock()
	return nil
}
func (s *Store) Load(id string, output any) error {
	if !safeID(id) {
		return errors.New("unsafe record ID")
	}
	s.mu.RLock()
	data, ok := s.records[id]
	s.mu.RUnlock()
	if !ok {
		lock := s.recordLock(id)
		lock.Lock()
		defer lock.Unlock()
		s.mu.RLock()
		data, ok = s.records[id]
		s.mu.RUnlock()
		if !ok {
			var err error
			data, err = os.ReadFile(s.path(id))
			if err != nil {
				return err
			}
			s.mu.Lock()
			s.records[id] = append([]byte(nil), data...)
			s.ids[id] = true
			s.mu.Unlock()
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	return decoder.Decode(output)
}
func (s *Store) IDs() ([]string, error) {
	s.mu.RLock()
	result := make([]string, 0, len(s.ids))
	for id := range s.ids {
		result = append(result, id)
	}
	s.mu.RUnlock()
	sort.Strings(result)
	return result, nil
}
func (s *Store) Delete(id string) error {
	if !safeID(id) {
		return errors.New("unsafe record ID")
	}
	lock := s.recordLock(id)
	lock.Lock()
	defer lock.Unlock()
	err := os.Remove(s.path(id))
	if errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	if err == nil {
		s.mu.Lock()
		delete(s.records, id)
		delete(s.ids, id)
		s.mu.Unlock()
	}
	return err
}

// Quarantine removes one unreadable record from the live registry while
// retaining its exact bytes for diagnosis. A quarantined document is never
// returned by IDs and therefore cannot block unrelated workload recovery.
func (s *Store) Quarantine(id string) (string, error) {
	if !safeID(id) {
		return "", errors.New("unsafe record ID")
	}
	lock := s.recordLock(id)
	lock.Lock()
	defer lock.Unlock()
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
	s.mu.Lock()
	delete(s.records, id)
	delete(s.ids, id)
	s.mu.Unlock()
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

func (s *Store) recordLock(id string) *sync.Mutex {
	var hash uint64 = 1469598103934665603
	for index := 0; index < len(id); index++ {
		hash ^= uint64(id[index])
		hash *= 1099511628211
	}
	return &s.locks[hash%uint64(len(s.locks))]
}
