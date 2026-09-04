package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"the8020/kernel/database"
)

const (
	minimumUsernameLength = 3
	maximumUsernameLength = 32
	usersTable            = `"the8020__users__users"`
)

var (
	ErrInvalidUsername    = errors.New("invalid username")
	ErrInvalidCredentials = errors.New("invalid credentials")
	ErrUserDisabled       = errors.New("user is disabled")
)

type UserRecord struct {
	Username     string `json:"username"`
	PasswordHash string `json:"-"`
	Enabled      bool   `json:"enabled"`
	AuthVersion  uint64 `json:"auth_version"`
}

func (u UserRecord) ID() string { return "user:" + u.Username }

type UserStoreConfig struct {
	Database database.Store
	Hasher   *PasswordHasher
}

// UserStore persists authentication identities in the users package table.
type UserStore struct {
	database database.Store
	hasher   *PasswordHasher
}

func NewUserStore(config UserStoreConfig) (*UserStore, error) {
	if config.Database == nil || config.Hasher == nil {
		return nil, errors.New("database and password hasher are required")
	}
	return &UserStore{database: config.Database, hasher: config.Hasher}, nil
}

// Check confirms that the package-owned users table is queryable without
// scanning its contents. An empty table is ready.
func (s *UserStore) Check() error {
	return checkTable(s.database, usersTable)
}

func checkTable(store database.Store, table string) error {
	var marker int
	err := store.QueryRowContext(context.Background(), `SELECT 1 FROM `+table+` LIMIT 1`).Scan(&marker)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	return err
}

func (s *UserStore) Get(username string) (UserRecord, bool, error) {
	return s.GetContext(context.Background(), username)
}

func (s *UserStore) GetContext(ctx context.Context, username string) (UserRecord, bool, error) {
	row := s.database.QueryRowContext(ctx, `SELECT "username", "passwordHash", "enabled", "authVersion" FROM `+usersTable+` WHERE "username" = $1`, username)
	user, err := scanUser(row)
	if errors.Is(err, sql.ErrNoRows) {
		return UserRecord{}, false, nil
	}
	return user, err == nil, err
}

type rowScanner interface{ Scan(...any) error }

func scanUser(row rowScanner) (UserRecord, error) {
	var user UserRecord
	var authVersion int64
	if err := row.Scan(&user.Username, &user.PasswordHash, &user.Enabled, &authVersion); err != nil {
		return UserRecord{}, err
	}
	if authVersion < 1 {
		return UserRecord{}, errors.New("user authentication version must be positive")
	}
	user.AuthVersion = uint64(authVersion)
	return user, nil
}

func (s *UserStore) Authenticate(username, password string) (UserRecord, error) {
	return s.AuthenticateContext(context.Background(), username, password)
}

func (s *UserStore) AuthenticateBytes(username string, password []byte) (UserRecord, error) {
	return s.AuthenticateBytesContext(context.Background(), username, password)
}

func (s *UserStore) AuthenticateContext(ctx context.Context, username, password string) (UserRecord, error) {
	return s.AuthenticateBytesContext(ctx, username, []byte(password))
}

func (s *UserStore) AuthenticateBytesContext(ctx context.Context, username string, password []byte) (UserRecord, error) {
	user, exists, err := s.GetContext(ctx, username)
	if err != nil {
		return UserRecord{}, err
	}
	if !exists {
		s.hasher.VerifyUnknownBytes(password)
		return UserRecord{}, ErrInvalidCredentials
	}
	valid, err := s.hasher.VerifyBytes(user.PasswordHash, password)
	if err != nil {
		return UserRecord{}, fmt.Errorf("verify user password: %w", err)
	}
	if !valid {
		return UserRecord{}, ErrInvalidCredentials
	}
	if !user.Enabled {
		return UserRecord{}, ErrUserDisabled
	}
	return user, nil
}

// ValidateUsername enforces the account name shared by authentication,
// persistent user storage, and development sandbox IDs.
func ValidateUsername(username string) error {
	if len(username) < minimumUsernameLength || len(username) > maximumUsernameLength {
		return fmt.Errorf("%w: must be between %d and %d characters", ErrInvalidUsername, minimumUsernameLength, maximumUsernameLength)
	}
	for _, character := range username {
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') {
			return fmt.Errorf("%w: must contain only lowercase letters and digits", ErrInvalidUsername)
		}
	}
	return nil
}
