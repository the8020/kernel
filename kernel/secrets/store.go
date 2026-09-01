// Package secrets owns global named secret persistence. Values remain outside
// sandboxes and are returned only by an explicit read or trusted resolver.
package secrets

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/pelletier/go-toml/v2"
	"golang.org/x/sys/unix"
)

const (
	documentSchema   = 1
	maximumFileSize  = 2 << 20
	maximumValueSize = 64 << 10
)

// Summary deliberately omits the stored value.
type Summary struct {
	Name      string    `json:"name"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Secret is returned only by the explicit get operation.
type Secret struct {
	Name      string    `toml:"name" json:"name"`
	Value     string    `toml:"value" json:"value"`
	UpdatedAt time.Time `toml:"updated_at" json:"updated_at"`
}

type document struct {
	Schema  int      `toml:"schema"`
	Secrets []Secret `toml:"secrets"`
}

type Config struct {
	Path        string
	LockTimeout time.Duration
	Now         func() time.Time
}

type Store struct {
	path        string
	lockPath    string
	lockTimeout time.Duration
	now         func() time.Time
	mu          sync.RWMutex
}

func New(config Config) (*Store, error) {
	if !filepath.IsAbs(config.Path) {
		return nil, errors.New("secrets path must be absolute")
	}
	directory := filepath.Dir(config.Path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, fmt.Errorf("create secrets directory: %w", err)
	}
	directoryInfo, err := os.Lstat(directory)
	if err != nil || !directoryInfo.IsDir() || directoryInfo.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("secrets directory must be a real directory")
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return nil, fmt.Errorf("restrict secrets directory: %w", err)
	}
	if config.LockTimeout <= 0 {
		config.LockTimeout = 5 * time.Second
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	store := &Store{
		path: config.Path, lockPath: config.Path + ".lock",
		lockTimeout: config.LockTimeout, now: config.Now,
	}
	if _, err := store.read(); err != nil {
		return nil, err
	}
	return store, nil
}

func (s *Store) Path() string { return s.path }

func (s *Store) List() ([]Summary, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	document, err := s.read()
	if err != nil {
		return nil, err
	}
	result := make([]Summary, 0, len(document.Secrets))
	for _, secret := range document.Secrets {
		result = append(result, Summary{Name: secret.Name, UpdatedAt: secret.UpdatedAt})
	}
	return result, nil
}

func (s *Store) Get(name string) (Secret, error) {
	name, err := normalizeName(name)
	if err != nil {
		return Secret{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	document, err := s.read()
	if err != nil {
		return Secret{}, err
	}
	for _, secret := range document.Secrets {
		if secret.Name == name {
			return secret, nil
		}
	}
	return Secret{}, fmt.Errorf("secret %q: %w", name, os.ErrNotExist)
}

// SecretValue is the narrow resolver used by kernel-owned operations.
func (s *Store) SecretValue(name string) (string, error) {
	secret, err := s.Get(name)
	if err != nil {
		return "", err
	}
	return secret.Value, nil
}

func (s *Store) Set(ctx context.Context, name, value string) (Summary, error) {
	name, err := normalizeName(name)
	if err != nil {
		return Summary{}, err
	}
	if value == "" {
		return Summary{}, errors.New("secret value is required")
	}
	if !utf8.ValidString(value) {
		return Summary{}, errors.New("secret value must be valid UTF-8")
	}
	if len([]byte(value)) > maximumValueSize {
		return Summary{}, fmt.Errorf("secret value exceeds %d bytes", maximumValueSize)
	}
	if err := ctx.Err(); err != nil {
		return Summary{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.acquire(ctx)
	if err != nil {
		return Summary{}, err
	}
	defer unlock()
	document, err := s.read()
	if err != nil {
		return Summary{}, err
	}
	updated := Secret{Name: name, Value: value, UpdatedAt: s.now().UTC()}
	found := false
	for index := range document.Secrets {
		if document.Secrets[index].Name == name {
			document.Secrets[index] = updated
			found = true
			break
		}
	}
	if !found {
		document.Secrets = append(document.Secrets, updated)
	}
	sort.Slice(document.Secrets, func(i, j int) bool {
		return document.Secrets[i].Name < document.Secrets[j].Name
	})
	if err := s.write(document); err != nil {
		return Summary{}, err
	}
	return Summary{Name: updated.Name, UpdatedAt: updated.UpdatedAt}, nil
}

func normalizeName(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 128 {
		return "", errors.New("secret name must contain 1-128 characters")
	}
	for index, character := range value {
		valid := character >= 'a' && character <= 'z' ||
			character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' ||
			index > 0 && strings.ContainsRune("._-", character)
		if !valid {
			return "", errors.New("secret name must start with a letter or digit and contain only letters, digits, dots, underscores, or hyphens")
		}
	}
	return value, nil
}

// ValidateName applies the canonical syntax used by secret references outside
// this package.
func ValidateName(value string) error {
	normalized, err := normalizeName(value)
	if err != nil {
		return err
	}
	if normalized != value {
		return errors.New("secret name must not contain surrounding whitespace")
	}
	return nil
}

func (s *Store) read() (document, error) {
	metadata, err := os.Lstat(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return document{Schema: documentSchema, Secrets: []Secret{}}, nil
	}
	if err != nil {
		return document{}, fmt.Errorf("inspect secrets: %w", err)
	}
	if !metadata.Mode().IsRegular() || metadata.Mode()&os.ModeSymlink != 0 || metadata.Mode().Perm()&0o077 != 0 || metadata.Size() > maximumFileSize {
		return document{}, errors.New("secrets file must be a bounded private regular file")
	}
	file, err := os.Open(s.path)
	if err != nil {
		return document{}, fmt.Errorf("open secrets: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return document{}, fmt.Errorf("inspect secrets: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || !os.SameFile(metadata, info) || info.Size() > maximumFileSize {
		return document{}, errors.New("secrets file changed while opening")
	}
	data := make([]byte, info.Size())
	if _, err := io.ReadFull(file, data); err != nil {
		return document{}, fmt.Errorf("read secrets: %w", err)
	}
	var decoded document
	if err := toml.Unmarshal(data, &decoded); err != nil {
		return document{}, fmt.Errorf("decode secrets: %w", err)
	}
	if decoded.Schema != documentSchema {
		return document{}, fmt.Errorf("secrets schema must equal %d", documentSchema)
	}
	seen := make(map[string]bool, len(decoded.Secrets))
	for _, secret := range decoded.Secrets {
		name, nameErr := normalizeName(secret.Name)
		if nameErr != nil || name != secret.Name || seen[name] || secret.Value == "" || len([]byte(secret.Value)) > maximumValueSize || secret.UpdatedAt.IsZero() {
			return document{}, errors.New("secrets file contains an invalid entry")
		}
		seen[name] = true
	}
	sort.Slice(decoded.Secrets, func(i, j int) bool {
		return decoded.Secrets[i].Name < decoded.Secrets[j].Name
	})
	return decoded, nil
}

func (s *Store) write(value document) error {
	data, err := toml.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode secrets: %w", err)
	}
	directory := filepath.Dir(s.path)
	temporary, err := os.CreateTemp(directory, ".secrets-*.tmp")
	if err != nil {
		return fmt.Errorf("create secrets temporary file: %w", err)
	}
	name := temporary.Name()
	defer os.Remove(name)
	if err = temporary.Chmod(0o600); err == nil {
		_, err = temporary.Write(data)
	}
	if err == nil {
		err = temporary.Sync()
	}
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(name, s.path)
	}
	if err == nil {
		err = syncDirectory(directory)
	}
	if err != nil {
		return fmt.Errorf("write secrets: %w", err)
	}
	return nil
}

func (s *Store) acquire(ctx context.Context) (func(), error) {
	file, err := os.OpenFile(s.lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open secrets lock: %w", err)
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("restrict secrets lock: %w", err)
	}
	deadline := time.Now().Add(s.lockTimeout)
	for {
		err = unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			var once sync.Once
			return func() {
				once.Do(func() {
					_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
					_ = file.Close()
				})
			}, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			_ = file.Close()
			return nil, fmt.Errorf("lock secrets: %w", err)
		}
		if time.Now().After(deadline) {
			_ = file.Close()
			return nil, errors.New("timed out acquiring secrets lock")
		}
		select {
		case <-ctx.Done():
			_ = file.Close()
			return nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
