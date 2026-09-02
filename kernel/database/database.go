// Package database owns the kernel's single system database connection.
package database

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"modernc.org/sqlite"
)

const (
	BackendSQLite     = "sqlite"
	BackendPostgreSQL = "postgresql"

	InstanceRootPlaceholder = "${INSTANCE_ROOT}"
	maxStatementBytes       = 1 << 20
	maxResultBytes          = 1 << 20
	maxResultRows           = 1_000

	StateReady       = "READY"
	StateUnavailable = "UNAVAILABLE"
)

// Config selects the one system database used by this kernel.
type Config struct {
	Backend      string
	Location     string
	Username     string
	Password     string
	InstanceRoot string
}

// Status is a credential-free connectivity snapshot.
type Status struct {
	Backend  string `json:"backend"`
	Location string `json:"location"`
	State    string `json:"state"`
	Error    string `json:"error,omitempty"`
}

// QueryResult preserves SQL column order and duplicate column names.
type QueryResult struct {
	Columns   []string `json:"columns"`
	Rows      [][]any  `json:"rows"`
	Truncated bool     `json:"truncated"`
}

// ExecuteResult describes a statement that does not return rows.
type ExecuteResult struct {
	RowsAffected int64 `json:"rows_affected"`
}

// Manager owns a database/sql pool. A failed initial connection remains
// retryable through Check, Query, and Execute.
type Manager struct {
	file     string
	db       *sql.DB
	openErr  error
	statusMu sync.RWMutex
	status   Status
}

// New prepares the configured database without requiring it to be reachable.
func New(config Config) *Manager {
	manager := &Manager{
		status: Status{
			Backend:  config.Backend,
			Location: displayLocation(config.Backend, config.Location),
			State:    StateUnavailable,
		},
	}
	location, err := resolveLocation(config.Location, config.InstanceRoot)
	if err != nil {
		manager.openErr = err
		manager.status.Error = err.Error()
		return manager
	}
	manager.status.Location = displayLocation(config.Backend, location)
	switch config.Backend {
	case BackendSQLite:
		manager.file = location
		if !filepath.IsAbs(location) {
			manager.openErr = errors.New("SQLite database path must be absolute")
			return manager
		}
		if err := os.MkdirAll(filepath.Dir(location), 0o700); err != nil {
			manager.openErr = fmt.Errorf("create SQLite database directory: %w", err)
			return manager
		}
		connector, err := sqlite.NewConnector(sqliteDSN(location))
		if err != nil {
			manager.openErr = fmt.Errorf("configure SQLite database: %w", err)
			return manager
		}
		manager.db = sql.OpenDB(connector)
		manager.db.SetMaxOpenConns(1)
		manager.db.SetMaxIdleConns(1)
	case BackendPostgreSQL:
		parsed, err := url.Parse(location)
		if err != nil || (parsed.Scheme != "postgresql" && parsed.Scheme != "postgres") || parsed.Host == "" {
			manager.openErr = errors.New("PostgreSQL location must be a postgres:// or postgresql:// URL with a host")
			return manager
		}
		if parsed.User != nil || hasQueryCredential(parsed.Query()) {
			manager.openErr = errors.New("PostgreSQL credentials must use database.username and database.password")
			return manager
		}
		connection, err := pgx.ParseConfig(location)
		if err != nil {
			manager.openErr = fmt.Errorf("configure PostgreSQL database: %w", err)
			return manager
		}
		connection.User, connection.Password = config.Username, config.Password
		manager.db = stdlib.OpenDB(*connection)
		manager.db.SetMaxOpenConns(16)
		manager.db.SetMaxIdleConns(4)
	default:
		manager.openErr = fmt.Errorf("unsupported database backend %q", config.Backend)
	}
	if manager.openErr != nil {
		manager.status.Error = manager.openErr.Error()
	}
	return manager
}

func resolveLocation(location, instanceRoot string) (string, error) {
	if strings.TrimSpace(location) == "" {
		return "", errors.New("database location is required")
	}
	resolved := strings.ReplaceAll(location, InstanceRootPlaceholder, instanceRoot)
	if strings.Contains(resolved, "${") {
		return "", errors.New("database location contains an unknown placeholder")
	}
	return resolved, nil
}

func sqliteDSN(path string) string {
	location := &url.URL{Scheme: "file", Path: filepath.Clean(path)}
	query := location.Query()
	query.Add("_pragma", "foreign_keys(1)")
	query.Add("_pragma", "busy_timeout(5000)")
	location.RawQuery = query.Encode()
	return location.String()
}

func displayLocation(backend, location string) string {
	if backend != BackendPostgreSQL {
		return location
	}
	parsed, err := url.Parse(location)
	if err != nil {
		return "<invalid PostgreSQL URL>"
	}
	parsed.User = nil
	query, err := url.ParseQuery(parsed.RawQuery)
	if err != nil {
		parsed.RawQuery = ""
	} else {
		for key := range query {
			if strings.EqualFold(key, "user") || strings.EqualFold(key, "password") {
				query.Del(key)
			}
		}
		parsed.RawQuery = query.Encode()
	}
	return parsed.String()
}

func hasQueryCredential(query url.Values) bool {
	for key := range query {
		if strings.EqualFold(key, "user") || strings.EqualFold(key, "password") {
			return true
		}
	}
	return false
}

// Status returns the last connectivity check without performing I/O.
func (m *Manager) Status() Status {
	if m == nil {
		return Status{State: StateUnavailable, Error: "database is unavailable"}
	}
	m.statusMu.RLock()
	defer m.statusMu.RUnlock()
	return m.status
}

// Check verifies connectivity and updates the cached status.
func (m *Manager) Check(ctx context.Context) (Status, error) {
	err := m.ping(ctx)
	if m == nil {
		return Status{State: StateUnavailable, Error: err.Error()}, err
	}
	m.statusMu.Lock()
	m.status.State = StateUnavailable
	m.status.Error = ""
	if err == nil {
		m.status.State = StateReady
	} else {
		m.status.Error = err.Error()
	}
	status := m.status
	m.statusMu.Unlock()
	return status, err
}

func (m *Manager) ping(ctx context.Context) error {
	if m == nil {
		return errors.New("database is unavailable")
	}
	if m.openErr != nil {
		return m.openErr
	}
	if m.db == nil {
		return errors.New("database is unavailable")
	}
	if err := m.db.PingContext(ctx); err != nil {
		return err
	}
	if m.file != "" {
		if err := os.Chmod(m.file, 0o600); err != nil {
			return fmt.Errorf("restrict SQLite database file: %w", err)
		}
	}
	return nil
}

// Query executes one row-returning SQL statement.
func (m *Manager) Query(ctx context.Context, statement string, parameters []any) (QueryResult, error) {
	if err := m.ready(statement, parameters); err != nil {
		return QueryResult{}, err
	}
	rows, err := m.db.QueryContext(ctx, statement, parameters...)
	if err != nil {
		return QueryResult{}, err
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return QueryResult{}, err
	}
	result := QueryResult{Columns: columns, Rows: make([][]any, 0)}
	used := 0
	for rows.Next() {
		if len(result.Rows) == maxResultRows {
			result.Truncated = true
			break
		}
		values := make([]any, len(columns))
		targets := make([]any, len(columns))
		for index := range values {
			targets[index] = &values[index]
		}
		if err := rows.Scan(targets...); err != nil {
			return QueryResult{}, err
		}
		for index := range values {
			values[index] = normalizeValue(values[index])
		}
		encoded, err := json.Marshal(values)
		if err != nil {
			return QueryResult{}, fmt.Errorf("encode database row: %w", err)
		}
		if used+len(encoded) > maxResultBytes {
			result.Truncated = true
			break
		}
		used += len(encoded)
		result.Rows = append(result.Rows, values)
	}
	if err := rows.Err(); err != nil {
		return QueryResult{}, err
	}
	return result, nil
}

// Execute runs one SQL statement that does not return rows.
func (m *Manager) Execute(ctx context.Context, statement string, parameters []any) (ExecuteResult, error) {
	if err := m.ready(statement, parameters); err != nil {
		return ExecuteResult{}, err
	}
	result, err := m.db.ExecContext(ctx, statement, parameters...)
	if err != nil {
		return ExecuteResult{}, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return ExecuteResult{}, err
	}
	return ExecuteResult{RowsAffected: affected}, nil
}

func (m *Manager) ready(statement string, parameters []any) error {
	if m == nil || m.db == nil {
		if m != nil && m.openErr != nil {
			return m.openErr
		}
		return errors.New("database is unavailable")
	}
	if len(strings.TrimSpace(statement)) == 0 {
		return errors.New("SQL statement is required")
	}
	if len(statement) > maxStatementBytes {
		return fmt.Errorf("SQL statement exceeds %d bytes", maxStatementBytes)
	}
	for _, parameter := range parameters {
		switch parameter.(type) {
		case nil, bool, int64, float64, string, []byte, time.Time:
		default:
			return fmt.Errorf("unsupported SQL parameter type %T", parameter)
		}
	}
	return nil
}

func normalizeValue(value any) any {
	switch typed := value.(type) {
	case []byte:
		if utf8.Valid(typed) {
			return string(typed)
		}
		return "base64:" + base64.StdEncoding.EncodeToString(typed)
	case time.Time:
		return typed.UTC().Format(time.RFC3339Nano)
	default:
		return typed
	}
}

// DecodeParameters parses a JSON array of scalar SQL parameters while
// retaining integer values as int64.
func DecodeParameters(data []byte) ([]any, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return nil, fmt.Errorf("parameters must be a JSON array: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("parameters contain trailing JSON data")
	}
	values, ok := decoded.([]any)
	if !ok {
		return nil, errors.New("parameters must be a JSON array")
	}
	for index, value := range values {
		normalized, err := normalizeParameter(value)
		if err != nil {
			return nil, fmt.Errorf("parameter %d: %w", index+1, err)
		}
		values[index] = normalized
	}
	return values, nil
}

func normalizeParameter(value any) (any, error) {
	switch typed := value.(type) {
	case nil, bool, string:
		return typed, nil
	case json.Number:
		if integer, err := strconv.ParseInt(string(typed), 10, 64); err == nil {
			return integer, nil
		}
		decimal, err := strconv.ParseFloat(string(typed), 64)
		if err != nil || math.IsInf(decimal, 0) || math.IsNaN(decimal) {
			return nil, errors.New("number is out of range")
		}
		return decimal, nil
	default:
		return nil, errors.New("must be null, a boolean, a number, or a string")
	}
}

// Close releases the connection pool.
func (m *Manager) Close() error {
	if m == nil || m.db == nil {
		return nil
	}
	return m.db.Close()
}
