package database

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestRuntimeStatementProtocolPreservesBytesAndTransactions(t *testing.T) {
	manager := New(sqliteConfig(filepath.Join(t.TempDir(), "system.db")))
	t.Cleanup(func() { _ = manager.Close() })
	ctx := context.Background()
	if _, err := manager.Execute(ctx, `CREATE TABLE values_test (id INTEGER PRIMARY KEY, payload BLOB NOT NULL)`, nil); err != nil {
		t.Fatal(err)
	}
	token, err := manager.BeginTransaction(ctx, "request-1", TransactionSettings{})
	if err != nil {
		t.Fatal(err)
	}
	parameters, _ := json.Marshal([]any{1, map[string]any{"type": "bytes", "value": "AP7/"}})
	result, err := manager.RunStatement(ctx, "request-1", StatementRequest{
		Statement: `INSERT INTO values_test (id, payload) VALUES ($1, $2)`, Parameters: parameters,
		ReturnInsertID: true, Transaction: token,
	})
	if err != nil || result.AffectedRows == nil || result.InsertID == nil {
		t.Fatalf("insert = %#v, %v", result, err)
	}
	updateParameters, _ := json.Marshal([]any{map[string]any{"type": "bytes", "value": "AP7/"}, 1})
	updated, err := manager.RunStatement(ctx, "request-1", StatementRequest{
		Statement: `UPDATE values_test SET payload = $1 WHERE id = $2`, Parameters: updateParameters,
		Transaction: token,
	})
	if err != nil || updated.InsertID != nil {
		t.Fatalf("update = %#v, %v", updated, err)
	}
	if err := manager.FinishTransaction(ctx, "request-1", token, true); err != nil {
		t.Fatal(err)
	}
	query, err := manager.RunStatement(ctx, "request-2", StatementRequest{Statement: `SELECT id, payload FROM values_test`, ReturnRows: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(query.Rows) != 1 || query.Rows[0][1].(map[string]any)["type"] != "bytes" {
		t.Fatalf("query = %#v", query)
	}
}

func TestTransactionLifetimeInterruptsActiveStatement(t *testing.T) {
	manager := New(sqliteConfig(filepath.Join(t.TempDir(), "system.db")))
	defer manager.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	token, err := manager.BeginTransaction(ctx, "bounded", TransactionSettings{TimeoutMS: 50})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.FinishTransaction(context.Background(), "bounded", token, false)
	start := time.Now()
	_, err = manager.RunStatement(ctx, "bounded", StatementRequest{
		Statement:  `WITH RECURSIVE count(n) AS (VALUES(0) UNION ALL SELECT n+1 FROM count WHERE n<100000000) SELECT sum(n) FROM count`,
		ReturnRows: true, Transaction: token,
	})
	if err == nil || time.Since(start) > 500*time.Millisecond {
		t.Fatalf("transaction lifetime did not interrupt active statement: %v %v", time.Since(start), err)
	}
}

func TestRuntimePostgreSQLJSONPassesCommonParameterValidation(t *testing.T) {
	config := sqliteConfig(filepath.Join(t.TempDir(), "unused.db"))
	config.Backend, config.Location = BackendPostgreSQL, "postgresql://localhost/test"
	manager := New(config)
	defer manager.Close()
	values, err := manager.decodeRuntimeParameters(json.RawMessage(`[{"type":"json","value":{"nested":[true,7,null]}}]`))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := values[0].(json.RawMessage); !ok {
		t.Fatalf("JSON driver parameter = %T", values[0])
	}
	if err := manager.ready("SELECT $1::jsonb", values); err != nil {
		t.Fatal(err)
	}
	if err := manager.ready("SELECT $1::jsonb", []any{json.RawMessage(`{invalid`)}); err == nil {
		t.Fatal("accepted invalid raw JSON")
	}
}

func TestTransactionTokensAreExecutionScopedAndCleanedUp(t *testing.T) {
	manager := New(sqliteConfig(filepath.Join(t.TempDir(), "system.db")))
	t.Cleanup(func() { _ = manager.Close() })
	ctx := context.Background()
	if _, err := manager.Execute(ctx, `CREATE TABLE tx_test (id INTEGER PRIMARY KEY)`, nil); err != nil {
		t.Fatal(err)
	}
	token, err := manager.BeginTransaction(ctx, "job-1", TransactionSettings{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.RunStatement(ctx, "job-2", StatementRequest{Statement: `INSERT INTO tx_test VALUES (1)`, Transaction: token}); err == nil {
		t.Fatal("another execution used a transaction token")
	}
	manager.CloseScope("job-1")
	if err := manager.FinishTransaction(ctx, "job-1", token, true); err == nil {
		t.Fatal("cleaned transaction remained usable")
	}
}

func TestTransactionOutlivesBeginCallbackContext(t *testing.T) {
	manager := New(sqliteConfig(filepath.Join(t.TempDir(), "system.db")))
	t.Cleanup(func() { _ = manager.Close() })
	ctx := context.Background()
	if _, err := manager.Execute(ctx, `CREATE TABLE callback_tx (id INTEGER PRIMARY KEY)`, nil); err != nil {
		t.Fatal(err)
	}
	callback, cancel := context.WithCancel(ctx)
	token, err := manager.BeginTransaction(callback, "group\x00sandbox\x00worker\x00request", TransactionSettings{})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if _, err := manager.RunStatement(ctx, "group\x00sandbox\x00worker\x00request", StatementRequest{
		Statement: `INSERT INTO callback_tx VALUES (1)`, Transaction: token,
	}); err != nil {
		t.Fatalf("transaction ended with begin callback: %v", err)
	}
	if err := manager.FinishTransaction(ctx, "group\x00sandbox\x00worker\x00request", token, true); err != nil {
		t.Fatal(err)
	}
	result, err := manager.Query(ctx, `SELECT COUNT(*) FROM callback_tx`, nil)
	if err != nil || result.Rows[0][0] != int64(1) {
		t.Fatalf("committed rows = %#v, %v", result, err)
	}
}

func TestWorkerScopeCleanupUsesSeparatorBoundary(t *testing.T) {
	manager := New(sqliteConfig(filepath.Join(t.TempDir(), "system.db")))
	t.Cleanup(func() { _ = manager.Close() })
	ctx := context.Background()
	first, err := manager.BeginTransaction(ctx, "group\x00sandbox\x00worker\x00request-1", TransactionSettings{})
	if err != nil {
		t.Fatal(err)
	}
	other, err := manager.BeginTransaction(ctx, "group\x00sandbox\x00worker-extra\x00request-2", TransactionSettings{})
	if err != nil {
		t.Fatal(err)
	}
	manager.CloseScopePrefix("group\x00sandbox\x00worker")
	if err := manager.FinishTransaction(ctx, "group\x00sandbox\x00worker\x00request-1", first, false); err == nil {
		t.Fatal("worker transaction survived prefix cleanup")
	}
	if err := manager.FinishTransaction(ctx, "group\x00sandbox\x00worker-extra\x00request-2", other, false); err != nil {
		t.Fatalf("neighboring worker transaction was removed: %v", err)
	}
}

func TestTransactionAdmissionIsBoundedByCallerAndReleased(t *testing.T) {
	config := sqliteConfig(filepath.Join(t.TempDir(), "system.db"))
	config.MaximumOpenConnections = 3
	config.MaximumIdleConnections = 1
	manager := New(config)
	t.Cleanup(func() { _ = manager.Close() })
	first, err := manager.BeginTransaction(context.Background(), "request-1", TransactionSettings{})
	if err != nil {
		t.Fatal(err)
	}
	wait, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := manager.BeginTransaction(wait, "request-2", TransactionSettings{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second transaction error = %v, want deadline exceeded", err)
	}
	if err := manager.FinishTransaction(context.Background(), "request-1", first, false); err != nil {
		t.Fatal(err)
	}
	second, err := manager.BeginTransaction(context.Background(), "request-2", TransactionSettings{})
	if err != nil {
		t.Fatalf("released permit was not reusable: %v", err)
	}
	manager.CloseScope("request-2")
	manager.CloseScope("request-2")
	if err := manager.FinishTransaction(context.Background(), "request-2", second, false); err == nil {
		t.Fatal("idempotent scope cleanup left the transaction registered")
	}
}

func TestApplicationTransactionsLeaveConnectionsForKernelWork(t *testing.T) {
	config := sqliteConfig(filepath.Join(t.TempDir(), "system.db"))
	config.MaximumOpenConnections = 4
	config.MaximumIdleConnections = 2
	manager := New(config)
	t.Cleanup(func() { _ = manager.Close() })
	for _, scope := range []string{"request-1", "request-2"} {
		if _, err := manager.BeginTransaction(context.Background(), scope, TransactionSettings{}); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result, err := manager.Query(ctx, "SELECT 1", nil)
	if err != nil || len(result.Rows) != 1 {
		t.Fatalf("reserved kernel query = %#v, %v", result, err)
	}
	manager.CloseScope("request-1")
	manager.CloseScope("request-2")
}

func TestCancelledRuntimeQueryReleasesApplicationAdmission(t *testing.T) {
	config := sqliteConfig(filepath.Join(t.TempDir(), "system.db"))
	config.MaximumOpenConnections = 3
	config.MaximumIdleConnections = 1
	manager := New(config)
	t.Cleanup(func() { _ = manager.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := manager.RunStatement(ctx, "request-1", StatementRequest{Statement: "SELECT 1", ReturnRows: true}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled query error = %v", err)
	}
	result, err := manager.RunStatement(context.Background(), "request-2", StatementRequest{Statement: "SELECT 1", ReturnRows: true})
	if err != nil || len(result.Rows) != 1 {
		t.Fatalf("admission was not released after cancellation: %#v, %v", result, err)
	}
}

func TestScaledDecimalAcceptsFractionsAndEnforcesSigned64Storage(t *testing.T) {
	tests := []struct {
		value     string
		precision int
		scale     int
		expected  int64
	}{
		{value: "0.50", precision: 2, scale: 2, expected: 50},
		{value: "-0.50", precision: 2, scale: 2, expected: -50},
		{value: "999999999999999999", precision: 18, expected: 999999999999999999},
	}
	for _, test := range tests {
		actual, err := scaledDecimal(test.value, test.precision, test.scale)
		if err != nil || actual != test.expected {
			t.Fatalf("scaledDecimal(%q) = %d, %v; want %d", test.value, actual, err, test.expected)
		}
	}
	for _, test := range []struct {
		value     string
		precision int
		scale     int
	}{
		{value: "-0.00", scale: 2},
		{value: "1000000000000000000"},
		{value: "9223372036854775808", precision: 19},
	} {
		precision := test.precision
		if precision == 0 {
			precision = 18
		}
		if _, err := scaledDecimal(test.value, precision, test.scale); err == nil {
			t.Fatalf("scaledDecimal(%q) succeeded", test.value)
		}
	}
}

func TestRuntimeDatetimeUsesCanonicalUTCMilliseconds(t *testing.T) {
	value, err := runtimeValue(time.Date(2026, time.January, 2, 3, 4, 5, 123456789, time.FixedZone("test", 3600)), "TIMESTAMPTZ")
	if err != nil || value.(map[string]any)["value"] != "2026-01-02T02:04:05.123Z" {
		t.Fatalf("datetime = %#v, %v", value, err)
	}
}

func TestTransactionLockTimeoutIsBoundedAndConnectionSettingRestored(t *testing.T) {
	manager := New(sqliteConfig(filepath.Join(t.TempDir(), "system.db")))
	defer manager.Close()
	ctx := context.Background()
	if _, err := manager.Execute(ctx, `CREATE TABLE claims (id INTEGER PRIMARY KEY, node TEXT)`, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Execute(ctx, `INSERT INTO claims VALUES (1,'')`, nil); err != nil {
		t.Fatal(err)
	}
	first, err := manager.BeginTransaction(ctx, "first", TransactionSettings{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = manager.RunStatement(ctx, "first", StatementRequest{Statement: `UPDATE claims SET node='a' WHERE id=1`, Transaction: first})
	if err != nil {
		t.Fatal(err)
	}
	wait := int64(25)
	second, err := manager.BeginTransaction(ctx, "second", TransactionSettings{LockTimeoutMS: &wait, TimeoutMS: 1000})
	if err != nil {
		t.Fatal(err)
	}
	manager.transactionsMu.Lock()
	entry := manager.transactions[second]
	manager.transactionsMu.Unlock()
	start := time.Now()
	_, err = manager.RunStatement(ctx, "second", StatementRequest{Statement: `UPDATE claims SET node='b' WHERE id=1`, Transaction: second})
	if err == nil || time.Since(start) > 500*time.Millisecond {
		t.Fatalf("contended write took %v: %v", time.Since(start), err)
	}
	if err = manager.FinishTransaction(ctx, "second", second, false); err != nil {
		t.Fatal(err)
	}
	if err = manager.FinishTransaction(ctx, "first", first, false); err != nil {
		t.Fatal(err)
	}
	if entry.restore == nil {
		t.Fatal("missing pooled connection restoration")
	}
	for range 4 {
		token, err := manager.BeginTransaction(ctx, "normal", TransactionSettings{})
		if err != nil {
			t.Fatal(err)
		}
		tx, _ := manager.transaction(token, "normal")
		var value int
		if err = tx.tx.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&value); err != nil || value == 25 {
			t.Fatalf("timeout leaked: %d %v", value, err)
		}
		_ = manager.FinishTransaction(ctx, "normal", token, false)
	}
}
