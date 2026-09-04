package webservices

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"the8020/kernel/database"
)

func newTestRouteDatabase(t *testing.T, root string) *database.Manager {
	t.Helper()
	db := database.New(database.Config{Backend: database.BackendSQLite, Location: filepath.Join(root, "system.db"), MaximumOpenConnections: 8, MaximumIdleConnections: 2})
	if _, err := db.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(context.Background(), `CREATE TABLE IF NOT EXISTS "the8020__services__routes" ("tokenHash" TEXT PRIMARY KEY, "serviceId" TEXT NOT NULL, "nodeId" TEXT NOT NULL, "poolId" TEXT NOT NULL, "runtimeGroupId" TEXT NOT NULL, "sandboxId" TEXT NOT NULL, "workerId" TEXT NOT NULL, "executionId" TEXT NOT NULL, "userId" TEXT NOT NULL, "keepAliveMs" INTEGER NOT NULL, "expiresAt" TEXT NOT NULL, "connected" INTEGER NOT NULL) STRICT`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestPersistentRoutesAreSharedAcrossNodesWithoutPersistingTokens(t *testing.T) {
	db := newTestRouteDatabase(t, t.TempDir())
	nodeA := newPersistentRouteRegistry("node-a", db)
	nodeB := newPersistentRouteRegistry("node-b", db)
	token, created, err := nodeA.create("example/realtime/channel", "pool-a", "group-a", "sandbox-a", "user-1", 2*time.Minute, false)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := nodeB.lookup(token, "example/realtime/channel", "user-1")
	if err != nil || resolved.NodeID != "node-a" || resolved.ExecutionID != created.ExecutionID {
		t.Fatalf("resolved=%#v err=%v", resolved, err)
	}
	if _, err := nodeB.lookup(token, "example/realtime/channel", "user-2"); !errors.Is(err, errRouteNotFound) {
		t.Fatalf("route was not bound to its authenticated user: %v", err)
	}
	var storedHash string
	if err := db.QueryRowContext(context.Background(), `SELECT "tokenHash" FROM "the8020__services__routes" WHERE "executionId" = $1`, created.ExecutionID).Scan(&storedHash); err != nil || storedHash == token || storedHash != tokenKey(token) {
		t.Fatalf("route token persistence = %q, %v", storedHash, err)
	}
	nodeA.succeed(token, "worker-a")
	resolved, err = nodeB.lookup(token, "example/realtime/channel", "user-1")
	if err != nil || resolved.WorkerID != "worker-a" {
		t.Fatalf("updated route=%#v err=%v", resolved, err)
	}
}

func TestConnectedRouteStillExpiresWithoutOwningNodeHeartbeat(t *testing.T) {
	registry := newPersistentRouteRegistry("node-a", newTestRouteDatabase(t, t.TempDir()))
	now := time.Unix(1_700_000_000, 0).UTC()
	registry.now = func() time.Time { return now }
	token, _, err := registry.create("example/realtime/channel", "pool-a", "group-a", "sandbox-a", "user-1", time.Minute, true)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	if _, err := registry.lookup(token, "example/realtime/channel", "user-1"); !errors.Is(err, errRouteExpired) {
		t.Fatalf("route survived a dead owning-node heartbeat: %v", err)
	}
}

func TestRouteHeartbeatRefreshesTransportLease(t *testing.T) {
	registry := newPersistentRouteRegistry("node-a", newTestRouteDatabase(t, t.TempDir()))
	now := time.Unix(1_700_000_000, 0).UTC()
	registry.now = func() time.Time { return now }
	token, _, err := registry.create("example/realtime/channel", "pool-a", "group-a", "sandbox-a", "user-1", time.Minute, true)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(45 * time.Second)
	registry.succeed(token, "worker-a")
	now = now.Add(45 * time.Second)
	resolved, err := registry.lookup(token, "example/realtime/channel", "user-1")
	if err != nil || resolved.WorkerID != "worker-a" {
		t.Fatalf("resolved=%#v err=%v", resolved, err)
	}
}

func TestDiscardExecutionInvalidatesItsOpaqueRoute(t *testing.T) {
	registry := newPersistentRouteRegistry("node-a", newTestRouteDatabase(t, t.TempDir()))
	token, record, err := registry.create("example/realtime/channel", "pool-a", "group-a", "sandbox-a", "user-1", time.Minute, false)
	if err != nil {
		t.Fatal(err)
	}
	registry.discardExecution(record.ExecutionID)
	if _, err := registry.lookup(token, "example/realtime/channel", "user-1"); !errors.Is(err, errRouteNotFound) {
		t.Fatalf("discarded execution route resolved: %v", err)
	}
}

func TestExactPersistentCompletionInvalidatesOnlyMatchingRoute(t *testing.T) {
	registry := newPersistentRouteRegistry("node-a", newTestRouteDatabase(t, t.TempDir()))
	token, record, err := registry.create("example/realtime/channel", "pool-a", "group-a", "sandbox-a", "user-1", time.Minute, false)
	if err != nil {
		t.Fatal(err)
	}
	registry.succeed(token, "worker-a")
	if err := registry.complete(record.ExecutionID, record.ServiceID, record.RuntimeGroupID, record.SandboxID, "worker-other"); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("mismatched completion error=%v", err)
	}
	if _, err := registry.lookup(token, record.ServiceID, record.UserID); err != nil {
		t.Fatalf("mismatched completion removed route: %v", err)
	}
	if err := registry.complete(record.ExecutionID, record.ServiceID, record.RuntimeGroupID, record.SandboxID, "worker-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.lookup(token, record.ServiceID, record.UserID); !errors.Is(err, errRouteNotFound) {
		t.Fatalf("completed route still resolves: %v", err)
	}
}
