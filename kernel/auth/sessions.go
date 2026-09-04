package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"time"

	"the8020/kernel/database"
)

var (
	ErrUnauthenticated  = errors.New("authentication is required")
	ErrSessionExpired   = errors.New("authentication session expired")
	ErrInvalidSessionID = errors.New("invalid authentication session ID")
)

const sessionsTable = `"the8020__users__sessions"`

type SessionRecord struct {
	SessionID   string    `json:"session_id"`
	Username    string    `json:"username"`
	SecretHash  string    `json:"-"`
	AuthVersion uint64    `json:"auth_version"`
	CreatedAt   time.Time `json:"created_at"`
	ExpiresAt   time.Time `json:"expires_at"`
}

type SessionStoreConfig struct {
	Database database.Store
	Random   io.Reader
	Now      func() time.Time
}

type SessionStore struct {
	database database.Store
	random   io.Reader
	now      func() time.Time
}

func NewSessionStore(config SessionStoreConfig) (*SessionStore, error) {
	if config.Database == nil {
		return nil, errors.New("database is required")
	}
	if config.Random == nil {
		config.Random = rand.Reader
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &SessionStore{database: config.Database, random: config.Random, now: config.Now}, nil
}

// Check confirms that the package-owned authentication-session table is
// queryable without scanning its contents. An empty table is ready.
func (s *SessionStore) Check() error {
	return checkTable(s.database, sessionsTable)
}

func (s *SessionStore) Create(username string, authVersion uint64, duration time.Duration) (SessionRecord, string, error) {
	if err := ValidateUsername(username); err != nil {
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
		record := SessionRecord{SessionID: sessionID, Username: username, SecretHash: hashSessionSecret(secret), AuthVersion: authVersion, CreatedAt: now, ExpiresAt: now.Add(duration)}
		result, err := s.database.ExecContext(context.Background(), `INSERT INTO `+sessionsTable+` ("sessionId", "username", "secretHash", "authVersion", "createdAt", "expiresAt") VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT ("sessionId") DO NOTHING`, record.SessionID, record.Username, record.SecretHash, int64(record.AuthVersion), database.EncodeTime(s.database, record.CreatedAt), database.EncodeTime(s.database, record.ExpiresAt))
		if err != nil {
			return SessionRecord{}, "", err
		}
		if affected, err := result.RowsAffected(); err != nil {
			return SessionRecord{}, "", err
		} else if affected == 0 {
			continue
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
	if errors.Is(err, sql.ErrNoRows) {
		return SessionRecord{}, ErrUnauthenticated
	}
	if err != nil {
		return SessionRecord{}, err
	}
	if subtle.ConstantTimeCompare([]byte(hashSessionSecret(secret)), []byte(record.SecretHash)) != 1 {
		return SessionRecord{}, ErrUnauthenticated
	}
	if !s.now().UTC().Before(record.ExpiresAt) {
		_ = s.Delete(sessionID)
		return SessionRecord{}, ErrSessionExpired
	}
	return record, nil
}

func (s *SessionStore) Read(sessionID string) (SessionRecord, error) {
	if !validLowerHex(sessionID, 32) {
		return SessionRecord{}, ErrInvalidSessionID
	}
	return scanSession(s.database.QueryRowContext(context.Background(), `SELECT "sessionId", "username", "secretHash", "authVersion", "createdAt", "expiresAt" FROM `+sessionsTable+` WHERE "sessionId" = $1`, sessionID))
}

func scanSession(row rowScanner) (SessionRecord, error) {
	var record SessionRecord
	var authVersion int64
	var created, expires any
	if err := row.Scan(&record.SessionID, &record.Username, &record.SecretHash, &authVersion, &created, &expires); err != nil {
		return SessionRecord{}, err
	}
	if authVersion < 1 {
		return SessionRecord{}, errors.New("authentication session version must be positive")
	}
	record.AuthVersion = uint64(authVersion)
	var err error
	if record.CreatedAt, err = database.DecodeTime(created); err != nil {
		return SessionRecord{}, fmt.Errorf("authentication session created time: %w", err)
	}
	if record.ExpiresAt, err = database.DecodeTime(expires); err != nil {
		return SessionRecord{}, fmt.Errorf("authentication session expiry time: %w", err)
	}
	if err := validateSessionRecord(record); err != nil {
		return SessionRecord{}, err
	}
	return record, nil
}

func (s *SessionStore) Delete(sessionID string) error {
	if !validLowerHex(sessionID, 32) {
		return ErrInvalidSessionID
	}
	_, err := s.database.ExecContext(context.Background(), `DELETE FROM `+sessionsTable+` WHERE "sessionId" = $1`, sessionID)
	return err
}

func (s *SessionStore) CleanupExpired(now time.Time) (int, error) {
	result, err := s.database.ExecContext(context.Background(), `DELETE FROM `+sessionsTable+` WHERE "expiresAt" <= $1`, database.EncodeTime(s.database, now))
	if err != nil {
		return 0, err
	}
	affected, err := result.RowsAffected()
	return int(affected), err
}

func validateSessionRecord(record SessionRecord) error {
	if !validLowerHex(record.SessionID, 32) || record.AuthVersion == 0 {
		return errors.New("invalid authentication session identity")
	}
	if err := ValidateUsername(record.Username); err != nil {
		return err
	}
	if !isSessionHash(record.SecretHash) {
		return errors.New("invalid authentication session secret hash")
	}
	if record.CreatedAt.IsZero() || record.ExpiresAt.IsZero() || !record.ExpiresAt.After(record.CreatedAt) {
		return errors.New("invalid authentication session timestamps")
	}
	return nil
}

func isSessionHash(value string) bool {
	return len(value) == len("sha256:")+64 && value[:len("sha256:")] == "sha256:" && validLowerHex(value[len("sha256:"):], 64)
}

func parseSessionToken(token string) (string, string, error) {
	if len(token) != 3+32+1+64 || token[:3] != "v1." || token[35] != '.' {
		return "", "", ErrUnauthenticated
	}
	sessionID, secret := token[3:35], token[36:]
	if !validLowerHex(sessionID, 32) || !validLowerHex(secret, 64) {
		return "", "", ErrUnauthenticated
	}
	return sessionID, secret, nil
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
