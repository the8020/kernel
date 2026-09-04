package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// DecodeTime normalizes datetime values returned by SQLite TEXT columns and
// PostgreSQL timestamp columns.
func DecodeTime(value any) (time.Time, error) {
	if typed, ok := value.(time.Time); ok {
		return typed.UTC(), nil
	}
	var text string
	switch typed := value.(type) {
	case string:
		text = typed
	case []byte:
		text = string(typed)
	default:
		return time.Time{}, fmt.Errorf("unsupported timestamp value %T", value)
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05", "2006-01-02 15:04:05.999999999Z07:00", "2006-01-02 15:04:05.999999999 -0700 MST"} {
		if parsed, err := time.Parse(layout, text); err == nil {
			return parsed.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid database timestamp %q", text)
}

// Row is the result shape needed by kernel-owned repositories.
type Row interface {
	Scan(...any) error
}

// Store is the kernel-internal SQL boundary used by durable domain
// repositories. It bypasses application result limits and the application
// readiness gate without exposing the pool or credentials.
type Store interface {
	Backend() string
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) Row
	BeginTx(context.Context, *sql.TxOptions) (*sql.Tx, error)
}

// Backend reports the configured non-secret SQL dialect.
func (m *Manager) Backend() string {
	if m == nil {
		return ""
	}
	return m.Status().Backend
}

// EncodeTime supplies the canonical SQLite representation while allowing the
// PostgreSQL driver to retain its native timestamp value.
func EncodeTime(store Store, value time.Time) any {
	value = value.UTC().Truncate(time.Millisecond)
	if store != nil && store.Backend() == BackendSQLite {
		return value.Format("2006-01-02T15:04:05.000Z")
	}
	return value
}

// EncodeJSON supplies the engine-native parameter representation shared by
// runtime SQL and kernel-owned repositories.
func EncodeJSON(backend string, value any) (any, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	switch backend {
	case BackendSQLite:
		return string(encoded), nil
	case BackendPostgreSQL:
		return json.RawMessage(encoded), nil
	default:
		return nil, fmt.Errorf("unsupported database backend %q", backend)
	}
}

type errorRow struct{ err error }

func (r errorRow) Scan(...any) error { return r.err }

func (m *Manager) internalDatabase() (*sql.DB, error) {
	if m == nil {
		return nil, errors.New("database is unavailable")
	}
	if m.openErr != nil {
		return nil, m.openErr
	}
	if m.db == nil {
		return nil, errors.New("database is unavailable")
	}
	return m.db, nil
}

// ExecContext executes kernel-owned repository SQL. Application SQL must use
// Execute or RunStatement instead.
func (m *Manager) ExecContext(ctx context.Context, statement string, parameters ...any) (sql.Result, error) {
	db, err := m.internalDatabase()
	if err != nil {
		return nil, err
	}
	return db.ExecContext(ctx, statement, parameters...)
}

// QueryContext executes a kernel-owned repository query.
func (m *Manager) QueryContext(ctx context.Context, statement string, parameters ...any) (*sql.Rows, error) {
	db, err := m.internalDatabase()
	if err != nil {
		return nil, err
	}
	return db.QueryContext(ctx, statement, parameters...)
}

// QueryRowContext executes a kernel-owned repository scalar query.
func (m *Manager) QueryRowContext(ctx context.Context, statement string, parameters ...any) Row {
	db, err := m.internalDatabase()
	if err != nil {
		return errorRow{err: err}
	}
	return db.QueryRowContext(ctx, statement, parameters...)
}

// BeginTx starts a kernel-owned repository transaction.
func (m *Manager) BeginTx(ctx context.Context, options *sql.TxOptions) (*sql.Tx, error) {
	db, err := m.internalDatabase()
	if err != nil {
		return nil, err
	}
	return db.BeginTx(ctx, options)
}
