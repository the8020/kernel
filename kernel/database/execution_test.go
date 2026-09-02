package database

import (
	"context"
	"encoding/json"
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
		Statement: `INSERT INTO values_test (id, payload) VALUES ($1, $2)`, Parameters: parameters, Transaction: token,
	})
	if err != nil || result.AffectedRows == nil {
		t.Fatalf("insert = %#v, %v", result, err)
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
