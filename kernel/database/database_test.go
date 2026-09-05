package database

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"the8020/kernel/settings"
)

func sqliteConfig(location string) Config {
	return Config{Backend: BackendSQLite, Location: location, MaximumOpenConnections: 32, MaximumIdleConnections: 8}
}

func TestDecodeTimeAcceptsSQLiteCurrentTimestamp(t *testing.T) {
	decoded, err := DecodeTime("2026-09-03 20:59:43")
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, time.September, 3, 20, 59, 43, 0, time.UTC)
	if !decoded.Equal(want) || decoded.Location() != time.UTC {
		t.Fatalf("decoded time = %s, want %s", decoded, want)
	}
}

func TestJSONEncodingBelongsToTheDatabaseBackend(t *testing.T) {
	sqlite, err := EncodeJSON(BackendSQLite, map[string]any{"enabled": true})
	if err != nil || sqlite != `{"enabled":true}` {
		t.Fatalf("SQLite JSON = %#v, %v", sqlite, err)
	}
	postgresql, err := EncodeJSON(BackendPostgreSQL, map[string]any{"enabled": true})
	if err != nil {
		t.Fatal(err)
	}
	if value, ok := postgresql.(json.RawMessage); !ok || string(value) != `{"enabled":true}` {
		t.Fatalf("PostgreSQL JSON = %#v", postgresql)
	}
	if _, err := EncodeJSON("unsupported", nil); err == nil {
		t.Fatal("unsupported backend accepted")
	}
}

// The encoder and the parameter guard sit several layers apart, so nothing
// stopped them from disagreeing: EncodeJSON produced the value PostgreSQL
// needs and Manager.ready rejected it, which failed bootstrap activation on
// that backend while SQLite was unaffected. Driving the guard from the
// encoder's own output keeps the two halves tied together. ready is
// backend-agnostic, so SQLite proves the acceptance without a live server.
func TestEncodedJSONParametersReachTheDatabase(t *testing.T) {
	manager := New(sqliteConfig(filepath.Join(t.TempDir(), "database.db")))
	defer manager.Close()
	for _, backend := range []string{BackendSQLite, BackendPostgreSQL} {
		encoded, err := EncodeJSON(backend, map[string]any{"enabled": true})
		if err != nil {
			t.Fatalf("%s: %v", backend, err)
		}
		if _, err := manager.Query(context.Background(), "SELECT ? AS value", []any{encoded}); err != nil {
			t.Fatalf("%s parameter %T rejected: %v", backend, encoded, err)
		}
	}
}

func TestSQLiteCreatesPrivateInstanceDatabaseAndExecutesSQL(t *testing.T) {
	root := t.TempDir()
	config := sqliteConfig(InstanceRootPlaceholder + "/database/system.db")
	config.InstanceRoot = root
	manager := New(config)
	t.Cleanup(func() { _ = manager.Close() })
	ctx := context.Background()
	status, err := manager.Check(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != StateConnected {
		t.Fatalf("pre-catalog status = %#v", status)
	}
	status, err = manager.InitializeCatalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "database", "system.db")
	if status.State != StateConnected || status.Backend != BackendSQLite || status.Location != path {
		t.Fatalf("status = %#v", status)
	}
	if status.MaximumOpenConnections != 32 || status.MaximumIdleConnections != 8 || status.OpenConnections != 1 || status.IdleConnections != 1 {
		t.Fatalf("default pool status = %#v", status)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("database permissions = %v", info.Mode().Perm())
	}
	if _, err := manager.Execute(ctx, "CREATE TABLE example (id INTEGER PRIMARY KEY, name TEXT NOT NULL)", nil); err != nil {
		t.Fatal(err)
	}
	parameters, err := DecodeParameters([]byte(`[1, "Alice"]`))
	if err != nil {
		t.Fatal(err)
	}
	insert, err := manager.Execute(ctx, "INSERT INTO example (id, name) VALUES ($1, $2)", parameters)
	if err != nil || insert.RowsAffected != 1 {
		t.Fatalf("insert = %#v, %v", insert, err)
	}
	query, err := manager.Query(ctx, "SELECT id, name FROM example", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(query.Columns) != 2 || query.Columns[0] != "id" || len(query.Rows) != 1 || query.Rows[0][0] != int64(1) || query.Rows[0][1] != "Alice" || query.Truncated {
		t.Fatalf("query = %#v", query)
	}
	journal, err := manager.Query(ctx, "PRAGMA journal_mode", nil)
	if err != nil || len(journal.Rows) != 1 || journal.Rows[0][0] != "wal" {
		t.Fatalf("journal mode = %#v, %v", journal, err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		info, err := os.Stat(path + suffix)
		if err != nil {
			t.Fatalf("stat SQLite%s: %v", suffix, err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("SQLite%s permissions = %v", suffix, info.Mode().Perm())
		}
	}
}

func TestSQLiteWALAllowsAWriterAlongsideAReader(t *testing.T) {
	manager := New(Config{Backend: BackendSQLite, Location: filepath.Join(t.TempDir(), "database.db"), MaximumOpenConnections: 4, MaximumIdleConnections: 4})
	t.Cleanup(func() { _ = manager.Close() })
	ctx := context.Background()
	if _, err := manager.Execute(ctx, "CREATE TABLE example (id INTEGER PRIMARY KEY, value TEXT NOT NULL)", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Execute(ctx, "INSERT INTO example (id, value) VALUES (1, 'first')", nil); err != nil {
		t.Fatal(err)
	}
	reader, err := manager.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Rollback()
	var value string
	if err := reader.QueryRowContext(ctx, "SELECT value FROM example WHERE id = 1").Scan(&value); err != nil {
		t.Fatal(err)
	}
	writeContext, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if _, err := manager.Execute(writeContext, "INSERT INTO example (id, value) VALUES (2, 'second')", nil); err != nil {
		t.Fatalf("write while read transaction is open: %v", err)
	}
	status := manager.Status()
	if status.OpenConnections < 2 || status.InUseConnections < 1 {
		t.Fatalf("concurrent pool status = %#v", status)
	}
}

func TestConnectivityCheckPreservesCatalogFailureState(t *testing.T) {
	manager := New(sqliteConfig(filepath.Join(t.TempDir(), "database.db")))
	t.Cleanup(func() { _ = manager.Close() })
	ctx := context.Background()
	if _, err := manager.InitializeCatalog(ctx); err != nil {
		t.Fatal(err)
	}
	manager.SetInitializationFailure(ctx, fmt.Errorf("invalid package table"))
	status, err := manager.Check(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != StateInitializationFailed || status.CatalogError != "invalid package table" {
		t.Fatalf("status = %#v", status)
	}
}

func TestPoolPolicyAppliesLiveAndReportsPressure(t *testing.T) {
	manager := New(sqliteConfig(filepath.Join(t.TempDir(), "database.db")))
	t.Cleanup(func() { _ = manager.Close() })
	if _, err := manager.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	prepared, err := manager.Prepare(context.Background(), settings.Values{
		"database.maximum_open_connections": int64(1),
		"database.maximum_idle_connections": int64(1),
	})
	if err != nil {
		t.Fatal(err)
	}
	prepared.Commit()
	connection, err := manager.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	queryContext, cancelQuery := context.WithCancel(context.Background())
	defer cancelQuery()
	done := make(chan error, 1)
	go func() {
		_, queryErr := manager.Query(queryContext, "SELECT 1", nil)
		done <- queryErr
	}()
	deadline := time.Now().Add(time.Second)
	for manager.Status().WaitCount == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if status := manager.Status(); status.MaximumOpenConnections != 1 || status.MaximumIdleConnections != 1 || status.WaitCount == 0 {
		t.Fatalf("waiting pool status = %#v", status)
	}
	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("waiting database query did not resume")
	}
	prepared, err = manager.Prepare(context.Background(), settings.Values{
		"database.maximum_open_connections": int64(64),
		"database.maximum_idle_connections": int64(16),
	})
	if err != nil {
		t.Fatal(err)
	}
	prepared.Commit()
	if status := manager.Status(); status.MaximumOpenConnections != 64 || status.MaximumIdleConnections != 16 || status.WaitCount == 0 {
		t.Fatalf("resized pool status = %#v", status)
	}
	if _, err := manager.Prepare(context.Background(), settings.Values{
		"database.maximum_open_connections": int64(8),
		"database.maximum_idle_connections": int64(9),
	}); err == nil {
		t.Fatal("accepted idle connections above open connections")
	}
	prepared, err = manager.Prepare(context.Background(), settings.Values{
		"database.maximum_open_connections": int64(64),
		"database.maximum_idle_connections": int64(16),
		"database.maximum_result_rows":      int64(1),
		"database.maximum_result_bytes":     int64(64),
	})
	if err != nil {
		t.Fatal(err)
	}
	prepared.Commit()
	if status := manager.Status(); status.MaximumResultRows != 1 || status.MaximumResultBytes != 64 {
		t.Fatalf("live result limits = %#v", status)
	}
	if _, err := manager.Query(context.Background(), "SELECT 1 UNION ALL SELECT 2", nil); err == nil || !strings.Contains(err.Error(), "paginate") {
		t.Fatalf("live row limit error = %v", err)
	}
}

func TestSQLiteDoesNotChangeExistingParentPermissions(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "shared")
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	manager := New(sqliteConfig(filepath.Join(parent, "system.db")))
	t.Cleanup(func() { _ = manager.Close() })
	if _, err := manager.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(parent)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("existing parent permissions = %v", info.Mode().Perm())
	}
}

func TestQueryResultsAreBounded(t *testing.T) {
	config := sqliteConfig(filepath.Join(t.TempDir(), "database.db"))
	config.MaximumResultRows = 10
	config.MaximumResultBytes = 128
	manager := New(config)
	t.Cleanup(func() { _ = manager.Close() })
	ctx := context.Background()
	if _, err := manager.Query(ctx, "WITH RECURSIVE n(value) AS (VALUES(1) UNION ALL SELECT value + 1 FROM n WHERE value <= 10) SELECT value FROM n", nil); err == nil || !strings.Contains(err.Error(), "paginate") {
		t.Fatalf("oversized row result error = %v", err)
	}
	if _, err := manager.Query(ctx, "SELECT $1", []any{strings.Repeat("x", 129)}); err == nil || !strings.Contains(err.Error(), "paginate") {
		t.Fatalf("oversized byte result error = %v", err)
	}
}

func TestConfigurationAndParametersFailExplicitly(t *testing.T) {
	tests := []Config{
		{Backend: "other", Location: "/tmp/example.db", MaximumOpenConnections: 32, MaximumIdleConnections: 8},
		{Backend: BackendSQLite, Location: "relative.db", MaximumOpenConnections: 32, MaximumIdleConnections: 8},
		{Backend: BackendSQLite, Location: "${UNKNOWN}/example.db", MaximumOpenConnections: 32, MaximumIdleConnections: 8},
		{Backend: BackendPostgreSQL, Location: "postgresql://user:password@localhost/example", MaximumOpenConnections: 32, MaximumIdleConnections: 8},
		{Backend: BackendPostgreSQL, Location: "postgresql://localhost/example?password=secret", MaximumOpenConnections: 32, MaximumIdleConnections: 8},
		{Backend: BackendSQLite, Location: filepath.Join(t.TempDir(), "database.db"), MaximumOpenConnections: 8, MaximumIdleConnections: 9},
	}
	for _, config := range tests {
		manager := New(config)
		if _, err := manager.Check(context.Background()); err == nil {
			t.Fatalf("config %#v unexpectedly connected", config)
		}
	}
	for _, raw := range []string{`null`, `{}`, `[[]]`, `[1] trailing`} {
		if _, err := DecodeParameters([]byte(raw)); err == nil {
			t.Fatalf("parameters %q unexpectedly valid", raw)
		}
	}
	statement := strings.Repeat("x", maxStatementBytes+1)
	manager := New(sqliteConfig(filepath.Join(t.TempDir(), "database.db")))
	defer manager.Close()
	if _, err := manager.Query(context.Background(), statement, nil); err == nil {
		t.Fatal("oversized statement accepted")
	}
}

func TestPostgreSQLStatusNeverDisplaysURLCredentials(t *testing.T) {
	manager := New(Config{Backend: BackendPostgreSQL, Location: "postgresql://alice:secret@localhost/example?password=other&sslmode=disable", MaximumOpenConnections: 32, MaximumIdleConnections: 8})
	status, err := manager.Check(context.Background())
	if err == nil {
		t.Fatal("credential-bearing URL unexpectedly accepted")
	}
	if strings.Contains(status.Location, "alice") || strings.Contains(status.Location, "secret") || strings.Contains(status.Location, "other") || status.Location != "postgresql://localhost/example?sslmode=disable" {
		t.Fatalf("credential-bearing status location = %q", status.Location)
	}
}

func BenchmarkSQLitePoolReadOnly(b *testing.B) {
	benchmarkSQLitePool(b, 0)
}

func BenchmarkSQLitePoolMixed(b *testing.B) {
	benchmarkSQLitePool(b, 10)
}

func benchmarkSQLitePool(b *testing.B, writeEvery uint64) {
	for _, maximumOpen := range []int{8, 16, 32, 64} {
		b.Run(fmt.Sprintf("connections-%d", maximumOpen), func(b *testing.B) {
			b.StopTimer()
			manager := New(Config{
				Backend: BackendSQLite, Location: filepath.Join(b.TempDir(), "benchmark.db"),
				MaximumOpenConnections: maximumOpen, MaximumIdleConnections: maximumOpen,
			})
			defer manager.Close()
			ctx := context.Background()
			if _, err := manager.Execute(ctx, "CREATE TABLE benchmark_data (id INTEGER PRIMARY KEY, value INTEGER NOT NULL)", nil); err != nil {
				b.Fatal(err)
			}
			if _, err := manager.Execute(ctx, "WITH RECURSIVE rows(id) AS (VALUES(1) UNION ALL SELECT id + 1 FROM rows WHERE id < 1024) INSERT INTO benchmark_data SELECT id, id FROM rows", nil); err != nil {
				b.Fatal(err)
			}
			var sequence atomic.Uint64
			var failed atomic.Bool
			b.SetParallelism(16)
			b.ResetTimer()
			b.StartTimer()
			b.RunParallel(func(worker *testing.PB) {
				for worker.Next() && !failed.Load() {
					operation := sequence.Add(1)
					id := int64(operation%1024 + 1)
					if writeEvery != 0 && operation%writeEvery == 0 {
						if _, err := manager.Execute(ctx, "UPDATE benchmark_data SET value = value + 1 WHERE id = $1", []any{id}); err != nil && failed.CompareAndSwap(false, true) {
							b.Errorf("write: %v", err)
						}
						continue
					}
					if _, err := manager.Query(ctx, "SELECT value FROM benchmark_data WHERE id = $1", []any{id}); err != nil && failed.CompareAndSwap(false, true) {
						b.Errorf("read: %v", err)
					}
				}
			})
		})
	}
}
