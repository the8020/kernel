package database

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// Uses a disposable PostgreSQL database when explicitly configured by the test
// runner. The uniquely named table is removed before this test returns.
func TestPostgreSQLRuntimeJSONAndShortClaimLocks(t *testing.T) {
	location := os.Getenv("THE8020_TEST_POSTGRES_LOCATION")
	if location == "" {
		t.Skip("set THE8020_TEST_POSTGRES_LOCATION for PostgreSQL integration")
	}
	config := sqliteConfig(filepath.Join(t.TempDir(), "unused.db"))
	config.Backend, config.Location, config.Username = BackendPostgreSQL, location, os.Getenv("THE8020_TEST_POSTGRES_USERNAME")
	m := New(config)
	defer m.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	table := fmt.Sprintf("test_runtime_claim_%d", time.Now().UnixNano())
	if _, err := m.Execute(ctx, `CREATE TABLE `+table+` (id INTEGER PRIMARY KEY, payload JSONB, node TEXT NOT NULL DEFAULT '')`, nil); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = m.Execute(cleanup, `DROP TABLE `+table, nil)
	}()
	params := json.RawMessage(`[{"type":"json","value":{"nested":[true,7,null]}}]`)
	if _, err := m.RunStatement(ctx, "insert", StatementRequest{Statement: `INSERT INTO ` + table + ` (id,payload) VALUES (1,$1)`, Parameters: params}); err != nil {
		t.Fatal(err)
	}
	result, err := m.RunStatement(ctx, "read", StatementRequest{Statement: `SELECT payload FROM ` + table + ` WHERE id=1`, ReturnRows: true})
	if err != nil {
		t.Fatal(err)
	}
	expected := map[string]any{"type": "json", "value": map[string]any{"nested": []any{true, float64(7), nil}}}
	if len(result.Rows) != 1 || !reflect.DeepEqual(result.Rows[0][0], expected) {
		t.Fatalf("JSON round trip=%#v", result)
	}
	holder, err := m.BeginTransaction(ctx, "holder", TransactionSettings{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.FinishTransaction(context.Background(), "holder", holder, false)
	if _, err = m.RunStatement(ctx, "holder", StatementRequest{Statement: `UPDATE ` + table + ` SET node='holder' WHERE id=1`, Transaction: holder}); err != nil {
		t.Fatal(err)
	}
	limit := int64(25)
	contender, err := m.BeginTransaction(ctx, "contender", TransactionSettings{TimeoutMS: 2000, LockTimeoutMS: &limit})
	if err != nil {
		t.Fatal(err)
	}
	defer m.FinishTransaction(context.Background(), "contender", contender, false)
	start := time.Now()
	_, err = m.RunStatement(ctx, "contender", StatementRequest{Statement: `UPDATE ` + table + ` SET node='contender' WHERE id=1 AND node='' RETURNING id`, ReturnRows: true, Transaction: contender})
	elapsed := time.Since(start)
	if err == nil || elapsed > 500*time.Millisecond {
		t.Fatalf("contended claim took %v: %v", elapsed, err)
	}
	_ = m.FinishTransaction(ctx, "contender", contender, false)
	_ = m.FinishTransaction(ctx, "holder", holder, false)
	claim, err := m.RunStatement(ctx, "winner", StatementRequest{Statement: `UPDATE ` + table + ` SET node='winner' WHERE id=1 AND node='' RETURNING id`, ReturnRows: true})
	if err != nil || len(claim.Rows) != 1 {
		t.Fatalf("unlocked claim=%#v %v", claim, err)
	}
	loser, err := m.RunStatement(ctx, "loser", StatementRequest{Statement: `UPDATE ` + table + ` SET node='loser' WHERE id=1 AND node='' RETURNING id`, ReturnRows: true})
	if err != nil || len(loser.Rows) != 0 {
		t.Fatalf("claim was overwritten: %#v %v", loser, err)
	}
	t.Logf("PostgreSQL JSON round trip and conditional claim passed; contended lock returned in %s", elapsed)
	bounded, err := m.BeginTransaction(ctx, "bounded", TransactionSettings{TimeoutMS: 100})
	if err != nil {
		t.Fatal(err)
	}
	defer m.FinishTransaction(context.Background(), "bounded", bounded, false)
	start = time.Now()
	_, err = m.RunStatement(ctx, "bounded", StatementRequest{Statement: "SELECT pg_sleep(1)", ReturnRows: true, Transaction: bounded})
	if err == nil || time.Since(start) > 500*time.Millisecond {
		t.Fatalf("transaction lifetime did not interrupt active query: %v %v", time.Since(start), err)
	}
}
