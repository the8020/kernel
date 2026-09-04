package database

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"
)

const maximumSafeInteger = int64(1<<53 - 1)
const transactionMaximumDuration = 5 * time.Minute

// StatementRequest is the lossless runtime SQL protocol used by Kysely.
type StatementRequest struct {
	Statement      string          `json:"statement"`
	Parameters     json.RawMessage `json:"parameters,omitempty"`
	ReturnRows     bool            `json:"return_rows"`
	ReturnInsertID bool            `json:"return_insert_id"`
	Transaction    string          `json:"transaction,omitempty"`
}

// StatementResult has one shape for row-returning and mutating statements.
type StatementResult struct {
	Columns      []string `json:"columns"`
	Rows         [][]any  `json:"rows"`
	AffectedRows any      `json:"affected_rows,omitempty"`
	InsertID     any      `json:"insert_id,omitempty"`
}

type TransactionSettings struct {
	IsolationLevel string `json:"isolationLevel,omitempty"`
	ReadOnly       bool   `json:"readOnly,omitempty"`
}

type transaction struct {
	tx      *sql.Tx
	conn    *sql.Conn
	scope   string
	cancel  context.CancelFunc
	release func()
	once    sync.Once
}

type sqlRunner interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

// BeginTransaction starts a transaction owned by one runtime execution scope.
func (m *Manager) BeginTransaction(ctx context.Context, scope string, settings TransactionSettings) (string, error) {
	if m == nil || m.db == nil {
		return "", errors.New("database is unavailable")
	}
	if scope == "" {
		return "", errors.New("database transaction scope is required")
	}
	isolation, err := isolationLevel(settings.IsolationLevel)
	if err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	release, err := m.application.acquire(ctx)
	if err != nil {
		return "", err
	}
	conn, err := m.db.Conn(ctx)
	if err != nil {
		release()
		return "", err
	}
	lifetime, cancel := context.WithTimeout(context.Background(), transactionMaximumDuration)
	tx, err := conn.BeginTx(lifetime, &sql.TxOptions{Isolation: isolation, ReadOnly: settings.ReadOnly})
	if err != nil {
		cancel()
		_ = conn.Close()
		release()
		return "", err
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		_ = tx.Rollback()
		cancel()
		_ = conn.Close()
		release()
		return "", fmt.Errorf("create database transaction token: %w", err)
	}
	token := hex.EncodeToString(random[:])
	entry := &transaction{tx: tx, conn: conn, scope: scope, cancel: cancel, release: release}
	m.transactionsMu.Lock()
	m.transactions[token] = entry
	m.transactionsMu.Unlock()
	go func() {
		<-lifetime.Done()
		m.transactionsMu.Lock()
		if m.transactions[token] == entry {
			delete(m.transactions, token)
		}
		m.transactionsMu.Unlock()
		_ = entry.finish(false)
	}()
	return token, nil
}

func (t *transaction) finish(commit bool) error {
	var result error
	t.once.Do(func() {
		if commit {
			result = t.tx.Commit()
		} else {
			result = t.tx.Rollback()
			if errors.Is(result, sql.ErrTxDone) {
				result = nil
			}
		}
		t.cancel()
		result = errors.Join(result, t.conn.Close())
		t.release()
	})
	return result
}

func isolationLevel(value string) (sql.IsolationLevel, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "default":
		return sql.LevelDefault, nil
	case "read uncommitted":
		return sql.LevelReadUncommitted, nil
	case "read committed":
		return sql.LevelReadCommitted, nil
	case "repeatable read":
		return sql.LevelRepeatableRead, nil
	case "serializable":
		return sql.LevelSerializable, nil
	default:
		return sql.LevelDefault, fmt.Errorf("unsupported transaction isolation level %q", value)
	}
}

func (m *Manager) transaction(token, scope string) (*sql.Tx, error) {
	m.transactionsMu.Lock()
	defer m.transactionsMu.Unlock()
	entry := m.transactions[token]
	if entry == nil || entry.scope != scope {
		return nil, errors.New("database transaction is unavailable in this execution")
	}
	return entry.tx, nil
}

// FinishTransaction commits or rolls back a scoped runtime transaction.
func (m *Manager) FinishTransaction(ctx context.Context, scope, token string, commit bool) error {
	if token == "" || scope == "" {
		return errors.New("database transaction and scope are required")
	}
	m.transactionsMu.Lock()
	entry := m.transactions[token]
	if entry == nil || entry.scope != scope {
		m.transactionsMu.Unlock()
		return errors.New("database transaction is unavailable in this execution")
	}
	delete(m.transactions, token)
	m.transactionsMu.Unlock()
	if err := ctx.Err(); err != nil {
		_ = entry.finish(false)
		return err
	}
	return entry.finish(commit)
}

// CloseScope rolls back transactions left behind by a completed execution.
func (m *Manager) CloseScope(scope string) {
	m.transactionsMu.Lock()
	entries := make([]*transaction, 0)
	for token, entry := range m.transactions {
		if entry.scope == scope {
			delete(m.transactions, token)
			entries = append(entries, entry)
		}
	}
	m.transactionsMu.Unlock()
	for _, entry := range entries {
		_ = entry.finish(false)
	}
}

// CloseScopePrefix rolls back every transaction owned by a Worker execution.
// The separator boundary prevents one opaque scope from matching another.
func (m *Manager) CloseScopePrefix(prefix string) {
	if prefix == "" {
		return
	}
	m.transactionsMu.Lock()
	entries := make([]*transaction, 0)
	for token, entry := range m.transactions {
		if entry.scope == prefix || strings.HasPrefix(entry.scope, prefix+"\x00") {
			delete(m.transactions, token)
			entries = append(entries, entry)
		}
	}
	m.transactionsMu.Unlock()
	for _, entry := range entries {
		_ = entry.finish(false)
	}
}

func (m *Manager) rollbackTransactions() {
	m.transactionsMu.Lock()
	entries := make([]*transaction, 0, len(m.transactions))
	for token, entry := range m.transactions {
		delete(m.transactions, token)
		entries = append(entries, entry)
	}
	m.transactionsMu.Unlock()
	for _, entry := range entries {
		_ = entry.finish(false)
	}
}

// RunStatement executes one Kysely statement with explicit result intent.
func (m *Manager) RunStatement(ctx context.Context, scope string, request StatementRequest) (StatementResult, error) {
	parameters, err := m.decodeRuntimeParameters(request.Parameters)
	if err != nil {
		return StatementResult{}, err
	}
	if err := m.ready(request.Statement, parameters); err != nil {
		return StatementResult{}, err
	}
	var runner sqlRunner = m.db
	if request.Transaction != "" {
		runner, err = m.transaction(request.Transaction, scope)
		if err != nil {
			return StatementResult{}, err
		}
	} else {
		release, acquireErr := m.application.acquire(ctx)
		if acquireErr != nil {
			return StatementResult{}, acquireErr
		}
		defer release()
	}
	if request.ReturnRows {
		return m.runQuery(ctx, runner, request.Statement, parameters)
	}
	result, err := runner.ExecContext(ctx, request.Statement, parameters...)
	if err != nil {
		return StatementResult{}, err
	}
	response := StatementResult{Columns: []string{}, Rows: [][]any{}}
	if affected, affectedErr := result.RowsAffected(); affectedErr == nil {
		response.AffectedRows = taggedInteger(affected)
	}
	if request.ReturnInsertID {
		if insertID, insertErr := result.LastInsertId(); insertErr == nil {
			response.InsertID = taggedInteger(insertID)
		}
	}
	return response, nil
}

func (m *Manager) runQuery(ctx context.Context, runner sqlRunner, statement string, parameters []any) (StatementResult, error) {
	rows, err := runner.QueryContext(ctx, statement, parameters...)
	if err != nil {
		return StatementResult{}, err
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return StatementResult{}, err
	}
	types, err := rows.ColumnTypes()
	if err != nil {
		return StatementResult{}, err
	}
	result := StatementResult{Columns: columns, Rows: make([][]any, 0)}
	maximumRows, maximumBytes := m.resultLimits()
	used := 0
	for rows.Next() {
		if len(result.Rows) == maximumRows {
			return StatementResult{}, fmt.Errorf("database result exceeds %d rows; paginate the query", maximumRows)
		}
		values := make([]any, len(columns))
		targets := make([]any, len(columns))
		for index := range values {
			targets[index] = &values[index]
		}
		if err := rows.Scan(targets...); err != nil {
			return StatementResult{}, err
		}
		for index := range values {
			values[index], err = runtimeValue(values[index], types[index].DatabaseTypeName())
			if err != nil {
				return StatementResult{}, fmt.Errorf("encode database column %s: %w", columns[index], err)
			}
		}
		encoded, err := json.Marshal(values)
		if err != nil {
			return StatementResult{}, fmt.Errorf("encode database row: %w", err)
		}
		if used+len(encoded) > maximumBytes {
			return StatementResult{}, fmt.Errorf("database result exceeds %d bytes; paginate the query", maximumBytes)
		}
		used += len(encoded)
		result.Rows = append(result.Rows, values)
	}
	if err := rows.Err(); err != nil {
		return StatementResult{}, err
	}
	return result, nil
}

func runtimeValue(value any, databaseType string) (any, error) {
	switch typed := value.(type) {
	case nil, bool, float64, string:
		if strings.EqualFold(databaseType, "JSON") || strings.EqualFold(databaseType, "JSONB") {
			var decoded any
			if err := json.Unmarshal([]byte(fmt.Sprint(typed)), &decoded); err != nil {
				return nil, err
			}
			return map[string]any{"type": "json", "value": decoded}, nil
		}
		return typed, nil
	case int64:
		if typed >= -maximumSafeInteger && typed <= maximumSafeInteger {
			return typed, nil
		}
		return taggedInteger(typed), nil
	case []byte:
		if strings.EqualFold(databaseType, "JSON") || strings.EqualFold(databaseType, "JSONB") {
			var decoded any
			if err := json.Unmarshal(typed, &decoded); err != nil {
				return nil, err
			}
			return map[string]any{"type": "json", "value": decoded}, nil
		}
		return map[string]any{"type": "bytes", "value": base64.StdEncoding.EncodeToString(typed)}, nil
	case time.Time:
		return map[string]any{"type": "datetime", "value": formatDateTime(typed)}, nil
	default:
		return nil, fmt.Errorf("unsupported result type %T", value)
	}
}

func taggedInteger(value int64) map[string]any {
	return map[string]any{"type": "bigint", "value": strconv.FormatInt(value, 10)}
}

func (m *Manager) decodeRuntimeParameters(data json.RawMessage) ([]any, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var values []any
	if err := decoder.Decode(&values); err != nil {
		return nil, fmt.Errorf("parameters must be a JSON array: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("parameters contain trailing JSON data")
	}
	for index, value := range values {
		normalized, err := m.decodeRuntimeParameter(value)
		if err != nil {
			return nil, fmt.Errorf("parameter %d: %w", index+1, err)
		}
		values[index] = normalized
	}
	return values, nil
}

func (m *Manager) decodeRuntimeParameter(value any) (any, error) {
	if object, ok := value.(map[string]any); ok {
		typeName, _ := object["type"].(string)
		switch typeName {
		case "bigint":
			return strconv.ParseInt(stringValue(object["value"]), 10, 64)
		case "decimal":
			precision, err := integerJSON(object["precision"])
			if err != nil || precision < 1 || precision > 18 {
				return nil, errors.New("invalid decimal precision")
			}
			scale, err := integerJSON(object["scale"])
			if err != nil || scale < 0 || scale > precision {
				return nil, errors.New("invalid decimal scale")
			}
			return scaledDecimal(stringValue(object["value"]), precision, scale)
		case "datetime":
			parsed, err := time.Parse(time.RFC3339Nano, stringValue(object["value"]))
			if err != nil || parsed.Nanosecond()%int(time.Millisecond) != 0 {
				return nil, errors.New("invalid datetime parameter")
			}
			if m.status.Backend == BackendSQLite {
				return formatDateTime(parsed), nil
			}
			return parsed.UTC().Truncate(time.Millisecond), nil
		case "bytes":
			decoded, err := base64.StdEncoding.DecodeString(stringValue(object["value"]))
			if err != nil {
				return nil, errors.New("invalid bytes parameter")
			}
			return decoded, nil
		case "json":
			encoded, err := EncodeJSON(m.status.Backend, object["value"])
			if err != nil {
				return nil, errors.New("invalid JSON parameter")
			}
			return encoded, nil
		default:
			return nil, errors.New("unknown tagged database value")
		}
	}
	return normalizeParameter(value)
}

func formatDateTime(value time.Time) string {
	return value.UTC().Truncate(time.Millisecond).Format("2006-01-02T15:04:05.000Z")
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

func integerJSON(value any) (int, error) {
	number, ok := value.(json.Number)
	if !ok {
		return 0, errors.New("value is not an integer")
	}
	parsed, err := strconv.Atoi(string(number))
	return parsed, err
}

func scaledDecimal(value string, precision, scale int) (int64, error) {
	if value == "" || strings.HasPrefix(value, "+") {
		return 0, errors.New("decimal is not canonical")
	}
	negative := strings.HasPrefix(value, "-")
	unsigned := strings.TrimPrefix(value, "-")
	parts := strings.Split(unsigned, ".")
	if len(parts) > 2 || parts[0] == "" || (len(parts[0]) > 1 && parts[0][0] == '0') {
		return 0, errors.New("decimal is not canonical")
	}
	fraction := ""
	if len(parts) == 2 {
		fraction = parts[1]
	}
	digits := strings.TrimLeft(parts[0]+fraction, "0")
	if digits == "" {
		digits = "0"
	}
	if len(fraction) != scale || len(digits) > precision {
		return 0, errors.New("decimal precision or scale does not match")
	}
	for _, character := range parts[0] + fraction {
		if character < '0' || character > '9' {
			return 0, errors.New("decimal is not canonical")
		}
	}
	if negative && digits == "0" {
		return 0, errors.New("decimal is not canonical")
	}
	parsed, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return 0, errors.New("decimal exceeds signed 64-bit storage")
	}
	if negative {
		parsed = -parsed
	}
	return parsed, nil
}

func validateFiniteNumber(value json.Number) (any, error) {
	if integer, err := strconv.ParseInt(string(value), 10, 64); err == nil {
		return integer, nil
	}
	decimal, err := strconv.ParseFloat(string(value), 64)
	if err != nil || math.IsInf(decimal, 0) || math.IsNaN(decimal) {
		return nil, errors.New("number is out of range")
	}
	return decimal, nil
}
