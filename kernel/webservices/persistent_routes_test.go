package webservices

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPersistentRoutesAreSharedAcrossNodesWithoutPersistingTokens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "persistent-routes.json")
	nodeA := newPersistentRouteRegistry("node-a", path)
	nodeB := newPersistentRouteRegistry("node-b", path)
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
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), token) {
		t.Fatal("shared route state persisted the bearer token")
	}
	nodeA.succeed(token, "worker-a")
	resolved, err = nodeB.lookup(token, "example/realtime/channel", "user-1")
	if err != nil || resolved.WorkerID != "worker-a" {
		t.Fatalf("updated route=%#v err=%v", resolved, err)
	}
}

func TestConnectedRouteStillExpiresWithoutOwningNodeHeartbeat(t *testing.T) {
	registry := newPersistentRouteRegistry("node-a")
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
	registry := newPersistentRouteRegistry("node-a")
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
	registry := newPersistentRouteRegistry("node-a")
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
	registry := newPersistentRouteRegistry("node-a")
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
