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
	"slices"
	"strings"
	"testing"
	"time"

	"the8020/kernel/execution/coordinator"
	"the8020/kernel/execution/records"
	"the8020/kernel/execution/supervisor"
	"the8020/kernel/execution/workers"
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
	idleSinceMS    map[string]int64
	states         map[string]string
	lifecycle      []string
	listErr        error
	stopErrors     map[string]error
}

func testOptions(minimum, maximum, concurrency int) Options {
	return Options{
		MinimumWorkers: minimum, MaximumWorkers: maximum,
		ConcurrencyPerWorker: concurrency, WorkerKeepAlive: time.Minute,
		ExecutionMode: "stateless", TargetUtilization: 0.7,
		PlacementWorkers: minimum,
	}
}

func testRecord(serviceID string) Record {
	return Record{
		ServiceID: serviceID, LogicalServiceID: "example/api/" + serviceID,
		Entrypoint: "file:///" + serviceID + ".ts", ReleaseID: "current", State: "IDLE",
		MaximumWorkers: 2, ConcurrencyPerWorker: 1, WorkerKeepAlive: time.Minute,
		ExecutionMode: "stateless", TargetUtilization: 0.7,
	}
}

func TestServiceRestoreIsolatesCorruptRecords(t *testing.T) {
	root := t.TempDir()
	store, err := records.New(root)
	if err != nil {
		t.Fatal(err)
	}
	healthy := testRecord("healthy")
	if err := store.Save(healthy.ServiceID, healthy); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "c-corrupt.json"), []byte(`{"service_id":"c-corrupt","unknown":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := New(&fakeCoordinator{}, &fakeWorkers{}, store, Policy{Strategy: model.GroupingOwner})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	if restored, err := manager.Inspect(healthy.ServiceID); err != nil || restored.State != "IDLE" {
		t.Fatalf("restored=%#v err=%v", restored, err)
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
			result = append(result, workers.Record{RuntimeGroupID: group, Worker: supervisor.WorkerStatus{WorkerID: request.Metadata.WorkerID, WorkloadID: request.Metadata.WorkloadID, InFlight: f.inFlight[request.Metadata.WorkerID], IdleSinceMS: f.idleSinceMS[request.Metadata.WorkerID], State: state}})
		}
	}
	return result, nil
}

func TestServiceRestoreMarksAStaleRuntimeRecordFailedSoItCanRestart(t *testing.T) {
	store, err := records.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	record := testRecord("api")
	record.RuntimeGroupID, record.SandboxID, record.WorkerIDs, record.State = "missing-group", "missing-sandbox", []string{"missing-worker"}, "READY"
	if err := store.Save(record.ServiceID, record); err != nil {
		t.Fatal(err)
	}
	workersFake := &fakeWorkers{listErr: context.Canceled}
	manager, err := New(&fakeCoordinator{}, workersFake, store, Policy{Strategy: model.GroupingOwner})
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
	record := testRecord("api")
	record.RuntimeGroupID, record.SandboxID, record.WorkerIDs, record.State = "group", "sandbox", []string{workerID}, "READY"
	if err := store.Save(record.ServiceID, record); err != nil {
		t.Fatal(err)
	}
	workersFake := &fakeWorkers{
		starts: []supervisor.StartWorkerRequest{{Metadata: supervisor.ExecutionMetadata{WorkerID: workerID, WorkloadID: record.ServiceID}}},
		states: map[string]string{workerID: "failed"},
	}
	manager, err := New(&fakeCoordinator{}, workersFake, store, Policy{Strategy: model.GroupingOwner})
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
	manager, err := New(&fakeCoordinator{}, workersFake, store, Policy{Strategy: model.GroupingOwner})
	if err != nil {
		t.Fatal(err)
	}
	record, err := manager.Start(context.Background(), "api", "file:///programs/api.ts", testOptions(2, 2, 1))
	if err != nil {
		t.Fatal(err)
	}
	workersFake.stopErrors[record.WorkerIDs[1]] = errors.New("temporary stop failure")
	workersFake.lifecycle = nil
	if _, err := manager.Stop(context.Background(), record.ServiceID); err == nil {
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
	if stopped, err := manager.Stop(context.Background(), record.ServiceID); err != nil || !stopped {
		t.Fatal(err)
	}
	stopped, err := manager.Inspect(record.ServiceID)
	if err != nil || stopped.State != "STOPPED" || len(stopped.WorkerIDs) != 0 {
		t.Fatalf("stopped=%#v err=%v", stopped, err)
	}
}

func TestServiceStopDrainsOccupiedWorkersWithoutError(t *testing.T) {
	store, _ := records.New(t.TempDir())
	workersFake := &fakeWorkers{inFlight: map[string]int{}}
	manager, err := New(&fakeCoordinator{}, workersFake, store, Policy{Strategy: model.GroupingOwner})
	if err != nil {
		t.Fatal(err)
	}
	record, err := manager.Start(context.Background(), "api", "file:///programs/api.ts", testOptions(1, 1, 1))
	if err != nil {
		t.Fatal(err)
	}
	workerID := record.WorkerIDs[0]
	workersFake.inFlight[workerID] = 1
	workersFake.lifecycle = nil

	stopped, err := manager.Stop(context.Background(), record.ServiceID)
	if err != nil || stopped {
		t.Fatalf("stopped=%t err=%v", stopped, err)
	}
	draining, err := manager.Inspect(record.ServiceID)
	if err != nil || draining.State != "DRAINING" || draining.Failure != "" || len(draining.WorkerIDs) != 1 || len(workersFake.stops) != 0 {
		t.Fatalf("draining=%#v stops=%#v err=%v", draining, workersFake.stops, err)
	}
	if len(workersFake.lifecycle) == 0 || workersFake.lifecycle[0] != "configure:" {
		t.Fatalf("pool was not excluded before drain: %#v", workersFake.lifecycle)
	}

	workersFake.inFlight[workerID] = 0
	if stopped, err = manager.Stop(context.Background(), record.ServiceID); err != nil || !stopped {
		t.Fatalf("stopped=%t err=%v", stopped, err)
	}
	if len(workersFake.stops) != 1 || workersFake.stops[0] != workerID {
		t.Fatalf("stops=%#v", workersFake.stops)
	}
}

func TestServiceStopRetiresMissingRuntimeGroupAndRecord(t *testing.T) {
	store, _ := records.New(t.TempDir())
	workersFake := &fakeWorkers{listErr: os.ErrNotExist}
	coordinatorFake := &fakeCoordinator{}
	manager, err := New(coordinatorFake, workersFake, store, Policy{Strategy: model.GroupingOwner})
	if err != nil {
		t.Fatal(err)
	}
	record := testRecord("stale-pool")
	record.LogicalServiceID, record.RuntimeGroupID, record.SandboxID = "core/example/service", "missing-group", "missing-sandbox"
	record.WorkerIDs, record.State = []string{"missing-worker"}, "FAILED"
	if err := store.Save(record.ServiceID, record); err != nil {
		t.Fatal(err)
	}
	if stopped, err := manager.Stop(context.Background(), record.ServiceID); err != nil || !stopped {
		t.Fatal(err)
	}
	stopped, err := manager.Inspect(record.ServiceID)
	if err != nil || stopped.State != "STOPPED" || len(stopped.WorkerIDs) != 0 {
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
	manager, err := New(&fakeCoordinator{ensureErr: errors.New("shared-group label update failed")}, &fakeWorkers{}, store, Policy{Strategy: model.GroupingOwner})
	if err != nil {
		t.Fatal(err)
	}
	failed, err := manager.Start(context.Background(), "unassigned-pool", "file:///programs/api.ts", testOptions(0, 1, 1))
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
	manager, err := New(coordinatorFake, workersFake, store, Policy{Strategy: model.GroupingOwner})
	if err != nil {
		t.Fatal(err)
	}
	failed, err := manager.Start(context.Background(), "rejected-pool", "file:///programs/api.ts", testOptions(1, 1, 1))
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
	record := testRecord("stale-pool")
	record.LogicalServiceID, record.RuntimeGroupID, record.SandboxID = "core/example/service", "missing-group", "missing-sandbox"
	record.WorkerIDs, record.State = []string{"missing-worker"}, "FAILED"
	if err := store.Save(record.ServiceID, record); err != nil {
		t.Fatal(err)
	}
	coordinatorFake := &fakeCoordinator{releaseErr: fmt.Errorf("sandbox missing: %w", os.ErrNotExist)}
	manager, err := New(coordinatorFake, &fakeWorkers{listErr: os.ErrNotExist}, store, Policy{Strategy: model.GroupingOwner})
	if err != nil {
		t.Fatal(err)
	}
	if stopped, err := manager.Stop(context.Background(), record.ServiceID); err != nil || !stopped {
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
	manager, err := New(&fakeCoordinator{releaseErr: errors.New("must not release group")}, workersFake, store, Policy{Strategy: model.GroupingOwner})
	if err != nil {
		t.Fatal(err)
	}
	record := testRecord("failed-pool")
	record.LogicalServiceID, record.RuntimeGroupID, record.SandboxID = "core/example/service", "absent-group", "absent-sandbox"
	record.WorkerIDs, record.State = []string{"absent-worker"}, "FAILED"
	if err := store.Save(record.ServiceID, record); err != nil {
		t.Fatal(err)
	}
	if err := manager.RetireUnavailable(record.ServiceID, "sandbox absent during startup"); err != nil {
		t.Fatal(err)
	}
	retired, err := manager.Inspect(record.ServiceID)
	if err != nil || retired.State != "STOPPED" || len(retired.WorkerIDs) != 0 || !strings.Contains(retired.Failure, "sandbox absent") {
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
	manager, err := New(&fakeCoordinator{}, workersFake, store, Policy{Strategy: model.GroupingOwner})
	if err != nil {
		t.Fatal(err)
	}
	record, err := manager.Start(context.Background(), "chat", "file:///programs/chat.ts", Options{
		ExecutionMode: "persistent", LogicalServiceID: "example/chat/session", Generation: 3,
		CanonicalBasePath: "/example/chat/session", MinimumWorkers: 2, MaximumWorkers: 8,
		ConcurrencyPerWorker: 4, WorkerKeepAlive: time.Minute, TargetUtilization: 0.7, PlacementWorkers: 2,
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

func TestServicePoolScaleStreamingDispatchAndStop(t *testing.T) {
	store, _ := records.New(t.TempDir())
	coordinatorFake, workersFake := &fakeCoordinator{}, &fakeWorkers{}
	manager, err := New(coordinatorFake, workersFake, store, Policy{Strategy: model.GroupingOwner})
	if err != nil {
		t.Fatal(err)
	}
	record, err := manager.Start(context.Background(), "api", "file:///programs/api.ts", testOptions(2, 4, 3))
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
	if len(workersFake.lifecycle) != 4 || workersFake.lifecycle[0] != "configure:"+scaled.WorkerIDs[0] {
		t.Fatalf("scale-down did not exclude idle Workers before stopping them: %#v", workersFake.lifecycle)
	}
	if _, err := manager.Scale(context.Background(), "api", 5); err == nil {
		t.Fatal("scale above maximum accepted")
	}
	request := httptest.NewRequest(http.MethodPost, "http://the8020/api", strings.NewReader("body"))
	response, err := manager.Dispatch(context.Background(), "api", request)
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if readErr != nil || response.StatusCode != http.StatusAccepted || response.Header.Get("X-Service") != "test" || string(body) != "reply:body" {
		t.Fatalf("response=%#v body=%q err=%v", response, body, readErr)
	}
	if err := manager.ProxyWebSocket(context.Background(), "api", httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://service/events", nil)); err != nil {
		t.Fatal(err)
	}
	if len(workersFake.websocketCalls) != 1 || workersFake.websocketCalls[0] != "group:api" {
		t.Fatalf("request-service WebSocket calls = %#v", workersFake.websocketCalls)
	}
	if stopped, err := manager.Stop(context.Background(), "api"); err != nil || !stopped {
		t.Fatal(err)
	}
	stopped, _ := manager.Inspect("api")
	if stopped.State != "STOPPED" {
		t.Fatalf("stopped=%#v", stopped)
	}
}

func TestServiceScaleReplacesFailedWorkerAtUnchangedDesiredCount(t *testing.T) {
	store, _ := records.New(t.TempDir())
	workersFake := &fakeWorkers{states: map[string]string{}}
	manager, err := New(&fakeCoordinator{}, workersFake, store, Policy{Strategy: model.GroupingOwner})
	if err != nil {
		t.Fatal(err)
	}
	record, err := manager.Start(context.Background(), "api", "file:///programs/api.ts", testOptions(1, 2, 1))
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

func TestServiceStartRequiresCanonicalScalingPolicy(t *testing.T) {
	store, _ := records.New(t.TempDir())
	manager, _ := New(&fakeCoordinator{}, &fakeWorkers{}, store, Policy{Strategy: model.GroupingOwner})
	for name, options := range map[string]Options{
		"bounds":      {MinimumWorkers: 3, MaximumWorkers: 2, ConcurrencyPerWorker: 1, WorkerKeepAlive: time.Minute, ExecutionMode: "stateless", TargetUtilization: 0.7, PlacementWorkers: 3},
		"concurrency": {MaximumWorkers: 2, WorkerKeepAlive: time.Minute, ExecutionMode: "stateless", TargetUtilization: 0.7},
		"keepalive":   {MaximumWorkers: 2, ConcurrencyPerWorker: 1, ExecutionMode: "stateless", TargetUtilization: 0.7},
		"utilization": {MaximumWorkers: 2, ConcurrencyPerWorker: 1, WorkerKeepAlive: time.Minute, ExecutionMode: "stateless"},
		"placement":   {MaximumWorkers: 2, ConcurrencyPerWorker: 1, WorkerKeepAlive: time.Minute, ExecutionMode: "stateless", TargetUtilization: 0.7, PlacementWorkers: 3},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := manager.Start(context.Background(), name, "file:///api.ts", options); err == nil {
				t.Fatal("invalid policy accepted")
			}
		})
	}
	record, err := manager.Start(context.Background(), "api", "file:///api.ts", testOptions(0, 2, 1))
	if err != nil || record.State != "IDLE" {
		t.Fatalf("record=%#v err=%v", record, err)
	}
}

func TestRuntimeGroupFailureMarksLiveServiceFailed(t *testing.T) {
	store, _ := records.New(t.TempDir())
	coordinatorFake := &fakeCoordinator{}
	workersFake := &fakeWorkers{}
	manager, _ := New(coordinatorFake, workersFake, store, Policy{Strategy: model.GroupingOwner})
	record, err := manager.Start(context.Background(), "api", "file:///api.ts", testOptions(1, 2, 1))
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
	if stopped, err := manager.Stop(context.Background(), record.ServiceID); err != nil || !stopped {
		t.Fatal(err)
	}
	stopped, err := manager.Inspect(record.ServiceID)
	if err != nil || stopped.State != "STOPPED" || len(stopped.WorkerIDs) != 0 || len(workersFake.stops) != 0 || len(coordinatorFake.releases) != 1 {
		t.Fatalf("stopped=%#v stops=%#v releases=%#v err=%v", stopped, workersFake.stops, coordinatorFake.releases, err)
	}
}

func TestTerminalRuntimeIsClassifiedAndReplaced(t *testing.T) {
	store, _ := records.New(t.TempDir())
	coordinatorFake := &fakeCoordinator{}
	workersFake := &fakeWorkers{}
	manager, _ := New(coordinatorFake, workersFake, store, Policy{Strategy: model.GroupingOwner})
	options := testOptions(1, 2, 1)
	record, err := manager.Start(context.Background(), "api", "file:///programs/api.ts", options)
	if err != nil {
		t.Fatal(err)
	}

	workersFake.listErr = fmt.Errorf("stopped sandbox: %w", workers.ErrRuntimeUnavailable)
	if _, err := manager.ReconcileCapacity(context.Background(), record.ServiceID, 1); !errors.Is(err, workers.ErrRuntimeUnavailable) {
		t.Fatalf("reconcile error=%v", err)
	}
	failed, err := manager.Inspect(record.ServiceID)
	if err != nil || failed.State != "FAILED" || !failed.RuntimeUnavailable {
		t.Fatalf("failed=%#v err=%v", failed, err)
	}

	workersFake.listErr = nil
	restarted, err := manager.Start(context.Background(), "api", "file:///programs/api.ts", options)
	if err != nil {
		t.Fatal(err)
	}
	if restarted.State != "READY" || restarted.RuntimeUnavailable || len(restarted.WorkerIDs) != 1 {
		t.Fatalf("restarted=%#v", restarted)
	}
	if len(coordinatorFake.releases) != 1 || len(coordinatorFake.requests) != 2 {
		t.Fatalf("releases=%#v ensures=%#v", coordinatorFake.releases, coordinatorFake.requests)
	}
}

func TestScaleToZeroServiceWakesAndReturnsToZero(t *testing.T) {
	store, _ := records.New(t.TempDir())
	workersFake := &fakeWorkers{idleSinceMS: map[string]int64{}}
	manager, err := New(&fakeCoordinator{}, workersFake, store, Policy{Strategy: model.GroupingOwner})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	manager.now = func() time.Time { return now }
	options := testOptions(0, 2, 1)
	options.WorkerKeepAlive = 5 * time.Millisecond
	record, err := manager.Start(context.Background(), "preview", "file:///programs/preview.ts", options)
	if err != nil {
		t.Fatal(err)
	}
	if len(record.WorkerIDs) != 0 {
		t.Fatalf("ephemeral service started Workers: %#v", record)
	}
	response, err := manager.Dispatch(context.Background(), "preview", httptest.NewRequest(http.MethodPost, "http://the8020/preview", strings.NewReader("body")))
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if readErr != nil || response.StatusCode != http.StatusAccepted || string(body) != "reply:body" {
		t.Fatalf("status=%d body=%q err=%v", response.StatusCode, body, readErr)
	}
	current, err := manager.Inspect("preview")
	if err != nil || len(current.WorkerIDs) != 1 {
		t.Fatalf("awake service=%#v err=%v", current, err)
	}
	workersFake.idleSinceMS[current.WorkerIDs[0]] = now.Add(-5 * time.Millisecond).UnixMilli()
	if _, err := manager.ReconcileCapacity(context.Background(), "preview", 0); err != nil {
		t.Fatal(err)
	}
	current, _ = manager.Inspect("preview")
	if current.State != "IDLE" || len(current.WorkerIDs) != 0 || len(workersFake.starts) != 1 || len(workersFake.stops) != 1 {
		t.Fatalf("current=%#v starts=%d stops=%#v", current, len(workersFake.starts), workersFake.stops)
	}
}

func TestWorkerKeepAliveRemovesOnlyExpiredExcessWorkersAndKeepsMinimum(t *testing.T) {
	store, _ := records.New(t.TempDir())
	workersFake := &fakeWorkers{idleSinceMS: map[string]int64{}}
	manager, err := New(&fakeCoordinator{}, workersFake, store, Policy{Strategy: model.GroupingOwner})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.February, 3, 4, 5, 6, 0, time.UTC)
	manager.now = func() time.Time { return now }
	options := testOptions(1, 3, 1)
	options.WorkerKeepAlive = 2 * time.Minute
	record, err := manager.Start(context.Background(), "api", "file:///programs/api.ts", options)
	if err != nil {
		t.Fatal(err)
	}
	record, err = manager.Scale(context.Background(), record.ServiceID, 3)
	if err != nil {
		t.Fatal(err)
	}
	workersFake.idleSinceMS[record.WorkerIDs[0]] = now.Add(-3 * time.Minute).UnixMilli()
	workersFake.idleSinceMS[record.WorkerIDs[1]] = now.Add(-time.Minute).UnixMilli()
	workersFake.idleSinceMS[record.WorkerIDs[2]] = now.Add(-3 * time.Minute).UnixMilli()

	reconciled, err := manager.ReconcileCapacity(context.Background(), record.ServiceID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(reconciled.WorkerIDs) != 1 || reconciled.WorkerIDs[0] != record.WorkerIDs[1] {
		t.Fatalf("reconciled Workers=%#v, want only recently idle Worker %s", reconciled.WorkerIDs, record.WorkerIDs[1])
	}
	now = now.Add(2 * time.Minute)
	reconciled, err = manager.ReconcileCapacity(context.Background(), record.ServiceID, 1)
	if err != nil || len(reconciled.WorkerIDs) != 1 {
		t.Fatalf("minimum Worker was removed: record=%#v err=%v", reconciled, err)
	}
}

func TestWorkerScaleDownRetainsTargetHeadroomUnderLoad(t *testing.T) {
	store, _ := records.New(t.TempDir())
	workersFake := &fakeWorkers{inFlight: map[string]int{}, idleSinceMS: map[string]int64{}}
	manager, err := New(&fakeCoordinator{}, workersFake, store, Policy{Strategy: model.GroupingOwner})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.February, 3, 4, 5, 6, 0, time.UTC)
	manager.now = func() time.Time { return now }
	record, err := manager.Start(context.Background(), "api", "file:///programs/api.ts", testOptions(0, 3, 1))
	if err != nil {
		t.Fatal(err)
	}
	record, err = manager.Scale(context.Background(), record.ServiceID, 3)
	if err != nil {
		t.Fatal(err)
	}
	workersFake.inFlight[record.WorkerIDs[0]] = 1
	workersFake.idleSinceMS[record.WorkerIDs[1]] = now.Add(-2 * time.Minute).UnixMilli()
	workersFake.idleSinceMS[record.WorkerIDs[2]] = now.Add(-2 * time.Minute).UnixMilli()

	reconciled, err := manager.ReconcileCapacity(context.Background(), record.ServiceID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(reconciled.WorkerIDs) != 2 || !slices.Contains(reconciled.WorkerIDs, record.WorkerIDs[0]) {
		t.Fatalf("Workers=%#v, want busy Worker plus target headroom", reconciled.WorkerIDs)
	}
}

func TestServiceScalesBeforeDispatchWhenEveryWorkerIsSaturated(t *testing.T) {
	store, _ := records.New(t.TempDir())
	workersFake := &fakeWorkers{inFlight: map[string]int{}}
	manager, _ := New(&fakeCoordinator{}, workersFake, store, Policy{Strategy: model.GroupingOwner})
	record, err := manager.Start(context.Background(), "api", "file:///programs/api.ts", testOptions(1, 2, 1))
	if err != nil {
		t.Fatal(err)
	}
	workersFake.inFlight[record.WorkerIDs[0]] = 1
	response, err := manager.Dispatch(context.Background(), "api", httptest.NewRequest(http.MethodGet, "http://the8020/api", nil))
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	current, _ := manager.Inspect("api")
	if response.StatusCode != http.StatusAccepted || len(current.WorkerIDs) != 2 || len(workersFake.starts) != 2 {
		t.Fatalf("status=%d current=%#v starts=%d", response.StatusCode, current, len(workersFake.starts))
	}
}

func TestTargetUtilizationUsesPerWorkerConcurrencyToAddCapacity(t *testing.T) {
	store, _ := records.New(t.TempDir())
	workersFake := &fakeWorkers{inFlight: map[string]int{}}
	manager, _ := New(&fakeCoordinator{}, workersFake, store, Policy{Strategy: model.GroupingOwner})
	options := testOptions(1, 3, 10)
	options.TargetUtilization = 0.5
	record, err := manager.Start(context.Background(), "api", "file:///programs/api.ts", options)
	if err != nil {
		t.Fatal(err)
	}
	workersFake.inFlight[record.WorkerIDs[0]] = 4
	record, err = manager.EnsureCapacity(context.Background(), record.ServiceID, 3, 0)
	if err != nil || len(record.WorkerIDs) != 1 {
		t.Fatalf("below target record=%#v err=%v", record, err)
	}
	workersFake.inFlight[record.WorkerIDs[0]] = 5
	record, err = manager.EnsureCapacity(context.Background(), record.ServiceID, 3, 0)
	if err != nil || len(record.WorkerIDs) != 2 {
		t.Fatalf("above target record=%#v err=%v", record, err)
	}
}

func TestTargetUtilizationIncludesKernelReservedRequests(t *testing.T) {
	store, _ := records.New(t.TempDir())
	workersFake := &fakeWorkers{inFlight: map[string]int{}}
	manager, _ := New(&fakeCoordinator{}, workersFake, store, Policy{Strategy: model.GroupingOwner})
	options := testOptions(1, 3, 10)
	options.TargetUtilization = 0.5
	record, err := manager.Start(context.Background(), "api", "file:///programs/api.ts", options)
	if err != nil {
		t.Fatal(err)
	}
	record, err = manager.EnsureCapacity(context.Background(), record.ServiceID, 3, 5)
	if err != nil || len(record.WorkerIDs) != 2 {
		t.Fatalf("reserved-demand record=%#v err=%v", record, err)
	}
}

func TestTargetHeadroomFailurePreservesAvailableHardCapacity(t *testing.T) {
	store, _ := records.New(t.TempDir())
	workersFake := &fakeWorkers{inFlight: map[string]int{}}
	manager, _ := New(&fakeCoordinator{}, workersFake, store, Policy{Strategy: model.GroupingOwner})
	record, err := manager.Start(context.Background(), "api", "file:///programs/api.ts", testOptions(1, 2, 1))
	if err != nil {
		t.Fatal(err)
	}
	workersFake.startErr = errors.New("sandbox Worker capacity is exhausted")
	record, err = manager.EnsureCapacity(context.Background(), record.ServiceID, 2, 0)
	var capacity *SandboxCapacityError
	if !errors.As(err, &capacity) || capacity.Occupied != 0 || capacity.Slots != 1 || len(record.WorkerIDs) != 1 {
		t.Fatalf("record=%#v capacity=%#v err=%v", record, capacity, err)
	}
}

func TestServiceDispatchRepairsFailedWorkerBeforeForwardingRequest(t *testing.T) {
	store, _ := records.New(t.TempDir())
	workersFake := &fakeWorkers{states: map[string]string{}}
	manager, err := New(&fakeCoordinator{}, workersFake, store, Policy{Strategy: model.GroupingOwner})
	if err != nil {
		t.Fatal(err)
	}
	record, err := manager.Start(context.Background(), "api", "file:///programs/api.ts", testOptions(1, 1, 1))
	if err != nil {
		t.Fatal(err)
	}
	failedWorkerID := record.WorkerIDs[0]
	workersFake.states[failedWorkerID] = "failed"

	response, err := manager.Dispatch(context.Background(), "api", httptest.NewRequest(http.MethodGet, "http://the8020/api", nil))
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("status=%d", response.StatusCode)
	}
	repaired, inspectErr := manager.Inspect(record.ServiceID)
	if inspectErr != nil || len(repaired.WorkerIDs) != 1 || repaired.WorkerIDs[0] == failedWorkerID {
		t.Fatalf("repaired=%#v err=%v", repaired, inspectErr)
	}
	if len(workersFake.starts) != 2 || len(workersFake.stops) != 1 || workersFake.stops[0] != failedWorkerID {
		t.Fatalf("starts=%d stops=%#v", len(workersFake.starts), workersFake.stops)
	}
}
