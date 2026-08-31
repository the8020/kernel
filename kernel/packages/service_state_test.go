package packages

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestFileServiceStateStoreCRUDListAndIdempotentDelete(t *testing.T) {
	store, err := NewFileServiceStateStore(filepath.Join(t.TempDir(), "state", "services"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	state := DesiredServiceState{Schema: 1, Enabled: true, Generation: 4}
	if err := store.Put("core/example/service", state); err != nil {
		t.Fatal(err)
	}
	loaded, exists, err := store.Get("core/example/service")
	if err != nil || !exists || loaded.Generation != 4 {
		t.Fatalf("loaded=%#v exists=%t err=%v", loaded, exists, err)
	}
	records, err := store.List()
	if err != nil || len(records) != 1 || records[0].ServiceID != "core/example/service" {
		t.Fatalf("records=%#v err=%v", records, err)
	}
	if err := store.Delete("core/example/service"); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete("core/example/service"); err != nil {
		t.Fatalf("idempotent delete: %v", err)
	}
}

func TestFirstDiscoveryFreezesPortableDefaults(t *testing.T) {
	root := t.TempDir()
	manifest := `schema = 1
description = "Frozen defaults"
[lifecycle]
default_enabled = true
[execution]
concurrency_per_worker = 16
[scaling]
replicas_min = 2
replicas_max = 3
workers_per_replica_min = 0
workers_per_replica_max = 8
target_utilization = 0.7
`
	writeFile(t, filepath.Join(root, "packages", "core", "example", "package.toml"), "schema = 1\n")
	writeFile(t, filepath.Join(root, "packages", "core", "example", "services", "api", "service.toml"), manifest)
	writeFile(t, filepath.Join(root, "packages", "core", "example", "services", "api", "service.ts"), "export default {};\n")
	store := newTestStore(t, root)
	first, err := store.ReadService("core/example/api")
	if err != nil {
		t.Fatal(err)
	}
	if !first.StateExists || !first.State.Enabled || first.Effective.Scaling.ReplicasMinimum != 2 || first.Effective.Scaling.WorkersPerReplicaMinimum != 0 {
		t.Fatalf("first=%#v", first)
	}
	writeFile(t, filepath.Join(root, "packages", "core", "example", "services", "api", "service.toml"), strings.Replace(manifest, "replicas_min = 2", "replicas_min = 3", 1))
	second, err := store.ReadService("core/example/api")
	if err != nil {
		t.Fatal(err)
	}
	if second.Effective.Scaling.ReplicasMinimum != 2 {
		t.Fatalf("portable edit changed materialized desired state: %#v", second.Effective.Scaling)
	}
}

type memoryServiceStateStore struct {
	mu     sync.Mutex
	states map[string]DesiredServiceState
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
	return func() error { return nil }, nil
}

func TestDefinitionStoreUsesReplaceableDesiredStateBackend(t *testing.T) {
	root := t.TempDir()
	writeService(t, root, "core", "example", "api", "service.ts")
	backend := &memoryServiceStateStore{states: map[string]DesiredServiceState{}}
	store, err := New(Config{WorkspaceRoot: root, StateStore: backend})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReadService("core/example/api"); err != nil {
		t.Fatal(err)
	}
	if _, exists := backend.states["core/example/api"]; !exists {
		t.Fatal("definition store bypassed configured desired-state backend")
	}
	if _, err := os.Stat(filepath.Join(root, "state", "services")); !os.IsNotExist(err) {
		t.Fatalf("replaceable backend unexpectedly created file state: %v", err)
	}
}

func TestServiceStateMutationProcess(t *testing.T) {
	root := os.Getenv("THE8020_TEST_SERVICE_STATE_ROOT")
	if root == "" {
		t.Skip("helper process")
	}
	store, err := New(Config{WorkspaceRoot: root, StateLockTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.MutateState(context.Background(), "core/example/api", func(state *DesiredServiceState) error {
		state.Enabled = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestDesiredStateWritesAreSerializedAcrossProcesses(t *testing.T) {
	root := t.TempDir()
	writeService(t, root, "core", "example", "api", "service.ts")
	store := newTestStore(t, root)
	if _, err := store.ReadService("core/example/api"); err != nil {
		t.Fatal(err)
	}
	commands := []*exec.Cmd{
		exec.Command(os.Args[0], "-test.run=^TestServiceStateMutationProcess$"),
		exec.Command(os.Args[0], "-test.run=^TestServiceStateMutationProcess$"),
	}
	outputs := make([]bytes.Buffer, len(commands))
	for index, command := range commands {
		command.Env = append(os.Environ(), "THE8020_TEST_SERVICE_STATE_ROOT="+root)
		command.Stdout, command.Stderr = &outputs[index], &outputs[index]
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
	}
	for index, command := range commands {
		if err := command.Wait(); err != nil {
			t.Fatalf("child mutation: %v\n%s", err, outputs[index].String())
		}
	}
	state, exists, err := store.ReadState("core/example/api")
	if err != nil || !exists || state.Generation != 2 {
		t.Fatalf("state=%#v exists=%t err=%v", state, exists, err)
	}
}
