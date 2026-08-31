package debugging

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"the8020/kernel/ports"
	"the8020/kernel/sandbox/model"
)

type fakePorts struct {
	requests []ports.Request
	leases   []ports.Lease
	closed   []string
	handler  http.Handler
}

func (f *fakePorts) ExposeHTTP(_ context.Context, request ports.Request, handler http.Handler) (ports.Lease, error) {
	f.requests = append(f.requests, request)
	f.handler = handler
	lease := ports.Lease{LeaseID: "debug-one", SandboxID: request.SandboxID, BindAddress: request.BindAddress, HostPort: 12345, InternalPort: request.InternalPort, Purpose: request.Purpose, ExpiresAt: request.ExpiresAt}
	f.leases = append(f.leases, lease)
	return lease, nil
}
func (f *fakePorts) List() []ports.Lease   { return append([]ports.Lease(nil), f.leases...) }
func (f *fakePorts) Close(id string) error { f.closed = append(f.closed, id); return nil }

func TestTargetsMapDebuggerNames(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/json/list" {
			http.NotFound(writer, request)
			return
		}
		_, _ = io.WriteString(writer, `[{"id":"target-1","type":"node","title":"service:owner:execution-42:worker-1","description":"Worker","url":"file:///app.ts","webSocketDebuggerUrl":"ws://runtime/target-1"}]`)
	}))
	defer server.Close()
	portManager := &fakePorts{}
	manager, err := New(Config{Ports: portManager, HTTPClient: server.Client(), InspectorEndpoint: func(model.SandboxSpec) (string, error) { return server.URL, nil }})
	if err != nil {
		t.Fatal(err)
	}
	targets, err := manager.Targets(context.Background(), model.SandboxSpec{})
	if err != nil || len(targets) != 1 || targets[0].ExecutionID != "execution-42" || targets[0].WebSocketDebugger == "" {
		t.Fatalf("targets=%#v err=%v", targets, err)
	}
}

func TestOpenIsLoopbackBoundedAndCloseDelegates(t *testing.T) {
	inspector := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(writer, `[{"id":"target","type":"node","title":"job:owner:execution:worker","webSocketDebuggerUrl":"ws://10.88.0.2:9229/target"}]`)
	}))
	defer inspector.Close()
	portManager := &fakePorts{leases: []ports.Lease{{LeaseID: "other", Purpose: "service"}}}
	manager, err := New(Config{Ports: portManager, HTTPClient: inspector.Client(), InspectorEndpoint: func(model.SandboxSpec) (string, error) { return inspector.URL, nil }, Enabled: true, DefaultDuration: time.Minute, MaximumDuration: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	spec := model.SandboxSpec{SandboxID: "sandbox", Network: model.NetworkConfiguration{SandboxIP: "10.88.0.2"}}
	lease, err := manager.Open(context.Background(), spec, 2*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	request := portManager.requests[0]
	if request.BindAddress != "127.0.0.1" || request.InternalPort != InspectorPort || request.Purpose != "debug" || lease.PortLease.Purpose != "debug" || len(lease.AccessToken) != 64 || len(lease.Targets) != 1 || !strings.Contains(lease.Targets[0].WebSocketDebugger, "token=") {
		t.Fatalf("request=%#v lease=%#v", request, lease)
	}
	unauthorized := httptest.NewRecorder()
	portManager.handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "http://debug/json/list", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status=%d", unauthorized.Code)
	}
	authorized := httptest.NewRecorder()
	portManager.handler.ServeHTTP(authorized, httptest.NewRequest(http.MethodGet, "http://debug/json/list?token="+lease.AccessToken, nil))
	if authorized.Code != http.StatusOK || !strings.Contains(authorized.Body.String(), `"id":"target"`) {
		t.Fatalf("authorized status=%d body=%q", authorized.Code, authorized.Body.String())
	}
	if _, err := manager.Open(context.Background(), spec, 2*time.Hour); err == nil {
		t.Fatal("accepted excessive duration")
	}
	if len(manager.List()) != 1 || manager.List()[0].Purpose != "debug" {
		t.Fatalf("list=%#v", manager.List())
	}
	if err := manager.Close("debug-one"); err != nil || len(portManager.closed) != 1 {
		t.Fatalf("close err=%v closed=%#v", err, portManager.closed)
	}
}

func TestOpenRequiresEnabledPolicyAndLoopbackConfiguration(t *testing.T) {
	portManager := &fakePorts{}
	manager, err := New(Config{Ports: portManager})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Open(context.Background(), model.SandboxSpec{}, time.Minute); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("disabled open error=%v", err)
	}
	if _, err := New(Config{Ports: portManager, Enabled: true, BindAddress: "0.0.0.0"}); err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("public bind error=%v", err)
	}
}
