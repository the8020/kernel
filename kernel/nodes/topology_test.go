package nodes

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"the8020/kernel/database"
)

const testSharedSecret = "shared-node-test-secret"

func newTestNodeDatabase(t *testing.T, root string) *database.Manager {
	t.Helper()
	db := database.New(database.Config{Backend: database.BackendSQLite, Location: filepath.Join(root, "system.db"), MaximumOpenConnections: 8, MaximumIdleConnections: 2})
	if _, err := db.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(context.Background(), `CREATE TABLE IF NOT EXISTS "the8020__system__nodes" ("id" TEXT PRIMARY KEY, "url" TEXT NOT NULL, "recipientAddress" TEXT NOT NULL, "recipientPort" INTEGER NOT NULL, "enabled" INTEGER NOT NULL, "updatedAt" TEXT NOT NULL) STRICT`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

type staticCapacityProvider struct{ capacity Capacity }

func (p staticCapacityProvider) NodeCapacity(context.Context) (Capacity, error) {
	return p.capacity, nil
}

type recordingWorkerInvoker struct {
	calls []WorkerInvocationRequest
}

func (i *recordingWorkerInvoker) InvokeLocalWorker(_ context.Context, input WorkerInvocationRequest) WorkerInvocationResult {
	i.calls = append(i.calls, input)
	return WorkerInvocationResult{OK: true, Output: "exact-worker-output"}
}

func TestTopologyPersistsAndReloadsSharedNodes(t *testing.T) {
	root := t.TempDir()
	manager, err := New(newTestNodeDatabase(t, root), "node-a", testSharedSecret)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	observer, err := New(newTestNodeDatabase(t, root), "node-c", testSharedSecret)
	if err != nil {
		t.Fatal(err)
	}
	defer observer.Close()
	node := Node{ID: "node-b", URL: "https://node-b.example", RecipientAddress: "10.0.0.2", RecipientPort: 9443, Enabled: true}
	if _, err := manager.Set(context.Background(), node); err != nil {
		t.Fatal(err)
	}
	if err := observer.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, err := observer.Inspect("node-b"); err != nil || got != node {
		t.Fatalf("running peer node=%#v err=%v", got, err)
	}
	reloaded, err := New(newTestNodeDatabase(t, root), "node-a", testSharedSecret)
	if err != nil {
		t.Fatal(err)
	}
	defer reloaded.Close()
	if got, err := reloaded.Inspect("node-b"); err != nil || got != node {
		t.Fatalf("node=%#v err=%v", got, err)
	}
}

func TestTopologyReadsUseTheRefreshedSnapshot(t *testing.T) {
	db := newTestNodeDatabase(t, t.TempDir())
	manager, err := New(db, "node-a", testSharedSecret)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	if _, err := manager.Set(context.Background(), Node{ID: "node-b", URL: "https://node-b.example", RecipientAddress: "10.0.0.2", RecipientPort: 9443, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(context.Background(), `DROP TABLE "the8020__system__nodes"`); err != nil {
		t.Fatal(err)
	}
	if nodes := manager.List(); len(nodes) != 1 || nodes[0].ID != "node-b" {
		t.Fatalf("cached nodes=%#v", nodes)
	}
	if _, err := manager.Inspect("node-b"); err != nil {
		t.Fatalf("cached inspect: %v", err)
	}
	if err := manager.Refresh(context.Background()); err == nil {
		t.Fatal("refresh unexpectedly succeeded without the topology table")
	}
}

func TestIndexesArePartitionedAcrossEnabledNodes(t *testing.T) {
	manager, err := New(newTestNodeDatabase(t, t.TempDir()), "node-b", testSharedSecret)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	for index, id := range []string{"node-a", "node-b", "node-c"} {
		if _, err := manager.Set(context.Background(), Node{ID: id, URL: "https://" + id + ".example", RecipientAddress: "10.0.0." + strconv.Itoa(index+1), RecipientPort: 9443, Enabled: true}); err != nil {
			t.Fatal(err)
		}
	}
	indexes := manager.LocalIndexes(8)
	if len(indexes) != 3 || indexes[0] != 1 || indexes[1] != 4 || indexes[2] != 7 {
		t.Fatalf("indexes=%#v", indexes)
	}
	configured, _ := manager.Inspect("node-b")
	configured.Enabled = false
	if _, err := manager.Set(context.Background(), configured); err != nil {
		t.Fatal(err)
	}
	if indexes := manager.LocalIndexes(8); len(indexes) != 0 {
		t.Fatalf("disabled local indexes=%#v", indexes)
	}
}

func TestForwardingRecipientRequiresSharedAuthentication(t *testing.T) {
	root := t.TempDir()
	db := newTestNodeDatabase(t, root)
	manager, err := New(db, "node-a", testSharedSecret)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	port := freePort(t)
	if _, err := manager.Set(context.Background(), Node{ID: "node-a", URL: "http://127.0.0.1", RecipientAddress: "127.0.0.1", RecipientPort: port, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := manager.Start(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) { _, _ = writer.Write([]byte("forwarded")) })); err != nil {
		t.Fatal(err)
	}
	unauthorized, err := http.Get("http://127.0.0.1:" + portString(port) + "/service")
	if err != nil {
		t.Fatal(err)
	}
	unauthorized.Body.Close()
	if unauthorized.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status=%d", unauthorized.StatusCode)
	}

	peer, err := New(db, "node-b", testSharedSecret)
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	if _, err := peer.Set(context.Background(), Node{ID: "node-a", URL: "http://127.0.0.1", RecipientAddress: "127.0.0.1", RecipientPort: port, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	if err := peer.Proxy("node-a", recorder, httptest.NewRequest(http.MethodGet, "http://the8020/service", nil)); err != nil {
		t.Fatal(err)
	}
	response := recorder.Result()
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || string(body) != "forwarded" {
		t.Fatalf("status=%d body=%q", response.StatusCode, body)
	}
}

func TestAvailableForwardingUsesAdvertisedCapacity(t *testing.T) {
	root := t.TempDir()
	db := newTestNodeDatabase(t, root)
	owner, err := New(db, "node-a", testSharedSecret)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	port := freePort(t)
	configured := Node{ID: "node-a", URL: "http://127.0.0.1", RecipientAddress: "127.0.0.1", RecipientPort: port, Enabled: true}
	if _, err := owner.Set(context.Background(), configured); err != nil {
		t.Fatal(err)
	}
	owner.SetCapacityProvider(staticCapacityProvider{capacity: Capacity{Accepting: true, AvailableWorkers: 8, AvailableSandboxes: 2, UpdatedAt: time.Now().UTC()}})
	if err := owner.Start(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) { _, _ = writer.Write([]byte("capacity-routed")) })); err != nil {
		t.Fatal(err)
	}
	peer, err := New(db, "node-b", testSharedSecret)
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	recorder := httptest.NewRecorder()
	forwarded, err := peer.ProxyAvailable(recorder, httptest.NewRequest(http.MethodGet, "http://the8020/core/service/path", nil))
	if err != nil || !forwarded {
		t.Fatalf("forwarded=%v err=%v", forwarded, err)
	}
	response := recorder.Result()
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || string(body) != "capacity-routed" {
		t.Fatalf("status=%d body=%q", response.StatusCode, body)
	}
	statuses := peer.Statuses(context.Background())
	if len(statuses) != 2 || !statuses[0].Reachable || statuses[0].Capacity == nil || statuses[0].Capacity.NodeID != "node-a" {
		t.Fatalf("statuses=%#v", statuses)
	}
}

func TestExactWorkerInvocationForwardsAcrossNodes(t *testing.T) {
	db := newTestNodeDatabase(t, t.TempDir())
	owner, err := New(db, "node-a", testSharedSecret)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	port := freePort(t)
	if _, err := owner.Set(context.Background(), Node{ID: "node-a", URL: "http://127.0.0.1", RecipientAddress: "127.0.0.1", RecipientPort: port, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	invoker := &recordingWorkerInvoker{}
	owner.SetWorkerInvoker(invoker)
	if err := owner.Start(http.NotFoundHandler()); err != nil {
		t.Fatal(err)
	}

	peer, err := New(db, "node-b", testSharedSecret)
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	input := WorkerInvocationRequest{NodeID: "node-a", SandboxID: "sandbox-a", WorkerID: "worker-a", Function: "example.inspect", Input: map[string]any{"id": "value"}}
	result := peer.InvokeWorker(context.Background(), input)
	if !result.OK || result.Output != "exact-worker-output" || len(invoker.calls) != 1 || invoker.calls[0].NodeID != "node-a" || invoker.calls[0].SandboxID != "sandbox-a" || invoker.calls[0].WorkerID != "worker-a" || invoker.calls[0].Function != "example.inspect" {
		t.Fatalf("result=%#v calls=%#v", result, invoker.calls)
	}
	if value, ok := invoker.calls[0].Input.(map[string]any)["id"]; !ok || value != "value" {
		t.Fatalf("opaque input=%#v", invoker.calls[0].Input)
	}
}

func TestWorkerInvocationRejectsInvalidAndOversizedInputBeforeDispatch(t *testing.T) {
	manager, err := New(newTestNodeDatabase(t, t.TempDir()), "node-a", testSharedSecret)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	invoker := &recordingWorkerInvoker{}
	manager.SetWorkerInvoker(invoker)
	invalid := WorkerInvocationRequest{NodeID: "node-a", SandboxID: "sandbox-a", Function: "example.inspect"}
	if result := manager.InvokeWorker(context.Background(), invalid); result.Error == nil || result.Error.Code != "invalid_request" {
		t.Fatalf("invalid result=%#v", result)
	}
	oversized := WorkerInvocationRequest{NodeID: "node-a", SandboxID: "sandbox-a", WorkerID: "worker-a", Function: "example.inspect", Input: strings.Repeat("x", maximumWorkerInvocationBytes)}
	if result := manager.InvokeWorker(context.Background(), oversized); result.Error == nil || result.Error.Code != "invalid_request" {
		t.Fatalf("oversized result=%#v", result)
	}
	if len(invoker.calls) != 0 {
		t.Fatalf("invalid request reached invoker: %#v", invoker.calls)
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

func portString(port int) string {
	return strconv.Itoa(port)
}
