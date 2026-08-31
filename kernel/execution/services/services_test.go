package services

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"the8020/kernel/execution/coordinator"
	"the8020/kernel/execution/records"
	"the8020/kernel/execution/supervisor"
	"the8020/kernel/execution/workers"
	"the8020/kernel/ports"
	"the8020/kernel/sandbox/manager"
	"the8020/kernel/sandbox/model"
)

type fakeCoordinator struct {
	requests   []coordinator.Request
	releases   []string
	ensureErr  error
	releaseErr error
}

func (f *fakeCoordinator) Ensure(_ context.Context, request coordinator.Request) (manager.Inspection, error) {
	f.requests = append(f.requests, request)
	if f.ensureErr != nil {
		return manager.Inspection{}, f.ensureErr
	}
	return manager.Inspection{Spec: model.SandboxSpec{SandboxID: "sandbox", RuntimeGroupID: "group", WorkloadType: model.WorkloadService, Network: model.NetworkConfiguration{SandboxIP: "10.88.0.2"}, Permissions: model.Permissions{ReadPaths: []string{"/programs"}}}}, nil
}

func (f *fakeCoordinator) Release(_ context.Context, groupID, ownerID, serviceID string) error {
	f.releases = append(f.releases, groupID+":"+ownerID+":"+serviceID)
	return f.releaseErr
}

type configuration struct {
	serviceID string
	workers   []string
	maximum   int
}
type fakeWorkers struct {
	starts         []supervisor.StartWorkerRequest
	startErr       error
	stops          []string
	configurations []configuration
	websocketCalls []string
	inFlight       map[string]int
	states         map[string]string
	lifecycle      []string
	listErr        error
	stopErrors     map[string]error
}

func TestServiceRestoreIsolatesCorruptAndRouteFailedRecords(t *testing.T) {
	root := t.TempDir()
	store, err := records.New(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range []Record{
		{ServiceID: "a-route-failure", LogicalServiceID: "example/api/a", Entrypoint: "file:///a.ts", WorkerIDs: []string{}, ReleaseID: "current", State: "READY", PathPrefix: "/taken", ExecutionMode: "stateless", TargetUtilization: 0.7},
		{ServiceID: "b-healthy", LogicalServiceID: "example/api/b", Entrypoint: "file:///b.ts", WorkerIDs: []string{}, ReleaseID: "current", State: "READY", PathPrefix: "/healthy", ExecutionMode: "stateless", TargetUtilization: 0.7},
	} {
		if err := store.Save(record.ServiceID, record); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "c-corrupt.json"), []byte(`{"service_id":"c-corrupt","unknown":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	router := &fakeRouter{handlers: map[string]http.Handler{"/taken": http.NotFoundHandler()}}
	manager, err := New(&fakeCoordinator{}, &fakeWorkers{}, store, router, &fakePorts{}, Policy{Strategy: model.GroupingOwner})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	if router.handlers["/healthy"] == nil {
		t.Fatal("healthy service route was not restored")
	}
	failed, err := manager.Inspect("a-route-failure")
	if err != nil || failed.State != "FAILED" || !strings.Contains(failed.Failure, "restore route") {
		t.Fatalf("failed=%#v err=%v", failed, err)
	}
	if _, err := os.Stat(filepath.Join(root, "c-corrupt.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("corrupt live record still exists: %v", err)
	}
	quarantined, err := filepath.Glob(filepath.Join(root, "quarantine", "c-corrupt-*.json"))
	if err != nil || len(quarantined) != 1 {
		t.Fatalf("quarantined=%#v err=%v", quarantined, err)
	}
}

func (f *fakeWorkers) Start(_ context.Context, group string, request supervisor.StartWorkerRequest) (workers.Record, error) {
	f.starts = append(f.starts, request)
	if f.startErr != nil {
		return workers.Record{}, f.startErr
	}
	return workers.Record{RuntimeGroupID: group, Worker: supervisor.WorkerStatus{WorkerID: request.Metadata.WorkerID}}, nil
}
func (f *fakeWorkers) List(_ context.Context, group string) ([]workers.Record, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	stopped := map[string]bool{}
	for _, id := range f.stops {
		stopped[id] = true
	}
	var result []workers.Record
	for _, request := range f.starts {
		if !stopped[request.Metadata.WorkerID] {
			state := f.states[request.Metadata.WorkerID]
			if state == "" {
				state = "ready"
			}
			result = append(result, workers.Record{RuntimeGroupID: group, Worker: supervisor.WorkerStatus{WorkerID: request.Metadata.WorkerID, WorkloadID: request.Metadata.WorkloadID, InFlight: f.inFlight[request.Metadata.WorkerID], State: state}})
		}
	}
	return result, nil
}

func TestServiceRestoreMarksAStaleRuntimeRecordFailedSoItCanRestart(t *testing.T) {
	store, err := records.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	record := Record{ServiceID: "api", LogicalServiceID: "example/api/main", Entrypoint: "file:///api.ts", RuntimeGroupID: "missing-group", SandboxID: "missing-sandbox", WorkerIDs: []string{"missing-worker"}, State: "READY", ExecutionMode: "stateless"}
	if err := store.Save(record.ServiceID, record); err != nil {
		t.Fatal(err)
	}
	workersFake := &fakeWorkers{listErr: context.Canceled}
	manager, err := New(&fakeCoordinator{}, workersFake, store, &fakeRouter{}, &fakePorts{}, Policy{Strategy: model.GroupingOwner})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	failed, err := manager.Inspect("api")
	if err != nil || failed.State != "FAILED" || !strings.Contains(failed.Failure, "restore runtime group") {
		t.Fatalf("failed=%#v err=%v", failed, err)
	}
}

func TestServiceRestoreRejectsPersistedFailedWorker(t *testing.T) {
	store, err := records.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const workerID = "worker-failed"
	record := Record{ServiceID: "api", LogicalServiceID: "example/api/main", Entrypoint: "file:///api.ts", RuntimeGroupID: "group", SandboxID: "sandbox", WorkerIDs: []string{workerID}, State: "READY", ExecutionMode: "stateless"}
	if err := store.Save(record.ServiceID, record); err != nil {
		t.Fatal(err)
	}
	workersFake := &fakeWorkers{
		starts: []supervisor.StartWorkerRequest{{Metadata: supervisor.ExecutionMetadata{WorkerID: workerID, WorkloadID: record.ServiceID}}},
		states: map[string]string{workerID: "failed"},
	}
	manager, err := New(&fakeCoordinator{}, workersFake, store, &fakeRouter{}, &fakePorts{}, Policy{Strategy: model.GroupingOwner})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	failed, err := manager.Inspect(record.ServiceID)
	if err != nil || failed.State != "FAILED" || !strings.Contains(failed.Failure, "persisted Workers are unavailable") {
		t.Fatalf("failed=%#v err=%v", failed, err)
	}
}
func (f *fakeWorkers) StopInGroup(_ context.Context, _ string, workerID string, _ bool) error {
	if err := f.stopErrors[workerID]; err != nil {
		return err
	}
	f.stops = append(f.stops, workerID)
	f.lifecycle = append(f.lifecycle, "stop:"+workerID)
	return nil
}

func TestServiceStopPersistsProgressAndResumes(t *testing.T) {
	store, _ := records.New(t.TempDir())
	workersFake := &fakeWorkers{stopErrors: map[string]error{}}
	manager, err := New(&fakeCoordinator{}, workersFake, store, &fakeRouter{}, &fakePorts{}, Policy{Strategy: model.GroupingOwner, MinimumWorkers: 2, MaximumWorkers: 2, MaximumInFlight: 1})
	if err != nil {
		t.Fatal(err)
	}
	record, err := manager.Start(context.Background(), "api", "file:///programs/api.ts", Options{})
	if err != nil {
		t.Fatal(err)
	}
	workersFake.stopErrors[record.WorkerIDs[1]] = errors.New("temporary stop failure")
	workersFake.lifecycle = nil
	if err := manager.Stop(context.Background(), record.ServiceID); err == nil {
		t.Fatal("partial stop unexpectedly succeeded")
	}
	draining, err := manager.Inspect(record.ServiceID)
	if err != nil || draining.State != "DRAINING" || len(draining.WorkerIDs) != 1 || draining.WorkerIDs[0] != record.WorkerIDs[1] {
		t.Fatalf("draining=%#v err=%v", draining, err)
	}
	if len(workersFake.lifecycle) == 0 || workersFake.lifecycle[0] != "configure:" {
		t.Fatalf("pool was not excluded before termination: %#v", workersFake.lifecycle)
	}

	delete(workersFake.stopErrors, record.WorkerIDs[1])
	if err := manager.Stop(context.Background(), record.ServiceID); err != nil {
		t.Fatal(err)
	}
	stopped, err := manager.Inspect(record.ServiceID)
	if err != nil || stopped.State != "STOPPED" || len(stopped.WorkerIDs) != 0 {
		t.Fatalf("stopped=%#v err=%v", stopped, err)
	}
}

func TestServiceStopRetiresMissingRuntimeGroupAndRecord(t *testing.T) {
	store, _ := records.New(t.TempDir())
	workersFake := &fakeWorkers{listErr: os.ErrNotExist}
	coordinatorFake := &fakeCoordinator{}
	manager, err := New(coordinatorFake, workersFake, store, &fakeRouter{}, &fakePorts{}, Policy{Strategy: model.GroupingOwner})
	if err != nil {
		t.Fatal(err)
	}
	record := Record{
		ServiceID: "stale-pool", LogicalServiceID: "core/example/service",
		RuntimeGroupID: "missing-group", SandboxID: "missing-sandbox",
		WorkerIDs: []string{"missing-worker"}, WorkerRequests: map[string]int{"missing-worker": 3},
		State: "FAILED", ExecutionMode: "stateless", TargetUtilization: 0.7,
	}
	if err := store.Save(record.ServiceID, record); err != nil {
		t.Fatal(err)
	}
	if err := manager.Stop(context.Background(), record.ServiceID); err != nil {
		t.Fatal(err)
	}
	stopped, err := manager.Inspect(record.ServiceID)
	if err != nil || stopped.State != "STOPPED" || len(stopped.WorkerIDs) != 0 || len(stopped.WorkerRequests) != 0 {
		t.Fatalf("stopped=%#v err=%v", stopped, err)
	}
	if err := manager.RemoveStopped(record.ServiceID); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Inspect(record.ServiceID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("retired pool record still exists: %v", err)
	}
}

func TestServiceStartDiscardsRecordWhenGroupWasNeverAcquired(t *testing.T) {
	store, _ := records.New(t.TempDir())
	manager, err := New(&fakeCoordinator{ensureErr: errors.New("shared-group label update failed")}, &fakeWorkers{}, store, &fakeRouter{}, &fakePorts{}, Policy{Strategy: model.GroupingOwner})
	if err != nil {
		t.Fatal(err)
	}
	failed, err := manager.Start(context.Background(), "unassigned-pool", "file:///programs/api.ts", Options{})
	if err == nil || failed.State != "FAILED" || !strings.Contains(err.Error(), "label update failed") {
		t.Fatalf("failed=%#v err=%v", failed, err)
	}
	if _, err := manager.Inspect("unassigned-pool"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unowned failed-start record still exists: %v", err)
	}
}

func TestRejectedServiceStartReleasesGroupAndDiscardsPoolRecord(t *testing.T) {
	store, _ := records.New(t.TempDir())
	coordinatorFake := &fakeCoordinator{}
	workersFake := &fakeWorkers{startErr: &supervisor.ResponseError{Method: http.MethodPost, Path: "/v1/workers/start", Status: "400 Bad Request", StatusCode: http.StatusBadRequest, Message: "service type check failed"}}
	manager, err := New(coordinatorFake, workersFake, store, &fakeRouter{}, &fakePorts{}, Policy{Strategy: model.GroupingOwner, MinimumWorkers: 1, MaximumWorkers: 1, MaximumInFlight: 1})
	if err != nil {
		t.Fatal(err)
	}
	failed, err := manager.Start(context.Background(), "rejected-pool", "file:///programs/api.ts", Options{})
	if !errors.Is(err, ErrInvalidServiceDefinition) || failed.State != "FAILED" || !strings.Contains(err.Error(), "type check failed") {
		t.Fatalf("failed=%#v err=%v", failed, err)
	}
	if len(coordinatorFake.releases) != 1 {
		t.Fatalf("released groups=%#v", coordinatorFake.releases)
	}
	if _, err := manager.Inspect("rejected-pool"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("clean failed-start pool record still exists: %v", err)
	}
}

func TestServiceStopTreatsAnAlreadyRemovedRuntimeGroupAsReleased(t *testing.T) {
	store, _ := records.New(t.TempDir())
	record := Record{ServiceID: "stale-pool", LogicalServiceID: "core/example/service", RuntimeGroupID: "missing-group", SandboxID: "missing-sandbox", WorkerIDs: []string{"missing-worker"}, WorkerRequests: map[string]int{"missing-worker": 1}, State: "FAILED", ExecutionMode: "stateless", TargetUtilization: 0.7}
	if err := store.Save(record.ServiceID, record); err != nil {
		t.Fatal(err)
	}
	coordinatorFake := &fakeCoordinator{releaseErr: fmt.Errorf("sandbox missing: %w", os.ErrNotExist)}
	manager, err := New(coordinatorFake, &fakeWorkers{listErr: os.ErrNotExist}, store, &fakeRouter{}, &fakePorts{}, Policy{Strategy: model.GroupingOwner})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Stop(context.Background(), record.ServiceID); err != nil {
		t.Fatal(err)
	}
	stopped, err := manager.Inspect(record.ServiceID)
	if err != nil || stopped.State != "STOPPED" || len(stopped.WorkerIDs) != 0 {
		t.Fatalf("stopped=%#v err=%v", stopped, err)
	}
}

func TestRetireUnavailableRequiresNoRuntimeCalls(t *testing.T) {
	store, _ := records.New(t.TempDir())
	workersFake := &fakeWorkers{listErr: errors.New("must not list Workers")}
	manager, err := New(&fakeCoordinator{releaseErr: errors.New("must not release group")}, workersFake, store, &fakeRouter{}, &fakePorts{}, Policy{Strategy: model.GroupingOwner})
	if err != nil {
		t.Fatal(err)
	}
	record := Record{
		ServiceID: "failed-pool", LogicalServiceID: "core/example/service",
		RuntimeGroupID: "absent-group", SandboxID: "absent-sandbox",
		WorkerIDs: []string{"absent-worker"}, WorkerRequests: map[string]int{"absent-worker": 2},
		State: "FAILED", PathPrefix: "/old", PortLeaseID: "old-lease", HostPort: 1234,
		ExecutionMode: "stateless", TargetUtilization: 0.7,
	}
	if err := store.Save(record.ServiceID, record); err != nil {
		t.Fatal(err)
	}
	if err := manager.RetireUnavailable(record.ServiceID, "sandbox absent during startup"); err != nil {
		t.Fatal(err)
	}
	retired, err := manager.Inspect(record.ServiceID)
	if err != nil || retired.State != "STOPPED" || len(retired.WorkerIDs) != 0 || len(retired.WorkerRequests) != 0 || retired.PathPrefix != "" || retired.PortLeaseID != "" || retired.HostPort != 0 || !strings.Contains(retired.Failure, "sandbox absent") {
		t.Fatalf("retired=%#v err=%v", retired, err)
	}
}
func (f *fakeWorkers) ConfigureService(_ context.Context, _ string, serviceID string, workerIDs []string, maximum int) error {
	f.configurations = append(f.configurations, configuration{serviceID, append([]string(nil), workerIDs...), maximum})
	f.lifecycle = append(f.lifecycle, "configure:"+strings.Join(workerIDs, ","))
	return nil
}

func (f *fakeWorkers) ServiceOpenAPI(context.Context, string, string) (map[string]any, error) {
	return map[string]any{"openapi": "3.1.0"}, nil
}
func (f *fakeWorkers) DispatchService(_ context.Context, _, _ string, request *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(request.Body)
	workerID := ""
	if len(f.starts) > 0 {
		workerID = f.starts[len(f.starts)-1].Metadata.WorkerID
	}
	return &http.Response{StatusCode: http.StatusAccepted, Header: http.Header{"X-Service": []string{"test"}, "X-80-20-Runtime-Worker-Id": []string{workerID}}, Body: io.NopCloser(strings.NewReader("reply:" + string(body)))}, nil
}
func (f *fakeWorkers) ProxyServiceWebSocket(_ context.Context, group, serviceID string, _ http.ResponseWriter, _ *http.Request) error {
	f.websocketCalls = append(f.websocketCalls, group+":"+serviceID)
	return nil
}
func TestPersistentModeUsesTheSamePrewarmedHTTPAndWebSocketWorkerPool(t *testing.T) {
	store, _ := records.New(t.TempDir())
	workersFake := &fakeWorkers{}
	manager, err := New(&fakeCoordinator{}, workersFake, store, &fakeRouter{}, &fakePorts{}, Policy{
		Strategy: model.GroupingOwner, MinimumWorkers: 2, MaximumWorkers: 8, MaximumInFlight: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	record, err := manager.Start(context.Background(), "chat", "file:///programs/chat.ts", Options{
		ExecutionMode: "persistent", LogicalServiceID: "example/chat/session", Generation: 3,
		CanonicalBasePath: "/example/chat/session", MinimumWorkers: 2, MaximumWorkers: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	if record.ExecutionMode != "persistent" || record.MinimumWorkers != 2 || len(record.WorkerIDs) != 2 || len(workersFake.starts) != 2 {
		t.Fatalf("persistent pool = %#v starts=%d", record, len(workersFake.starts))
	}
	scaled, err := manager.Scale(context.Background(), record.ServiceID, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(scaled.WorkerIDs) != 3 || len(workersFake.starts) != 3 {
		t.Fatalf("persistent Workers = %#v; starts=%d", scaled.WorkerIDs, len(workersFake.starts))
	}
	if _, err := manager.Dispatch(context.Background(), record.ServiceID, httptest.NewRequest(http.MethodGet, "http://service/", nil)); err != nil {
		t.Fatalf("persistent HTTP dispatch: %v", err)
	}
	if err := manager.ProxyWebSocket(context.Background(), record.ServiceID, httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://service/connect", nil)); err != nil {
		t.Fatal(err)
	}
	if len(workersFake.websocketCalls) != 1 || workersFake.websocketCalls[0] != "group:chat" {
		t.Fatalf("WebSocket calls = %#v", workersFake.websocketCalls)
	}
}

type fakeRouter struct {
	handlers map[string]http.Handler
	removed  []string
}

func (f *fakeRouter) RegisterRoute(prefix string, handler http.Handler) error {
	if f.handlers == nil {
		f.handlers = map[string]http.Handler{}
	}
	if _, ok := f.handlers[prefix]; ok {
		return context.Canceled
	}
	f.handlers[prefix] = handler
	return nil
}
func (f *fakeRouter) UnregisterRoute(prefix string) {
	delete(f.handlers, prefix)
	f.removed = append(f.removed, prefix)
}

type fakePorts struct {
	lease   ports.Lease
	handler http.Handler
	closed  []string
}

func (f *fakePorts) ExposeHTTP(_ context.Context, request ports.Request, handler http.Handler) (ports.Lease, error) {
	f.handler = handler
	f.lease = ports.Lease{LeaseID: "port-lease", SandboxID: request.SandboxID, SandboxIP: request.SandboxIP, HostPort: 18080}
	return f.lease, nil
}
func (f *fakePorts) AttachHTTP(_ context.Context, leaseID string, handler http.Handler) (ports.Lease, error) {
	if f.lease.LeaseID != leaseID {
		return ports.Lease{}, context.Canceled
	}
	f.handler = handler
	return f.lease, nil
}
func (f *fakePorts) Close(id string) error { f.closed = append(f.closed, id); return nil }

func TestServicePoolScaleStreamingExposureAndStop(t *testing.T) {
	store, _ := records.New(t.TempDir())
	coordinatorFake, workersFake, routerFake, portsFake := &fakeCoordinator{}, &fakeWorkers{}, &fakeRouter{}, &fakePorts{}
	manager, err := New(coordinatorFake, workersFake, store, routerFake, portsFake, Policy{Strategy: model.GroupingOwner, MinimumWorkers: 2, MaximumWorkers: 4, MaximumInFlight: 3})
	if err != nil {
		t.Fatal(err)
	}
	record, err := manager.Start(context.Background(), "api", "file:///programs/api.ts", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if record.State != "READY" || record.SandboxIP != "10.88.0.2" || len(record.WorkerIDs) != 2 || len(workersFake.configurations) != 1 || workersFake.configurations[0].maximum != 3 {
		t.Fatalf("record=%#v configurations=%#v", record, workersFake.configurations)
	}
	scaled, err := manager.Scale(context.Background(), "api", 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(scaled.WorkerIDs) != 4 {
		t.Fatalf("scaled=%#v", scaled)
	}
	workersFake.lifecycle = nil
	scaled, err = manager.Scale(context.Background(), "api", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(scaled.WorkerIDs) != 1 || len(workersFake.stops) != 3 {
		t.Fatalf("scaled=%#v stops=%#v", scaled, workersFake.stops)
	}
	if len(workersFake.lifecycle) != 4 || workersFake.lifecycle[3] != "configure:"+scaled.WorkerIDs[0] {
		t.Fatalf("scale-down did not stop idle Workers before publishing the smaller pool: %#v", workersFake.lifecycle)
	}
	if _, err := manager.Scale(context.Background(), "api", 5); err == nil {
		t.Fatal("scale above maximum accepted")
	}
	exposed, err := manager.Expose(context.Background(), "api", ExposeOptions{PathPrefix: "/api/", AutomaticHostPort: true})
	if err != nil {
		t.Fatal(err)
	}
	if exposed.PathPrefix != "/api" || exposed.PortLeaseID != "port-lease" || portsFake.handler == nil {
		t.Fatalf("exposed=%#v", exposed)
	}
	request := httptest.NewRequest(http.MethodPost, "http://the8020/api", strings.NewReader("body"))
	recorder := httptest.NewRecorder()
	routerFake.handlers["/api"].ServeHTTP(recorder, request)
	if recorder.Code != http.StatusAccepted || recorder.Header().Get("X-Service") != "test" || recorder.Body.String() != "reply:body" {
		t.Fatalf("response=%#v body=%q", recorder.Result(), recorder.Body.String())
	}
	if err := manager.ProxyWebSocket(context.Background(), "api", httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://service/events", nil)); err != nil {
		t.Fatal(err)
	}
	if len(workersFake.websocketCalls) != 1 || workersFake.websocketCalls[0] != "group:api" {
		t.Fatalf("request-service WebSocket calls = %#v", workersFake.websocketCalls)
	}
	if err := manager.Stop(context.Background(), "api"); err != nil {
		t.Fatal(err)
	}
	stopped, _ := manager.Inspect("api")
	if stopped.State != "STOPPED" || stopped.PathPrefix != "" || len(portsFake.closed) != 1 {
		t.Fatalf("stopped=%#v closed=%#v", stopped, portsFake.closed)
	}
}

func TestServiceScaleReplacesFailedWorkerAtUnchangedDesiredCount(t *testing.T) {
	store, _ := records.New(t.TempDir())
	workersFake := &fakeWorkers{states: map[string]string{}}
	manager, err := New(&fakeCoordinator{}, workersFake, store, &fakeRouter{}, nil, Policy{Strategy: model.GroupingOwner, MinimumWorkers: 1, MaximumWorkers: 2, MaximumInFlight: 1})
	if err != nil {
		t.Fatal(err)
	}
	record, err := manager.Start(context.Background(), "api", "file:///programs/api.ts", Options{})
	if err != nil {
		t.Fatal(err)
	}
	failedWorkerID := record.WorkerIDs[0]
	workersFake.states[failedWorkerID] = "failed"
	workersFake.lifecycle = nil

	repaired, err := manager.Scale(context.Background(), record.ServiceID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(repaired.WorkerIDs) != 1 || repaired.WorkerIDs[0] == failedWorkerID {
		t.Fatalf("failed Worker was not replaced: before=%q after=%#v", failedWorkerID, repaired.WorkerIDs)
	}
	if len(workersFake.stops) != 1 || workersFake.stops[0] != failedWorkerID {
		t.Fatalf("failed Worker was not reaped: %#v", workersFake.stops)
	}
	if len(workersFake.lifecycle) < 2 || workersFake.lifecycle[0] != "configure:" || workersFake.lifecycle[1] != "stop:"+failedWorkerID {
		t.Fatalf("failed Worker was not excluded before termination: %#v", workersFake.lifecycle)
	}
	persisted, err := manager.Inspect(record.ServiceID)
	if err != nil || len(persisted.WorkerIDs) != 1 || persisted.WorkerIDs[0] != repaired.WorkerIDs[0] {
		t.Fatalf("persisted=%#v repaired=%#v err=%v", persisted, repaired, err)
	}
}

func TestServiceStartValidatesLimitsAndDuplicateExposure(t *testing.T) {
	store, _ := records.New(t.TempDir())
	manager, _ := New(&fakeCoordinator{}, &fakeWorkers{}, store, &fakeRouter{}, &fakePorts{}, Policy{Strategy: model.GroupingOwner, MinimumWorkers: 1, MaximumWorkers: 2, MaximumInFlight: 1})
	if _, err := manager.Start(context.Background(), "api", "file:///api.ts", Options{MinimumWorkers: 3, MaximumWorkers: 2}); err == nil {
		t.Fatal("invalid limits accepted")
	}
	if _, err := manager.Start(context.Background(), "api", "file:///api.ts", Options{PathPrefix: "/api"}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Expose(context.Background(), "api", ExposeOptions{}); err == nil {
		t.Fatal("duplicate exposure accepted")
	}
}

func TestRuntimeGroupFailureMarksLiveServiceFailed(t *testing.T) {
	store, _ := records.New(t.TempDir())
	coordinatorFake := &fakeCoordinator{}
	workersFake := &fakeWorkers{}
	manager, _ := New(coordinatorFake, workersFake, store, &fakeRouter{}, &fakePorts{}, Policy{Strategy: model.GroupingOwner, MinimumWorkers: 1, MaximumWorkers: 2, MaximumInFlight: 1})
	record, err := manager.Start(context.Background(), "api", "file:///api.ts", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.FailGroup(record.RuntimeGroupID, "cgroup OOM"); err != nil {
		t.Fatal(err)
	}
	failed, _ := manager.Inspect(record.ServiceID)
	if failed.State != "FAILED" || failed.Failure != "cgroup OOM" || !failed.RuntimeUnavailable {
		t.Fatalf("failed=%#v", failed)
	}
	workersFake.listErr = errors.New("dead supervisor must not be queried")
	if err := manager.Stop(context.Background(), record.ServiceID); err != nil {
		t.Fatal(err)
	}
	stopped, err := manager.Inspect(record.ServiceID)
	if err != nil || stopped.State != "STOPPED" || len(stopped.WorkerIDs) != 0 || len(workersFake.stops) != 0 || len(coordinatorFake.releases) != 1 {
		t.Fatalf("stopped=%#v stops=%#v releases=%#v err=%v", stopped, workersFake.stops, coordinatorFake.releases, err)
	}
}

func TestServiceRestoreReattachesRouteAndDurableHTTPLease(t *testing.T) {
	store, err := records.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	record := Record{ServiceID: "api", LogicalServiceID: "example/api/main", Entrypoint: "file:///api.ts", RuntimeGroupID: "group", SandboxID: "sandbox", SandboxIP: "10.88.0.2", State: "READY", PathPrefix: "/api", PortLeaseID: "port-original", HostPort: 18080, ExecutionMode: "stateless"}
	if err := store.Save(record.ServiceID, record); err != nil {
		t.Fatal(err)
	}
	workersFake, routerFake := &fakeWorkers{}, &fakeRouter{}
	portsFake := &fakePorts{lease: ports.Lease{LeaseID: "port-original", HostPort: 18080, Protocol: "http"}}
	manager, err := New(&fakeCoordinator{}, workersFake, store, routerFake, portsFake, Policy{Strategy: model.GroupingOwner})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	if routerFake.handlers["/api"] == nil || portsFake.handler == nil || portsFake.lease.LeaseID != "port-original" {
		t.Fatalf("handlers=%#v port=%#v", routerFake.handlers, portsFake.lease)
	}
	request := httptest.NewRequest(http.MethodPost, "http://the8020/api", strings.NewReader("restored"))
	recorder := httptest.NewRecorder()
	portsFake.handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusAccepted || recorder.Body.String() != "reply:restored" {
		t.Fatalf("status=%d body=%q", recorder.Code, recorder.Body.String())
	}
}

func TestEphemeralServiceWakesRecyclesAndReturnsToZero(t *testing.T) {
	store, _ := records.New(t.TempDir())
	workersFake, routerFake := &fakeWorkers{}, &fakeRouter{}
	manager, err := New(&fakeCoordinator{}, workersFake, store, routerFake, nil, Policy{Strategy: model.GroupingOwner, MinimumWorkers: 1, MaximumWorkers: 2, MaximumInFlight: 1, WorkerIdleTimeout: 5 * time.Millisecond, RecycleRequests: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	record, err := manager.Start(context.Background(), "preview", "file:///programs/preview.ts", Options{Ephemeral: true, MaximumWorkers: 2, MaximumInFlight: 1, PathPrefix: "/preview", IdleTimeout: 5 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if len(record.WorkerIDs) != 0 {
		t.Fatalf("ephemeral service started Workers: %#v", record)
	}
	request := httptest.NewRequest(http.MethodPost, "http://the8020/preview", strings.NewReader("body"))
	response := httptest.NewRecorder()
	routerFake.handlers["/preview"].ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || response.Body.String() != "reply:body" {
		t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		current, inspectErr := manager.Inspect("preview")
		if inspectErr == nil && current.State == "IDLE" && len(current.WorkerIDs) == 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	current, _ := manager.Inspect("preview")
	if current.State != "IDLE" || len(current.WorkerIDs) != 0 || len(workersFake.starts) != 2 || len(workersFake.stops) != 2 {
		t.Fatalf("current=%#v starts=%d stops=%#v", current, len(workersFake.starts), workersFake.stops)
	}
}

func TestServiceScalesBeforeDispatchWhenEveryWorkerIsSaturated(t *testing.T) {
	store, _ := records.New(t.TempDir())
	workersFake, routerFake := &fakeWorkers{inFlight: map[string]int{}}, &fakeRouter{}
	manager, _ := New(&fakeCoordinator{}, workersFake, store, routerFake, nil, Policy{Strategy: model.GroupingOwner, MinimumWorkers: 1, MaximumWorkers: 2, MaximumInFlight: 1})
	record, err := manager.Start(context.Background(), "api", "file:///programs/api.ts", Options{PathPrefix: "/api"})
	if err != nil {
		t.Fatal(err)
	}
	workersFake.inFlight[record.WorkerIDs[0]] = 1
	request := httptest.NewRequest(http.MethodGet, "http://the8020/api", nil)
	response := httptest.NewRecorder()
	routerFake.handlers["/api"].ServeHTTP(response, request)
	current, _ := manager.Inspect("api")
	if response.Code != http.StatusAccepted || len(current.WorkerIDs) != 2 || len(workersFake.starts) != 2 {
		t.Fatalf("status=%d current=%#v starts=%d", response.Code, current, len(workersFake.starts))
	}
}

func TestServiceDispatchRepairsFailedWorkerBeforeForwardingRequest(t *testing.T) {
	store, _ := records.New(t.TempDir())
	workersFake, routerFake := &fakeWorkers{states: map[string]string{}}, &fakeRouter{}
	manager, err := New(&fakeCoordinator{}, workersFake, store, routerFake, nil, Policy{Strategy: model.GroupingOwner, MinimumWorkers: 1, MaximumWorkers: 1, MaximumInFlight: 1})
	if err != nil {
		t.Fatal(err)
	}
	record, err := manager.Start(context.Background(), "api", "file:///programs/api.ts", Options{PathPrefix: "/api"})
	if err != nil {
		t.Fatal(err)
	}
	failedWorkerID := record.WorkerIDs[0]
	workersFake.states[failedWorkerID] = "failed"

	response := httptest.NewRecorder()
	routerFake.handlers["/api"].ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://the8020/api", nil))
	if response.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
	}
	repaired, inspectErr := manager.Inspect(record.ServiceID)
	if inspectErr != nil || len(repaired.WorkerIDs) != 1 || repaired.WorkerIDs[0] == failedWorkerID {
		t.Fatalf("repaired=%#v err=%v", repaired, inspectErr)
	}
	if len(workersFake.starts) != 2 || len(workersFake.stops) != 1 || workersFake.stops[0] != failedWorkerID {
		t.Fatalf("starts=%d stops=%#v", len(workersFake.starts), workersFake.stops)
	}
}
