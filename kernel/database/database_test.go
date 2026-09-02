package database

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSQLiteCreatesPrivateInstanceDatabaseAndExecutesSQL(t *testing.T) {
	root := t.TempDir()
	manager := New(Config{Backend: BackendSQLite, Location: InstanceRootPlaceholder + "/database/system.db", InstanceRoot: root})
	t.Cleanup(func() { _ = manager.Close() })
	ctx := context.Background()
	status, err := manager.Check(ctx)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "database", "system.db")
	if status.State != StateReady || status.Backend != BackendSQLite || status.Location != path {
		t.Fatalf("status = %#v", status)
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
}

func TestSQLiteDoesNotChangeExistingParentPermissions(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "shared")
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	manager := New(Config{Backend: BackendSQLite, Location: filepath.Join(parent, "system.db")})
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
	manager := New(Config{Backend: BackendSQLite, Location: filepath.Join(t.TempDir(), "database.db")})
	t.Cleanup(func() { _ = manager.Close() })
	ctx := context.Background()
	result, err := manager.Query(ctx, "WITH RECURSIVE n(value) AS (VALUES(1) UNION ALL SELECT value + 1 FROM n WHERE value <= 1000) SELECT value FROM n", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Rows) != maxResultRows || !result.Truncated {
		t.Fatalf("rows=%d truncated=%t", len(result.Rows), result.Truncated)
	}
	result, err = manager.Query(ctx, "SELECT $1", []any{strings.Repeat("x", maxResultBytes)})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Rows) != 0 || !result.Truncated {
		t.Fatalf("oversized row count=%d truncated=%t", len(result.Rows), result.Truncated)
	}
}

func TestConfigurationAndParametersFailExplicitly(t *testing.T) {
	tests := []Config{
		{Backend: "other", Location: "/tmp/example.db"},
		{Backend: BackendSQLite, Location: "relative.db"},
		{Backend: BackendSQLite, Location: "${UNKNOWN}/example.db"},
		{Backend: BackendPostgreSQL, Location: "postgresql://user:password@localhost/example"},
		{Backend: BackendPostgreSQL, Location: "postgresql://localhost/example?password=secret"},
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
	manager := New(Config{Backend: BackendSQLite, Location: filepath.Join(t.TempDir(), "database.db")})
	defer manager.Close()
	if _, err := manager.Query(context.Background(), statement, nil); err == nil {
		t.Fatal("oversized statement accepted")
	}
}

func TestPostgreSQLStatusNeverDisplaysURLCredentials(t *testing.T) {
	manager := New(Config{Backend: BackendPostgreSQL, Location: "postgresql://alice:secret@localhost/example?password=other&sslmode=disable"})
	status, err := manager.Check(context.Background())
	if err == nil {
		t.Fatal("credential-bearing URL unexpectedly accepted")
	}
	if strings.Contains(status.Location, "alice") || strings.Contains(status.Location, "secret") || strings.Contains(status.Location, "other") || status.Location != "postgresql://localhost/example?sslmode=disable" {
		t.Fatalf("credential-bearing status location = %q", status.Location)
	}
}
