package callback

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"the8020/kernel/auth"
	"the8020/kernel/database"
	"the8020/kernel/runtime/operations"
	"the8020/kernel/sandbox/model"
	"the8020/kernel/sandbox/state"
	"the8020/kernel/services"
)

// Exercises ordinary package source through the real Worker, Unix callback,
// signer and SQLite driver. No installed instance or external store is touched.
func TestPasswordUsersWorkerAndRecovery(t *testing.T) {
	deno, err := exec.LookPath("deno")
	if err != nil {
		t.Skip("requires pinned Deno on PATH")
	}
	workspace, err := filepath.Abs("../../../..")
	if err != nil {
		t.Fatal(err)
	}
	fixture := filepath.Join(workspace, "users/src/password_runtime_fixture.ts")
	if _, err := os.Stat(fixture); os.IsNotExist(err) {
		t.Skip("requires sibling users source workspace")
	}
	root := t.TempDir()
	store, err := state.New(filepath.Join(root, "sandboxes"))
	if err != nil {
		t.Fatal(err)
	}
	spec, _ := callbackFixtureForWorkload(t, store, model.WorkloadService)
	db := database.New(database.Config{Backend: database.BackendSQLite, Location: filepath.Join(root, "system.db"), MaximumOpenConnections: 8, MaximumIdleConnections: 4})
	defer db.Close()
	for _, sql := range []string{
		`CREATE TABLE the8020__users__users (username TEXT PRIMARY KEY, fullName TEXT NOT NULL DEFAULT '', passwordHash TEXT NOT NULL DEFAULT '', enabled INTEGER NOT NULL DEFAULT 1, authVersion INTEGER NOT NULL DEFAULT 1, createdAt TEXT NOT NULL DEFAULT '2026-01-01T00:00:00Z', updatedAt TEXT NOT NULL DEFAULT '2026-01-01T00:00:00Z')`,
		`CREATE TABLE the8020__users__sessions (sessionId TEXT PRIMARY KEY, username TEXT NOT NULL, authVersion INTEGER NOT NULL, createdAt TEXT NOT NULL, expiresAt TEXT NOT NULL, type TEXT NOT NULL, transport TEXT NOT NULL)`,
		`CREATE TABLE the8020__system__settings (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`INSERT INTO the8020__system__settings VALUES ('system.profile','{"id":"sys-aaaaaaaaaa","name":"fixture","role":"test"}')`,
		`INSERT INTO the8020__users__users (username) VALUES ('alice'),('other')`,
		`CREATE TABLE custom_store (value TEXT NOT NULL)`,
		`INSERT INTO custom_store VALUES ('preserve')`,
	} {
		if _, err := db.Execute(t.Context(), sql, nil); err != nil {
			t.Fatal(err)
		}
	}
	server := newCallbackTestServer(t, store, func(c *Config) { c.Database = db })
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	defer server.Close(context.Background())
	for _, phase := range []string{"fresh", "restart"} {
		signer, err := auth.OpenSigner(filepath.Join(root, "key"), "")
		if err != nil {
			t.Fatal(err)
		}
		dispatcher, err := operations.New(&services.Services{Signing: signer})
		if err != nil {
			t.Fatal(err)
		}
		server.SetRuntimeOperations(dispatcher)
		ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
		command := exec.CommandContext(ctx, deno, "run", "--cached-only", "--frozen", "--unstable-worker-options", "--allow-read="+workspace+","+server.Address(), "--allow-write="+server.Address(), "--allow-net", "--allow-import", fixture, server.Address(), spec.InternalToken, phase)
		command.Dir = filepath.Join(workspace, "users")
		output, err := command.CombinedOutput()
		cancel()
		if err != nil {
			t.Fatalf("%s fixture: %v\n%s", phase, err, output)
		}
		t.Logf("%s: %s", phase, output)
	}
}
