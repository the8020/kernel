package packages

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestPortableDefaultsRemainLiveWithoutOperatorOverrides(t *testing.T) {
	root := t.TempDir()
	manifest := `schema = 2
description = "Frozen defaults"
[lifecycle]
default_enabled = true
[scaling]
minimum_workers = 0
maximum_workers = 24
concurrency_per_worker = 16
target_utilization = 0.7
[placement]
minimum_sandboxes = 2
workers_per_sandbox = 8
`
	writeFile(t, filepath.Join(root, "packages", "core", "example", "package.toml"), "schema = 1\n")
	writeFile(t, filepath.Join(root, "packages", "core", "example", "services", "api", "service.toml"), manifest)
	writeFile(t, filepath.Join(root, "packages", "core", "example", "services", "api", "service.ts"), "export default {};\n")
	store := newTestStore(t, root)
	first, err := store.ReadService("core/example/api")
	if err != nil {
		t.Fatal(err)
	}
	if first.StateExists || !first.State.Enabled || first.Effective.Placement.MinimumSandboxes != 2 || first.Effective.Scaling.MinimumWorkers != 0 || first.Effective.Scaling.MaximumWorkers != 24 {
		t.Fatalf("first=%#v", first)
	}
	writeFile(t, filepath.Join(root, "packages", "core", "example", "services", "api", "service.toml"), strings.Replace(manifest, "minimum_sandboxes = 2", "minimum_sandboxes = 3", 1))
	second, err := store.ReadService("core/example/api")
	if err != nil {
		t.Fatal(err)
	}
	if second.Effective.Placement.MinimumSandboxes != 3 {
		t.Fatalf("portable edit was hidden by materialized defaults: %#v", second.Effective.Placement)
	}
}

type memoryServiceStateStore struct {
	mu       sync.Mutex
	mutation sync.Mutex
	states   map[string]DesiredServiceState
}

func (s *memoryServiceStateStore) Get(id string) (DesiredServiceState, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, exists := s.states[id]
	return state, exists, nil
}
func (s *memoryServiceStateStore) Put(id string, state DesiredServiceState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.states[id] = state
	return nil
}
func (s *memoryServiceStateStore) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.states, id)
	return nil
}
func (s *memoryServiceStateStore) List() ([]StoredServiceState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]StoredServiceState, 0, len(s.states))
	for id, state := range s.states {
		result = append(result, StoredServiceState{ServiceID: id, State: state})
	}
	return result, nil
}
func (s *memoryServiceStateStore) Lock(context.Context, string) (UnlockFunc, error) {
	s.mutation.Lock()
	return func() error { s.mutation.Unlock(); return nil }, nil
}

func TestSourceInspectionDoesNotMutateReplaceableDesiredStateBackend(t *testing.T) {
	root := t.TempDir()
	writeService(t, root, "core", "example", "api", "service.ts")
	backend := &memoryServiceStateStore{states: map[string]DesiredServiceState{}}
	store, err := New(Config{WorkspaceRoot: root, StateStore: backend, IndexStore: newMemoryPackageIndexStore()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReadService("core/example/api"); err != nil {
		t.Fatal(err)
	}
	if _, exists := backend.states["core/example/api"]; exists {
		t.Fatal("source inspection unexpectedly materialized desired state")
	}
	if _, err := os.Stat(filepath.Join(root, "state", "services")); !os.IsNotExist(err) {
		t.Fatalf("replaceable backend unexpectedly created file state: %v", err)
	}
}
