package auth

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/pelletier/go-toml/v2"
)

const authSchema = 1

var (
	ErrDuplicateUser      = errors.New("bootstrap administrator already exists")
	ErrUserNotFound       = errors.New("bootstrap administrator not found")
	ErrInvalidUsername    = errors.New("invalid bootstrap administrator username")
	ErrInvalidCredentials = errors.New("invalid credentials")
	ErrUserDisabled       = errors.New("bootstrap administrator is disabled")
)

type UserRecord struct {
	Username     string    `toml:"username" json:"username"`
	PasswordHash string    `toml:"password_hash" json:"-"`
	Enabled      bool      `toml:"enabled" json:"enabled"`
	AuthVersion  uint64    `toml:"auth_version" json:"auth_version"`
	CreatedAt    time.Time `toml:"created_at" json:"created_at"`
	UpdatedAt    time.Time `toml:"updated_at" json:"updated_at"`
}

func (u UserRecord) ID() string { return "bootstrap-admin:" + u.Username }

type userDocument struct {
	Schema int          `toml:"schema"`
	Users  []UserRecord `toml:"users"`
}

type UserStoreConfig struct {
	Path        string
	Hasher      *PasswordHasher
	LockTimeout time.Duration
	Now         func() time.Time
}

type UserStore struct {
	path        string
	lockPath    string
	hasher      *PasswordHasher
	lockTimeout time.Duration
	now         func() time.Time
}

func NewUserStore(config UserStoreConfig) (*UserStore, error) {
	if config.Path == "" || config.Hasher == nil {
		return nil, errors.New("bootstrap user path and password hasher are required")
	}
	path, err := filepath.Abs(config.Path)
	if err != nil {
		return nil, fmt.Errorf("resolve bootstrap user path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create bootstrap user directory: %w", err)
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	store := &UserStore{path: path, lockPath: filepath.Join(filepath.Dir(path), "bootstrap-users.lock"), hasher: config.Hasher, lockTimeout: config.LockTimeout, now: config.Now}
	unlock, err := acquireFileLock(context.Background(), store.lockPath, store.lockTimeout)
	if err != nil {
		return nil, err
	}
	defer unlock()
	info, err := os.Lstat(store.path)
	if errors.Is(err, os.ErrNotExist) {
		if err := store.write(userDocument{Schema: authSchema, Users: []UserRecord{}}); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, fmt.Errorf("inspect bootstrap user file: %w", err)
	} else {
		if !info.Mode().IsRegular() {
			return nil, errors.New("bootstrap user file must be a regular file")
		}
		if err := os.Chmod(store.path, 0o600); err != nil {
			return nil, fmt.Errorf("restrict bootstrap user file: %w", err)
		}
		if _, err := store.read(); err != nil {
			return nil, err
		}
	}
	return store, nil
}

func (s *UserStore) Path() string { return s.path }

func (s *UserStore) List() ([]UserRecord, error) {
	document, err := s.read()
	if err != nil {
		return nil, err
	}
	return append([]UserRecord(nil), document.Users...), nil
}

func (s *UserStore) Get(username string) (UserRecord, bool, error) {
	document, err := s.read()
	if err != nil {
		return UserRecord{}, false, err
	}
	for _, user := range document.Users {
		if user.Username == username {
			return user, true, nil
		}
	}
	return UserRecord{}, false, nil
}

func (s *UserStore) Authenticate(username, password string) (UserRecord, error) {
	return s.AuthenticateBytes(username, []byte(password))
}

// AuthenticateBytes authenticates mutable secret input without retaining it.
func (s *UserStore) AuthenticateBytes(username string, password []byte) (UserRecord, error) {
	user, exists, err := s.Get(username)
	if err != nil {
		return UserRecord{}, err
	}
	if !exists {
		s.hasher.VerifyUnknownBytes(password)
		return UserRecord{}, ErrInvalidCredentials
	}
	valid, err := s.hasher.VerifyBytes(user.PasswordHash, password)
	if err != nil {
		return UserRecord{}, fmt.Errorf("verify bootstrap administrator password: %w", err)
	}
	if !valid {
		return UserRecord{}, ErrInvalidCredentials
	}
	if !user.Enabled {
		return UserRecord{}, ErrUserDisabled
	}
	return user, nil
}

func (s *UserStore) Add(ctx context.Context, username, password string) (UserRecord, error) {
	if err := validateUsername(username); err != nil {
		return UserRecord{}, err
	}
	passwordHash, err := s.hasher.Hash(password)
	if err != nil {
		return UserRecord{}, err
	}
	now := s.now().UTC()
	created := UserRecord{Username: username, PasswordHash: passwordHash, Enabled: true, AuthVersion: 1, CreatedAt: now, UpdatedAt: now}
	err = s.mutate(ctx, func(document *userDocument) error {
		for _, user := range document.Users {
			if user.Username == username {
				return ErrDuplicateUser
			}
		}
		document.Users = append(document.Users, created)
		return nil
	})
	if err != nil {
		return UserRecord{}, err
	}
	return created, nil
}

func (s *UserStore) Remove(ctx context.Context, username string) error {
	return s.mutate(ctx, func(document *userDocument) error {
		for index, user := range document.Users {
			if user.Username == username {
				document.Users = append(document.Users[:index], document.Users[index+1:]...)
				return nil
			}
		}
		return ErrUserNotFound
	})
}

func (s *UserStore) Enable(ctx context.Context, username string) (UserRecord, error) {
	return s.update(ctx, username, func(user *UserRecord) error {
		if !user.Enabled {
			user.Enabled = true
			user.UpdatedAt = s.now().UTC()
		}
		return nil
	})
}

func (s *UserStore) Disable(ctx context.Context, username string) (UserRecord, error) {
	return s.update(ctx, username, func(user *UserRecord) error {
		if user.Enabled {
			user.Enabled = false
			user.AuthVersion++
			user.UpdatedAt = s.now().UTC()
		}
		return nil
	})
}

func (s *UserStore) SetPassword(ctx context.Context, username, password string) (UserRecord, error) {
	passwordHash, err := s.hasher.Hash(password)
	if err != nil {
		return UserRecord{}, err
	}
	return s.update(ctx, username, func(user *UserRecord) error {
		user.PasswordHash = passwordHash
		user.AuthVersion++
		user.UpdatedAt = s.now().UTC()
		return nil
	})
}

func (s *UserStore) InvalidateSessions(ctx context.Context, username string) (UserRecord, error) {
	return s.update(ctx, username, func(user *UserRecord) error {
		user.AuthVersion++
		user.UpdatedAt = s.now().UTC()
		return nil
	})
}

func (s *UserStore) update(ctx context.Context, username string, apply func(*UserRecord) error) (UserRecord, error) {
	var updated UserRecord
	err := s.mutate(ctx, func(document *userDocument) error {
		for index := range document.Users {
			if document.Users[index].Username == username {
				if err := apply(&document.Users[index]); err != nil {
					return err
				}
				updated = document.Users[index]
				return nil
			}
		}
		return ErrUserNotFound
	})
	return updated, err
}

func (s *UserStore) mutate(ctx context.Context, apply func(*userDocument) error) error {
	unlock, err := acquireFileLock(ctx, s.lockPath, s.lockTimeout)
	if err != nil {
		return err
	}
	defer unlock()
	document, err := s.read()
	if err != nil {
		return err
	}
	if err := apply(&document); err != nil {
		return err
	}
	sort.Slice(document.Users, func(i, j int) bool { return document.Users[i].Username < document.Users[j].Username })
	return s.write(document)
}

func (s *UserStore) read() (userDocument, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return userDocument{}, fmt.Errorf("read bootstrap user file: %w", err)
	}
	var document userDocument
	decoder := toml.NewDecoder(bytes.NewReader(data)).DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return userDocument{}, fmt.Errorf("parse bootstrap user file: %w", err)
	}
	if err := validateUserDocument(document); err != nil {
		return userDocument{}, fmt.Errorf("validate bootstrap user file: %w", err)
	}
	return document, nil
}

func (s *UserStore) write(document userDocument) error {
	if err := validateUserDocument(document); err != nil {
		return err
	}
	data, err := toml.Marshal(document)
	if err != nil {
		return fmt.Errorf("encode bootstrap user file: %w", err)
	}
	return writeAtomicFile(s.path, data, 0o600)
}

func validateUserDocument(document userDocument) error {
	if document.Schema != authSchema {
		return fmt.Errorf("schema must equal %d", authSchema)
	}
	seen := make(map[string]bool, len(document.Users))
	for _, user := range document.Users {
		if err := validateUsername(user.Username); err != nil {
			return err
		}
		if seen[user.Username] {
			return fmt.Errorf("duplicate bootstrap administrator %q", user.Username)
		}
		seen[user.Username] = true
		if _, _, _, err := parsePHC(user.PasswordHash); err != nil {
			return fmt.Errorf("user %q password hash: %w", user.Username, err)
		}
		if user.AuthVersion == 0 {
			return fmt.Errorf("user %q authentication version must be positive", user.Username)
		}
		if user.CreatedAt.IsZero() || user.UpdatedAt.IsZero() || user.UpdatedAt.Before(user.CreatedAt) {
			return fmt.Errorf("user %q timestamps are invalid", user.Username)
		}
	}
	return nil
}

func validateUsername(username string) error {
	if username == "" || len(username) > 128 || !utf8.ValidString(username) || strings.TrimSpace(username) != username {
		return fmt.Errorf("%w: must be non-empty valid UTF-8, at most 128 bytes, and have no surrounding whitespace", ErrInvalidUsername)
	}
	for _, character := range username {
		if unicode.IsControl(character) {
			return fmt.Errorf("%w: must not contain control characters", ErrInvalidUsername)
		}
	}
	return nil
}
