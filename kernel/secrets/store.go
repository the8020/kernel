// Package secrets owns shared named secrets. Values remain kernel-only and are
// returned only by an explicit read or trusted resolver.
package secrets

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"the8020/kernel/database"
)

const (
	maximumValueSize = 64 << 10
	secretsTable     = `"the8020__secrets__secrets"`
)

// Summary deliberately omits the stored value.
type Summary struct {
	Name      string    `json:"name"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Secret is returned only by the explicit get operation.
type Secret struct {
	Name      string    `json:"name"`
	Value     string    `json:"value"`
	UpdatedAt time.Time `json:"updated_at"`
}

type Config struct {
	Database database.Store
	Now      func() time.Time
}

type Store struct {
	database database.Store
	now      func() time.Time
}

func New(config Config) (*Store, error) {
	if config.Database == nil {
		return nil, errors.New("database is required")
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	store := &Store{database: config.Database, now: config.Now}
	if _, err := store.List(); err != nil {
		return nil, fmt.Errorf("open secrets table: %w", err)
	}
	return store, nil
}

func (s *Store) List() ([]Summary, error) {
	rows, err := s.database.QueryContext(context.Background(), `SELECT "name", "updatedAt" FROM `+secretsTable+` ORDER BY "name"`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Summary{}
	for rows.Next() {
		var item Summary
		var updated any
		if err := rows.Scan(&item.Name, &updated); err != nil {
			return nil, err
		}
		if item.UpdatedAt, err = database.DecodeTime(updated); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) Get(name string) (Secret, error) {
	name, err := normalizeName(name)
	if err != nil {
		return Secret{}, err
	}
	var secret Secret
	var updated any
	err = s.database.QueryRowContext(context.Background(), `SELECT "name", "value", "updatedAt" FROM `+secretsTable+` WHERE "name" = $1`, name).Scan(&secret.Name, &secret.Value, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return Secret{}, fmt.Errorf("secret %q: %w", name, os.ErrNotExist)
	}
	if err != nil {
		return Secret{}, err
	}
	secret.UpdatedAt, err = database.DecodeTime(updated)
	return secret, err
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
	now := s.now().UTC()
	_, err = s.database.ExecContext(ctx, `INSERT INTO `+secretsTable+` ("name", "value", "updatedAt") VALUES ($1, $2, $3) ON CONFLICT ("name") DO UPDATE SET "value" = excluded."value", "updatedAt" = excluded."updatedAt"`, name, value, database.EncodeTime(s.database, now))
	if err != nil {
		return Summary{}, err
	}
	return Summary{Name: name, UpdatedAt: now}, nil
}

// EnsureRandom returns one shared random secret, creating it exactly once.
// Concurrent kernels may generate candidates, but all read the winning row.
func (s *Store) EnsureRandom(ctx context.Context, name string, size int) (Secret, error) {
	name, err := normalizeName(name)
	if err != nil {
		return Secret{}, err
	}
	if size < 16 || size > maximumValueSize {
		return Secret{}, errors.New("random secret size must be between 16 and 65536 bytes")
	}
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		return Secret{}, err
	}
	now := s.now().UTC()
	encoded := base64.RawURLEncoding.EncodeToString(value)
	if _, err := s.database.ExecContext(ctx, `INSERT INTO `+secretsTable+` ("name", "value", "updatedAt") VALUES ($1, $2, $3) ON CONFLICT ("name") DO NOTHING`, name, encoded, database.EncodeTime(s.database, now)); err != nil {
		return Secret{}, err
	}
	return s.Get(name)
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
