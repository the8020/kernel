package auth

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
)

var (
	ErrUnauthenticated  = errors.New("authentication is required")
	ErrSessionExpired   = errors.New("authentication session expired")
	ErrInvalidSessionID = errors.New("invalid authentication session ID")
)

type SessionRecord struct {
	SessionID   string    `toml:"-" json:"session_id"`
	Schema      int       `toml:"schema" json:"schema"`
	Username    string    `toml:"username" json:"username"`
	SecretHash  string    `toml:"secret_hash" json:"-"`
	AuthVersion uint64    `toml:"auth_version" json:"auth_version"`
	CreatedAt   time.Time `toml:"created_at" json:"created_at"`
	ExpiresAt   time.Time `toml:"expires_at" json:"expires_at"`
}

type SessionStoreConfig struct {
	Root   string
	Random io.Reader
	Now    func() time.Time
}

type SessionStore struct {
	root   string
	random io.Reader
	now    func() time.Time
}

func NewSessionStore(config SessionStoreConfig) (*SessionStore, error) {
	if config.Root == "" {
		return nil, errors.New("bootstrap authentication-session root is required")
	}
	root, err := filepath.Abs(config.Root)
	if err != nil {
		return nil, fmt.Errorf("resolve authentication-session root: %w", err)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create authentication-session root: %w", err)
	}
	info, err := os.Lstat(root)
	if err != nil {
		return nil, fmt.Errorf("inspect authentication-session root: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("authentication-session root must be a real directory")
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return nil, fmt.Errorf("restrict authentication-session root: %w", err)
	}
	if config.Random == nil {
		config.Random = rand.Reader
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &SessionStore{root: root, random: config.Random, now: config.Now}, nil
}

func (s *SessionStore) Root() string { return s.root }

func (s *SessionStore) Create(username string, authVersion uint64, duration time.Duration) (SessionRecord, string, error) {
	if err := validateUsername(username); err != nil {
		return SessionRecord{}, "", err
	}
	if authVersion == 0 || duration <= 0 {
		return SessionRecord{}, "", errors.New("positive authentication version and session duration are required")
	}
	for attempt := 0; attempt < 16; attempt++ {
		sessionID, err := randomHex(s.random, 16)
		if err != nil {
			return SessionRecord{}, "", fmt.Errorf("generate authentication session ID: %w", err)
		}
		secret, err := randomHex(s.random, 32)
		if err != nil {
			return SessionRecord{}, "", fmt.Errorf("generate authentication session secret: %w", err)
		}
		now := s.now().UTC()
		record := SessionRecord{Schema: authSchema, SessionID: sessionID, Username: username, SecretHash: hashSessionSecret(secret), AuthVersion: authVersion, CreatedAt: now, ExpiresAt: now.Add(duration)}
		data, err := toml.Marshal(record)
		if err != nil {
			return SessionRecord{}, "", fmt.Errorf("encode authentication session: %w", err)
		}
		path, err := s.createPath(sessionID)
		if err != nil {
			return SessionRecord{}, "", err
		}
		if err := publishFile(path, data, 0o600); err != nil {
			if errors.Is(err, os.ErrExist) {
				continue
			}
			return SessionRecord{}, "", fmt.Errorf("publish authentication session: %w", err)
		}
		return record, "v1." + sessionID + "." + secret, nil
	}
	return SessionRecord{}, "", errors.New("authentication session ID collision limit exceeded")
}

func (s *SessionStore) ValidateToken(token string) (SessionRecord, error) {
	sessionID, secret, err := parseSessionToken(token)
	if err != nil {
		return SessionRecord{}, ErrUnauthenticated
	}
	record, err := s.Read(sessionID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return SessionRecord{}, ErrUnauthenticated
		}
		return SessionRecord{}, err
	}
	presented := hashSessionSecret(secret)
	if subtle.ConstantTimeCompare([]byte(presented), []byte(record.SecretHash)) != 1 {
		return SessionRecord{}, ErrUnauthenticated
	}
	if !s.now().UTC().Before(record.ExpiresAt) {
		_ = s.Delete(sessionID)
		return SessionRecord{}, ErrSessionExpired
	}
	return record, nil
}

func (s *SessionStore) Read(sessionID string) (SessionRecord, error) {
	path, err := s.path(sessionID)
	if err != nil {
		return SessionRecord{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return SessionRecord{}, err
	}
	var record SessionRecord
	if err := toml.NewDecoder(bytes.NewReader(data)).DisallowUnknownFields().Decode(&record); err != nil {
		return SessionRecord{}, fmt.Errorf("parse authentication session %s: %w", sessionID, err)
	}
	record.SessionID = sessionID
	if err := validateSessionRecord(record); err != nil {
		return SessionRecord{}, fmt.Errorf("validate authentication session %s: %w", sessionID, err)
	}
	return record, nil
}

func (s *SessionStore) List() ([]SessionRecord, error) {
	shards, err := os.ReadDir(s.root)
	if err != nil {
		return nil, err
	}
	var records []SessionRecord
	for _, shard := range shards {
		if !shard.IsDir() || !validLowerHex(shard.Name(), 2) {
			continue
		}
		entries, err := os.ReadDir(filepath.Join(s.root, shard.Name()))
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".toml") {
				continue
			}
			sessionID := strings.TrimSuffix(entry.Name(), ".toml")
			if !validLowerHex(sessionID, 32) || !strings.HasPrefix(sessionID, shard.Name()) {
				continue
			}
			record, err := s.Read(sessionID)
			if err != nil {
				return nil, err
			}
			records = append(records, record)
		}
	}
	sort.Slice(records, func(i, j int) bool { return records[i].SessionID < records[j].SessionID })
	return records, nil
}

func (s *SessionStore) Delete(sessionID string) error {
	path, err := s.path(sessionID)
	if err != nil {
		return err
	}
	if err := os.Remove(path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("delete authentication session: %w", err)
	}
	return syncDirectory(filepath.Dir(path))
}

func (s *SessionStore) RevokeUser(username string) (int, error) {
	records, err := s.List()
	if err != nil {
		return 0, err
	}
	removed := 0
	var joined error
	for _, record := range records {
		if record.Username != username {
			continue
		}
		if err := s.Delete(record.SessionID); err != nil {
			joined = errors.Join(joined, err)
		} else {
			removed++
		}
	}
	return removed, joined
}

func (s *SessionStore) CleanupExpired(now time.Time) (int, error) {
	records, err := s.List()
	if err != nil {
		return 0, err
	}
	removed := 0
	var joined error
	for _, record := range records {
		if now.UTC().Before(record.ExpiresAt) {
			continue
		}
		if err := s.Delete(record.SessionID); err != nil {
			joined = errors.Join(joined, err)
		} else {
			removed++
		}
	}
	return removed, joined
}

func (s *SessionStore) createPath(sessionID string) (string, error) {
	if !validLowerHex(sessionID, 32) {
		return "", ErrInvalidSessionID
	}
	shard := filepath.Join(s.root, sessionID[:2])
	info, err := os.Lstat(shard)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.Mkdir(shard, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return "", fmt.Errorf("create authentication-session shard: %w", err)
		}
		info, err = os.Lstat(shard)
	}
	if err != nil {
		return "", fmt.Errorf("inspect authentication-session shard: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("authentication-session shard must be a real directory")
	}
	if err := os.Chmod(shard, 0o700); err != nil {
		return "", err
	}
	return filepath.Join(shard, sessionID+".toml"), nil
}

func (s *SessionStore) path(sessionID string) (string, error) {
	if !validLowerHex(sessionID, 32) {
		return "", ErrInvalidSessionID
	}
	return filepath.Join(s.root, sessionID[:2], sessionID+".toml"), nil
}

func validateSessionRecord(record SessionRecord) error {
	if record.Schema != authSchema || !validLowerHex(record.SessionID, 32) || record.AuthVersion == 0 {
		return errors.New("invalid authentication session identity or schema")
	}
	if err := validateUsername(record.Username); err != nil {
		return err
	}
	if !strings.HasPrefix(record.SecretHash, "sha256:") || !validLowerHex(strings.TrimPrefix(record.SecretHash, "sha256:"), 64) {
		return errors.New("invalid authentication session secret hash")
	}
	if record.CreatedAt.IsZero() || record.ExpiresAt.IsZero() || !record.ExpiresAt.After(record.CreatedAt) {
		return errors.New("invalid authentication session timestamps")
	}
	return nil
}

func parseSessionToken(token string) (string, string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != "v1" || !validLowerHex(parts[1], 32) || !validLowerHex(parts[2], 64) {
		return "", "", ErrUnauthenticated
	}
	return parts[1], parts[2], nil
}

func hashSessionSecret(secret string) string {
	hash := sha256.Sum256([]byte(secret))
	return "sha256:" + hex.EncodeToString(hash[:])
}

func randomHex(random io.Reader, bytes int) (string, error) {
	value := make([]byte, bytes)
	if _, err := io.ReadFull(random, value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func validLowerHex(value string, length int) bool {
	if len(value) != length {
		return false
	}
	for _, character := range value {
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f')) {
			return false
		}
	}
	return true
}
