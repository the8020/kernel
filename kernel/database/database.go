// Package database owns the kernel's system database connection pool.
package database

import (
	"bytes"
	"context"
	"database/sql"
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

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"modernc.org/sqlite"

	"the8020/kernel/settings"
)

const (
	BackendSQLite     = "sqlite"
	BackendPostgreSQL = "postgresql"

	InstanceRootPlaceholder   = "${INSTANCE_ROOT}"
	maxStatementBytes         = 1 << 20
	DefaultMaximumResultBytes = 10 << 20
	DefaultMaximumResultRows  = 10_000

	StateConnected            = "CONNECTED"
	StateInitializing         = "INITIALIZING"
	StateReady                = "READY"
	StateInitializationFailed = "INITIALIZATION_FAILED"
	StateUnavailable          = "UNAVAILABLE"
)

// Config selects the one system database used by this kernel.
type Config struct {
	Backend                string
	Location               string
	Username               string
	Password               string
	InstanceRoot           string
	MaximumOpenConnections int
	MaximumIdleConnections int
	MaximumResultRows      int
	MaximumResultBytes     int
}

// Status is a credential-free connectivity snapshot.
type Status struct {
	Backend                  string `json:"backend"`
	Location                 string `json:"location"`
	State                    string `json:"state"`
	Error                    string `json:"error,omitempty"`
	MaximumOpenConnections   int    `json:"maximum_open_connections"`
	MaximumIdleConnections   int    `json:"maximum_idle_connections"`
	MaximumResultRows        int    `json:"maximum_result_rows"`
	MaximumResultBytes       int    `json:"maximum_result_bytes"`
	OpenConnections          int    `json:"open_connections"`
	InUseConnections         int    `json:"in_use_connections"`
	IdleConnections          int    `json:"idle_connections"`
	WaitCount                int64  `json:"wait_count"`
	WaitDurationMilliseconds int64  `json:"wait_duration_milliseconds"`
	CatalogVersion           int    `json:"catalog_version"`
	Initialized              bool   `json:"initialized"`
	PendingDeployment        bool   `json:"pending_deployment"`
	PackageSetHash           string `json:"package_set_hash,omitempty"`
	DescriptorSetHash        string `json:"descriptor_set_hash,omitempty"`
	InitializedAt            string `json:"initialized_at,omitempty"`
	CatalogError             string `json:"catalog_error,omitempty"`
	LastDeploymentAt         string `json:"last_deployment_at,omitempty"`
	LastDeploymentError      string `json:"last_deployment_error,omitempty"`
}

type poolPolicy struct {
	maximumOpen int
	maximumIdle int
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
	file             string
	fileModeMu       sync.Mutex
	fileModeReady    bool
	db               *sql.DB
	openErr          error
	statusMu         sync.RWMutex
	status           Status
	schemaMu         sync.Mutex
	transactionsMu   sync.Mutex
	transactions     map[string]*transaction
	application      *applicationGate
	evaluatorMu      sync.RWMutex
	evaluator        DefinitionEvaluator
	fullSynchronizer FullSynchronizer
	sourceEvaluator  SourceEvaluator
}

// New prepares the configured database without requiring it to be reachable.
func New(config Config) *Manager {
	policy, policyErr := newPoolPolicy(config.MaximumOpenConnections, config.MaximumIdleConnections)
	if config.MaximumResultRows <= 0 {
		config.MaximumResultRows = DefaultMaximumResultRows
	}
	if config.MaximumResultBytes <= 0 {
		config.MaximumResultBytes = DefaultMaximumResultBytes
	}
	manager := &Manager{
		transactions: map[string]*transaction{},
		application:  newApplicationGate(applicationLimit(policy.maximumOpen)),
		status: Status{
			Backend:                config.Backend,
			Location:               displayLocation(config.Backend, config.Location),
			State:                  StateUnavailable,
			MaximumOpenConnections: policy.maximumOpen,
			MaximumIdleConnections: policy.maximumIdle,
			MaximumResultRows:      config.MaximumResultRows,
			MaximumResultBytes:     config.MaximumResultBytes,
		},
	}
	if policyErr != nil {
		manager.openErr = policyErr
		manager.status.Error = policyErr.Error()
		return manager
	}
	if config.MaximumResultRows < 1 || config.MaximumResultBytes < 1 {
		manager.openErr = errors.New("database result limits must be positive")
		manager.status.Error = manager.openErr.Error()
		return manager
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
	default:
		manager.openErr = fmt.Errorf("unsupported database backend %q", config.Backend)
	}
	manager.applyPool(policy)
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
	query.Set("_busy_timeout", "5000")
	query.Set("_foreign_keys", "on")
	query.Set("_journal_mode", "wal")
	location.RawQuery = query.Encode()
	return location.String()
}

func newPoolPolicy(maximumOpen, maximumIdle int) (poolPolicy, error) {
	if maximumOpen < 1 {
		return poolPolicy{}, errors.New("database maximum open connections must be positive")
	}
	if maximumIdle < 0 {
		return poolPolicy{}, errors.New("database maximum idle connections cannot be negative")
	}
	if maximumIdle > maximumOpen {
		return poolPolicy{}, errors.New("database maximum idle connections cannot exceed maximum open connections")
	}
	return poolPolicy{maximumOpen: maximumOpen, maximumIdle: maximumIdle}, nil
}

func poolPolicyFromValues(values settings.Values) (poolPolicy, error) {
	maximumOpen, openOK := values["database.maximum_open_connections"].(int64)
	maximumIdle, idleOK := values["database.maximum_idle_connections"].(int64)
	if !openOK || !idleOK {
		return poolPolicy{}, errors.New("database connection pool settings are unavailable")
	}
	maximumInt := int64(^uint(0) >> 1)
	if maximumOpen > maximumInt || maximumIdle > maximumInt {
		return poolPolicy{}, errors.New("database connection pool setting exceeds the supported integer range")
	}
	return newPoolPolicy(int(maximumOpen), int(maximumIdle))
}

func (m *Manager) applyPool(policy poolPolicy) {
	if m.db != nil {
		m.db.SetMaxOpenConns(policy.maximumOpen)
		m.db.SetMaxIdleConns(policy.maximumIdle)
	}
	if m.application != nil {
		m.application.setLimit(applicationLimit(policy.maximumOpen))
	}
	m.statusMu.Lock()
	m.status.MaximumOpenConnections = policy.maximumOpen
	m.status.MaximumIdleConnections = policy.maximumIdle
	m.statusMu.Unlock()
}

func applicationLimit(maximumOpen int) int {
	if maximumOpen >= 3 {
		return maximumOpen - 2
	}
	return max(1, maximumOpen)
}

type applicationGate struct {
	mu      sync.Mutex
	limit   int
	inUse   int
	changed chan struct{}
}

func newApplicationGate(limit int) *applicationGate {
	return &applicationGate{limit: limit, changed: make(chan struct{})}
}

func (g *applicationGate) acquire(ctx context.Context) (func(), error) {
	for {
		g.mu.Lock()
		if g.inUse < g.limit {
			g.inUse++
			g.mu.Unlock()
			var once sync.Once
			return func() { once.Do(g.release) }, nil
		}
		changed := g.changed
		g.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-changed:
		}
	}
}

func (g *applicationGate) release() {
	g.mu.Lock()
	if g.inUse > 0 {
		g.inUse--
	}
	g.signalLocked()
	g.mu.Unlock()
}

func (g *applicationGate) setLimit(limit int) {
	g.mu.Lock()
	g.limit = max(1, limit)
	g.signalLocked()
	g.mu.Unlock()
}

func (g *applicationGate) signalLocked() {
	close(g.changed)
	g.changed = make(chan struct{})
}

type preparedPool struct {
	manager            *Manager
	policy             poolPolicy
	maximumResultRows  int
	maximumResultBytes int
}

func (p preparedPool) Commit() {
	p.manager.applyPool(p.policy)
	p.manager.statusMu.Lock()
	p.manager.status.MaximumResultRows = p.maximumResultRows
	p.manager.status.MaximumResultBytes = p.maximumResultBytes
	p.manager.statusMu.Unlock()
}
func (preparedPool) Discard() {}

// Prepare validates a live connection-pool policy change.
func (m *Manager) Prepare(_ context.Context, values settings.Values) (settings.Prepared, error) {
	policy, err := poolPolicyFromValues(values)
	if err != nil {
		return nil, err
	}
	status := m.Status()
	maximumRows, err := resultLimit(values, "database.maximum_result_rows", status.MaximumResultRows)
	if err != nil {
		return nil, err
	}
	maximumBytes, err := resultLimit(values, "database.maximum_result_bytes", status.MaximumResultBytes)
	if err != nil {
		return nil, err
	}
	return preparedPool{
		manager: m, policy: policy,
		maximumResultRows: maximumRows, maximumResultBytes: maximumBytes,
	}, nil
}

func resultLimit(values settings.Values, key string, current int) (int, error) {
	value, exists := values[key]
	if !exists {
		return current, nil
	}
	integer, ok := value.(int64)
	if !ok || integer < 1 || uint64(integer) > uint64(^uint(0)>>1) {
		return 0, fmt.Errorf("%s must be a positive supported integer", key)
	}
	return int(integer), nil
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

// Status combines the last connectivity check with local pool counters without
// performing database I/O.
func (m *Manager) Status() Status {
	if m == nil {
		return Status{State: StateUnavailable, Error: "database is unavailable"}
	}
	m.statusMu.RLock()
	status := m.status
	m.statusMu.RUnlock()
	if m.db != nil {
		pool := m.db.Stats()
		status.MaximumOpenConnections = pool.MaxOpenConnections
		status.OpenConnections = pool.OpenConnections
		status.InUseConnections = pool.InUse
		status.IdleConnections = pool.Idle
		status.WaitCount = pool.WaitCount
		status.WaitDurationMilliseconds = pool.WaitDuration.Milliseconds()
	}
	return status
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
		if m.status.CatalogError != "" {
			m.status.State = StateInitializationFailed
		} else if m.status.CatalogVersion > 0 && m.status.Initialized {
			m.status.State = StateReady
		} else {
			m.status.State = StateConnected
		}
	} else {
		m.status.Error = err.Error()
	}
	m.statusMu.Unlock()
	return m.Status(), err
}

// MarkUnavailable records a shared-state operation failure discovered after a
// successful connectivity probe. A later successful Check may restore READY.
func (m *Manager) MarkUnavailable(err error) {
	if m == nil || err == nil {
		return
	}
	m.statusMu.Lock()
	m.status.State = StateUnavailable
	m.status.Error = err.Error()
	m.statusMu.Unlock()
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
		m.fileModeMu.Lock()
		defer m.fileModeMu.Unlock()
		if !m.fileModeReady {
			if err := os.Chmod(m.file, 0o600); err != nil {
				return fmt.Errorf("restrict SQLite database file: %w", err)
			}
			m.fileModeReady = true
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
	maximumRows, maximumBytes := m.resultLimits()
	used := 0
	for rows.Next() {
		if len(result.Rows) == maximumRows {
			return QueryResult{}, fmt.Errorf("database result exceeds %d rows; paginate the query", maximumRows)
		}
		values := make([]any, len(columns))
		targets := make([]any, len(columns))
		for index := range values {
			targets[index] = &values[index]
		}
		if err := rows.Scan(targets...); err != nil {
			return QueryResult{}, err
		}
		encoded, err := json.Marshal(values)
		if err != nil {
			return QueryResult{}, fmt.Errorf("encode database row: %w", err)
		}
		if used+len(encoded) > maximumBytes {
			return QueryResult{}, fmt.Errorf("database result exceeds %d bytes; paginate the query", maximumBytes)
		}
		used += len(encoded)
		result.Rows = append(result.Rows, values)
	}
	if err := rows.Err(); err != nil {
		return QueryResult{}, err
	}
	return result, nil
}

func (m *Manager) resultLimits() (int, int) {
	m.statusMu.RLock()
	defer m.statusMu.RUnlock()
	return m.status.MaximumResultRows, m.status.MaximumResultBytes
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
		switch value := parameter.(type) {
		case nil, bool, int64, float64, string, []byte, time.Time:
		case json.RawMessage:
			if !json.Valid(value) {
				return errors.New("invalid JSON SQL parameter")
			}
		default:
			return fmt.Errorf("unsupported SQL parameter type %T", parameter)
		}
	}
	return nil
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
	m.rollbackTransactions()
	return m.db.Close()
}
