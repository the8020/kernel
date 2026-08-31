// Package state persists sandbox desired and observed state atomically.
package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"the8020/kernel/sandbox/model"
)

type Store struct {
	mu   sync.Mutex
	root string
}

func New(root string) (*Store, error) {
	if root == "" {
		return nil, errors.New("runtime-group state root is required")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create runtime-group state: %w", err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return nil, fmt.Errorf("restrict runtime-group state: %w", err)
	}
	return &Store{root: root}, nil
}

func (s *Store) SaveSpec(spec model.SandboxSpec) error {
	if err := spec.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.write(spec.RuntimeGroupID, "spec.json", spec); err != nil {
		return err
	}
	if spec.InternalToken != "" {
		return s.write(spec.RuntimeGroupID, "secret.json", map[string]string{"internal_token": spec.InternalToken})
	}
	return nil
}

func (s *Store) SaveStatus(runtimeGroupID string, status model.SandboxStatus) error {
	if runtimeGroupID == "" || !status.DesiredState.Valid() || !status.ObservedState.Valid() {
		return errors.New("runtime-group ID and valid desired/observed states are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.write(runtimeGroupID, "state.json", status)
}

// UpdateStatus applies a read-modify-write while holding the store lock. It is
// used when independent callback and monitor paths update different fields.
func (s *Store) UpdateStatus(runtimeGroupID string, update func(*model.SandboxStatus) error) (model.SandboxStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var status model.SandboxStatus
	if err := s.read(runtimeGroupID, "state.json", &status); err != nil {
		return status, err
	}
	if update != nil {
		if err := update(&status); err != nil {
			return status, err
		}
	}
	if !status.DesiredState.Valid() || !status.ObservedState.Valid() {
		return status, errors.New("valid desired and observed states are required")
	}
	if err := s.write(runtimeGroupID, "state.json", status); err != nil {
		return model.SandboxStatus{}, err
	}
	return status, nil
}

func (s *Store) Transition(runtimeGroupID string, observed model.SandboxState, update func(*model.SandboxStatus)) (model.SandboxStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var status model.SandboxStatus
	if err := s.read(runtimeGroupID, "state.json", &status); err != nil {
		return status, err
	}
	if !model.ValidTransition(status.ObservedState, observed) {
		return status, fmt.Errorf("invalid sandbox transition %s -> %s", status.ObservedState, observed)
	}
	status.ObservedState = observed
	if update != nil {
		update(&status)
	}
	if err := s.write(runtimeGroupID, "state.json", status); err != nil {
		return model.SandboxStatus{}, err
	}
	return status, nil
}

// TransitionIf atomically re-checks a lifecycle predicate before publishing a
// transition. A false predicate leaves state unchanged.
func (s *Store) TransitionIf(runtimeGroupID string, observed model.SandboxState, condition func(model.SandboxStatus) bool, update func(*model.SandboxStatus)) (model.SandboxStatus, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var status model.SandboxStatus
	if err := s.read(runtimeGroupID, "state.json", &status); err != nil {
		return status, false, err
	}
	if condition != nil && !condition(status) {
		return status, false, nil
	}
	if !model.ValidTransition(status.ObservedState, observed) {
		return status, false, fmt.Errorf("invalid sandbox transition %s -> %s", status.ObservedState, observed)
	}
	status.ObservedState = observed
	if update != nil {
		update(&status)
	}
	if err := s.write(runtimeGroupID, "state.json", status); err != nil {
		return model.SandboxStatus{}, false, err
	}
	return status, true, nil
}

func (s *Store) Load(runtimeGroupID string) (model.SandboxSpec, model.SandboxStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var spec model.SandboxSpec
	var status model.SandboxStatus
	if err := s.read(runtimeGroupID, "spec.json", &spec); err != nil {
		return spec, status, err
	}
	if err := s.read(runtimeGroupID, "state.json", &status); err != nil {
		return spec, status, err
	}
	if spec.RuntimeGroupID != runtimeGroupID {
		return spec, status, errors.New("runtime-group state identity mismatch")
	}
	var secret struct {
		InternalToken string `json:"internal_token"`
	}
	if err := s.read(runtimeGroupID, "secret.json", &secret); err == nil {
		spec.InternalToken = secret.InternalToken
	} else if !errors.Is(err, os.ErrNotExist) {
		return spec, status, err
	}
	return spec, status, nil
}

func (s *Store) List() ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return nil, err
	}
	var result []string
	for _, entry := range entries {
		if entry.IsDir() {
			result = append(result, entry.Name())
		}
	}
	sort.Strings(result)
	return result, nil
}

func (s *Store) Delete(runtimeGroupID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	directory := filepath.Join(s.root, runtimeGroupID)
	for _, name := range []string{"spec.json", "state.json", "secret.json"} {
		if err := os.Remove(filepath.Join(directory, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := os.Remove(directory); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (s *Store) write(runtimeGroupID, name string, value any) error {
	if runtimeGroupID == "" || filepath.Base(runtimeGroupID) != runtimeGroupID {
		return errors.New("invalid runtime-group ID")
	}
	directory := filepath.Join(s.root, runtimeGroupID)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	temporary, err := os.CreateTemp(directory, ".state-*")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
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
	return os.Rename(temporaryName, filepath.Join(directory, name))
}

func (s *Store) read(runtimeGroupID, name string, output any) error {
	if runtimeGroupID == "" || filepath.Base(runtimeGroupID) != runtimeGroupID {
		return errors.New("invalid runtime-group ID")
	}
	data, err := os.ReadFile(filepath.Join(s.root, runtimeGroupID, name))
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		return fmt.Errorf("decode %s for %s: %w", name, runtimeGroupID, err)
	}
	return nil
}
