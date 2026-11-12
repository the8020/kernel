package database

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"the8020/kernel/settings"
)

func passwordSettings(t *testing.T, file, password string, manager *Manager) *settings.Manager {
	t.Helper()
	definitions := []settings.Definition{
		{Key: "database.password", Type: settings.TypeString, Storage: settings.StorageNode, Default: "", Environment: "THE8020_TEST_PASSWORD", RuntimeMutable: true, Description: "Password."},
		{Key: "database.username", Type: settings.TypeString, Storage: settings.StorageNode, Default: "", Environment: "THE8020_TEST_USERNAME", RestartRequired: true, Description: "Username."},
		{Key: "database.maximum_open_connections", Type: settings.TypeInteger, Storage: settings.StorageNode, Default: int64(4), Environment: "THE8020_TEST_OPEN", RuntimeMutable: true, Description: "Open."},
		{Key: "database.maximum_idle_connections", Type: settings.TypeInteger, Storage: settings.StorageNode, Default: int64(0), Environment: "THE8020_TEST_IDLE", RuntimeMutable: true, Description: "Idle."},
	}
	configured, err := settings.New(definitions, settings.PersistencePaths{Node: file}, map[string]string{"database.password": password}, func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	if err := configured.RegisterApplier([]string{"database.password", "database.maximum_open_connections", "database.maximum_idle_connections"}, manager); err != nil {
		t.Fatal(err)
	}
	return configured
}

func TestSQLitePasswordIsLiveButIgnored(t *testing.T) {
	manager := New(sqliteConfig(filepath.Join(t.TempDir(), "system.db")))
	defer manager.Close()
	configured := passwordSettings(t, filepath.Join(t.TempDir(), "kernel.toml"), "", manager)
	info, err := configured.Set(context.Background(), "database.password", "ignored")
	if err != nil || info.ActiveValue != "ignored" || info.RestartPending {
		t.Fatalf("SQLite password = %#v, %v", info, err)
	}
	if _, err := manager.Query(context.Background(), "SELECT 1", nil); err != nil {
		t.Fatal(err)
	}
}

// Explicit opt-in: this test creates and drops a disposable PostgreSQL login role.
func TestPostgreSQLPasswordRotation(t *testing.T) {
	if os.Getenv("THE8020_TEST_POSTGRES_PASSWORD_ROTATION") != "true" || os.Getenv("THE8020_TEST_POSTGRES_LOCATION") == "" {
		t.Skip("set THE8020_TEST_POSTGRES_PASSWORD_ROTATION=true and PostgreSQL admin connection settings")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	config := Config{Backend: BackendPostgreSQL, Location: os.Getenv("THE8020_TEST_POSTGRES_LOCATION"), Username: os.Getenv("THE8020_TEST_POSTGRES_USERNAME"), Password: os.Getenv("THE8020_TEST_POSTGRES_PASSWORD"), MaximumOpenConnections: 4}
	admin := New(config)
	defer admin.Close()
	role := fmt.Sprintf("test_password_%d", time.Now().UnixNano())
	quotedRole := pgx.Identifier{role}.Sanitize()
	const original, replacement = "initial-test-password", "rotated-test-'password\\value"
	if _, err := admin.Execute(ctx, "CREATE ROLE "+quotedRole+" LOGIN PASSWORD '"+original+"'", nil); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := admin.Execute(cleanup, "DROP ROLE "+quotedRole, nil); err != nil {
			t.Error(err)
		}
	}()
	rotate := func(password string) {
		t.Helper()
		if _, err := admin.Execute(ctx, "ALTER ROLE "+quotedRole+" PASSWORD '"+strings.ReplaceAll(password, "'", "''")+"'", nil); err != nil {
			t.Fatal(err)
		}
	}
	config.Username, config.Password = role, original
	manager := New(config)
	defer manager.Close()
	file := filepath.Join(t.TempDir(), "kernel.toml")
	configured := passwordSettings(t, file, original, manager)
	held, err := manager.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Rollback()
	var pid, transaction int64
	if err := held.QueryRowContext(ctx, "SELECT pg_backend_pid(), txid_current()").Scan(&pid, &transaction); err != nil {
		t.Fatal(err)
	}
	assertActive := func(password string) {
		t.Helper()
		info, err := configured.Get("database.password")
		if err != nil || info.ActiveValue != password || info.ConfiguredValue != password || info.RestartPending || manager.postgresPassword.Load() != password {
			t.Fatal("credential policy changed unexpectedly")
		}
	}
	const invalid = "invalid-secret-must-not-appear"
	if _, err := configured.Set(ctx, "database.password", invalid); err == nil || strings.Contains(err.Error(), invalid) {
		t.Fatal("invalid credential was accepted or exposed")
	}
	assertActive(original)
	rotate(replacement)
	if err := os.Mkdir(file, 0o700); err != nil {
		t.Fatal(err)
	}
	_, err = configured.Set(ctx, "database.password", replacement)
	var operation *settings.OperationError
	if !errors.As(err, &operation) || operation.Kind != settings.ErrorPersistence {
		t.Fatalf("credential persistence failure = %v", err)
	}
	assertActive(original)
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	// Pending authority changes must not redirect password validation or live connections.
	if _, err := configured.Set(ctx, "database.username", "pending-other-authority"); err != nil {
		t.Fatal(err)
	}
	var changes sync.WaitGroup
	for key, value := range map[string]string{"database.password": replacement, "database.maximum_open_connections": "8"} {
		changes.Go(func() {
			if _, err := configured.Set(ctx, key, value); err != nil {
				t.Error(err)
			}
		})
	}
	changes.Wait()
	assertActive(replacement)
	var retainedPID, retainedTransaction int64
	if err := held.QueryRowContext(ctx, "SELECT pg_backend_pid(), txid_current()").Scan(&retainedPID, &retainedTransaction); err != nil || pid != retainedPID || transaction != retainedTransaction {
		t.Fatalf("in-flight transaction changed: %v", err)
	}
	if err := held.Commit(); err != nil {
		t.Fatal(err)
	}
	var queries sync.WaitGroup
	for range 6 {
		queries.Go(func() {
			for range 5 {
				result, err := manager.Query(ctx, "SELECT current_user", nil)
				if err != nil || len(result.Rows) != 1 || result.Rows[0][0] != role {
					t.Errorf("future connection failed: %v", err)
					return
				}
			}
		})
	}
	queries.Wait()
	// Reload the persisted password into an ordinary new manager.
	reloaded := passwordSettings(t, file, original, manager)
	password, _ := reloaded.Active("database.password")
	config.Password = password.(string)
	restarted := New(config)
	if _, err := restarted.Check(ctx); err != nil {
		t.Fatal(err)
	}
	_ = restarted.Close()
	if _, err := configured.Unset(ctx, "database.password"); err == nil {
		t.Fatal("unset accepted an invalid lower-precedence password")
	}
	assertActive(replacement)
	rotate(original)
	if _, err := configured.Unset(ctx, "database.password"); err != nil {
		t.Fatal(err)
	}
	assertActive(original)
	if _, err := manager.Check(ctx); err != nil {
		t.Fatal(err)
	}
	reloaded = passwordSettings(t, file, original, manager)
	if password, _ := reloaded.Active("database.password"); password != original {
		t.Fatal("unset override survived reload")
	}
}
