package packages

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/pelletier/go-toml/v2"
	"golang.org/x/sys/unix"
)

type UnlockFunc func() error

type StoredServiceState struct {
	ServiceID string              `json:"service_id"`
	State     DesiredServiceState `json:"state"`
}

// ServiceStateStore is the replaceable persistence boundary used by service
// discovery and reconciliation. Implementations must make Put atomic to
// concurrent readers; callers use Lock for read-modify-write transactions.
type ServiceStateStore interface {
	Get(serviceID string) (DesiredServiceState, bool, error)
	Put(serviceID string, state DesiredServiceState) error
	Delete(serviceID string) error
	List() ([]StoredServiceState, error)
	Lock(ctx context.Context, serviceID string) (UnlockFunc, error)
}

// FileServiceStateStore persists shared service state below state/services.
type FileServiceStateStore struct {
	root        string
	lockTimeout time.Duration
}

func NewFileServiceStateStore(root string, lockTimeout time.Duration) (*FileServiceStateStore, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("create service state root: %w", err)
	}
	canonical, err := canonicalDirectory(root)
	if err != nil {
		return nil, fmt.Errorf("service state root: %w", err)
	}
	if lockTimeout <= 0 {
		lockTimeout = 5 * time.Second
	}
	return &FileServiceStateStore{root: canonical, lockTimeout: lockTimeout}, nil
}

func (s *FileServiceStateStore) Root() string { return s.root }

func (s *FileServiceStateStore) Get(serviceID string) (DesiredServiceState, bool, error) {
	identity, err := ParseServiceID(serviceID)
	if err != nil {
		return DesiredServiceState{}, false, err
	}
	path := s.statePath(identity)
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return DesiredServiceState{}, false, nil
	} else if err != nil {
		return DesiredServiceState{}, false, fmt.Errorf("inspect service state %s: %w", path, err)
	}
	canonical, err := canonicalWithin(path, s.root)
	if err != nil {
		return DesiredServiceState{}, false, fmt.Errorf("service state %s: %w", path, err)
	}
	if canonical != path {
		return DesiredServiceState{}, false, fmt.Errorf("service state %s resolves through a symlink", path)
	}
	var state DesiredServiceState
	if err := decodeTOMLFile(path, &state); err != nil {
		return DesiredServiceState{}, true, fmt.Errorf("service state %s: %w", path, err)
	}
	if state.Schema != manifestSchema {
		return DesiredServiceState{}, true, fmt.Errorf("service state %s: schema must equal %d", path, manifestSchema)
	}
	return state, true, nil
}

func (s *FileServiceStateStore) Put(serviceID string, state DesiredServiceState) error {
	identity, err := ParseServiceID(serviceID)
	if err != nil {
		return err
	}
	if state.Schema != manifestSchema {
		return fmt.Errorf("service state schema must equal %d", manifestSchema)
	}
	directory, err := ensureServiceStateDirectory(s.root, identity)
	if err != nil {
		return err
	}
	data, err := toml.Marshal(state)
	if err != nil {
		return fmt.Errorf("encode service state: %w", err)
	}
	return writeAtomicFile(directory, s.statePath(identity), data, 0o644)
}

func (s *FileServiceStateStore) Delete(serviceID string) error {
	identity, err := ParseServiceID(serviceID)
	if err != nil {
		return err
	}
	path := s.statePath(identity)
	if err := os.Remove(path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("delete service state %s: %w", path, err)
	}
	return syncDirectory(filepath.Dir(path))
}

func (s *FileServiceStateStore) List() ([]StoredServiceState, error) {
	ids, err := listStateServiceIDs(s.root)
	if err != nil {
		return nil, err
	}
	result := make([]StoredServiceState, 0, len(ids))
	for _, serviceID := range ids {
		state, exists, getErr := s.Get(serviceID)
		if getErr != nil {
			return nil, getErr
		}
		if exists {
			result = append(result, StoredServiceState{ServiceID: serviceID, State: state})
		}
	}
	return result, nil
}

func (s *FileServiceStateStore) Lock(ctx context.Context, serviceID string) (UnlockFunc, error) {
	identity, err := ParseServiceID(serviceID)
	if err != nil {
		return nil, err
	}
	directory, err := ensureServiceStateDirectory(s.root, identity)
	if err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(directory, ".state.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open service state lock: %w", err)
	}
	deadline := time.Now().Add(s.lockTimeout)
	for {
		err = unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			var once sync.Once
			var unlockErr error
			return func() error {
				once.Do(func() {
					unlockErr = errors.Join(unix.Flock(int(file.Fd()), unix.LOCK_UN), file.Close())
				})
				return unlockErr
			}, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			_ = file.Close()
			return nil, fmt.Errorf("lock service state: %w", err)
		}
		if time.Now().After(deadline) {
			_ = file.Close()
			return nil, errors.New("timed out acquiring service state lock")
		}
		select {
		case <-ctx.Done():
			_ = file.Close()
			return nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func (s *FileServiceStateStore) statePath(identity Identity) string {
	return filepath.Join(s.root, identity.Namespace, identity.Repository, identity.Service, "state.toml")
}

func listStateServiceIDs(root string) ([]string, error) {
	namespaces, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var result []string
	for _, namespace := range namespaces {
		if !namespace.IsDir() || ValidateName(namespace.Name()) != nil {
			continue
		}
		repositories, err := os.ReadDir(filepath.Join(root, namespace.Name()))
		if err != nil {
			return nil, err
		}
		for _, repository := range repositories {
			if !repository.IsDir() || ValidateName(repository.Name()) != nil {
				continue
			}
			services, err := os.ReadDir(filepath.Join(root, namespace.Name(), repository.Name()))
			if err != nil {
				return nil, err
			}
			for _, service := range services {
				if !service.IsDir() || ValidateName(service.Name()) != nil {
					continue
				}
				if info, statErr := os.Lstat(filepath.Join(root, namespace.Name(), repository.Name(), service.Name(), "state.toml")); statErr == nil && info.Mode().IsRegular() {
					result = append(result, namespace.Name()+"/"+repository.Name()+"/"+service.Name())
				}
			}
		}
	}
	sort.Strings(result)
	return result, nil
}

func ensureServiceStateDirectory(root string, identity Identity) (string, error) {
	directory := root
	for _, name := range []string{identity.Namespace, identity.Repository, identity.Service} {
		next := filepath.Join(directory, name)
		info, err := os.Lstat(next)
		if errors.Is(err, os.ErrNotExist) {
			if err := os.Mkdir(next, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
				return "", fmt.Errorf("create service state directory %s: %w", next, err)
			}
			info, err = os.Lstat(next)
		}
		if err != nil {
			return "", fmt.Errorf("inspect service state directory %s: %w", next, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return "", fmt.Errorf("service state directory %s must be a real directory", next)
		}
		canonical, err := canonicalWithin(next, root)
		if err != nil || canonical != next {
			if err == nil {
				err = errors.New("directory resolves through a symlink")
			}
			return "", fmt.Errorf("service state directory %s: %w", next, err)
		}
		directory = next
	}
	return directory, nil
}

func writeAtomicFile(directory, destination string, data []byte, mode os.FileMode) error {
	temporary, err := os.CreateTemp(directory, ".state-*.tmp")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(mode); err == nil {
		_, err = temporary.Write(data)
	}
	if err == nil {
		err = temporary.Sync()
	}
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("write atomic file: %w", err)
	}
	if err := os.Rename(temporaryName, destination); err != nil {
		return fmt.Errorf("replace %s: %w", destination, err)
	}
	return syncDirectory(directory)
}

func syncDirectory(directory string) error {
	file, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync directory %s: %w", directory, err)
	}
	return nil
}
