package packages

import (
	"context"
	"sync"
)

type memoryPackageIndexStore struct {
	mu      sync.Mutex
	entries map[string]PackageIndex
}

func newMemoryPackageIndexStore() *memoryPackageIndexStore {
	return &memoryPackageIndexStore{entries: map[string]PackageIndex{}}
}

func (s *memoryPackageIndexStore) List(context.Context) ([]PackageIndex, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]PackageIndex, 0, len(s.entries))
	for _, entry := range s.entries {
		result = append(result, entry)
	}
	return result, nil
}

func (s *memoryPackageIndexStore) Get(_ context.Context, packageID string) (PackageIndex, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, exists := s.entries[packageID]
	return entry, exists, nil
}

func (s *memoryPackageIndexStore) Put(_ context.Context, entry PackageIndex) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry.Valid = true
	s.entries[entry.PackageID] = entry
	return nil
}

func (*memoryPackageIndexStore) SetActivation(context.Context, string, string, string, error) error {
	return nil
}

func (*memoryPackageIndexStore) Revision(context.Context) (uint64, error) { return 0, nil }
