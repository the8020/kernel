package webservices

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	platformauth "the8020/kernel/auth"
	executionservices "the8020/kernel/execution/services"
	workspacepackages "the8020/kernel/packages"
)

const persistentServiceManifest = `[execution]
mode = "persistent"
concurrency_per_worker = 1
keep_alive = "2m"
[access]
mode = "authenticated"
[access.unauthenticated]
action = "reject"
status = 401
message = "Authentication is required."
[scaling]
replicas_min = 2
replicas_max = 2
workers_per_replica_min = 1
workers_per_replica_max = 8
target_utilization = 0.7
[placement]
sandbox_group = "realtime"
`

type fakeRouter struct{ boundary http.Handler }

type fakeNodeRouter struct {
	local   string
	node    string
	calls   int
	indexes []int
}

func (r *fakeNodeRouter) LocalNodeID() string { return r.local }
func (r *fakeNodeRouter) LocalReplicaIndexes(limit int) []int {
	if r.indexes != nil {
		result := make([]int, 0, len(r.indexes))
		for _, index := range r.indexes {
			if index < limit {
				result = append(result, index)
			}
		}
		return result
	}
	result := make([]int, limit)
	for index := range result {
		result[index] = index
	}
	return result
}

func TestMinimumReplicasUseOnlyIndexesAssignedToLocalNode(t *testing.T) {
	root := t.TempDir()
	store := writeTestService(t, root, "the8020/demo/variables", `[scaling]
replicas_min = 4
replicas_max = 8
workers_per_replica_min = 1
workers_per_replica_max = 2
`)
	pools, router := newFakePools(), &fakeRouter{}
	manager := newTestManager(t, store, pools, router, filepath.Join(root, "node", "kernel", "services"))
	manager.nodes = &fakeNodeRouter{local: "node-b", indexes: []int{1, 3, 5, 7}}
	status, err := manager.Start(context.Background(), "the8020/demo/variables")
	if err != nil || status.State != StateReady || status.InstanceCount != 2 || status.Instances[0].Index != 1 || status.Instances[1].Index != 3 {
		t.Fatalf("status=%#v err=%v", status, err)
	}
}

func TestReconcileMovesMinimumReplicasWhenNodeAssignmentChanges(t *testing.T) {
	root := t.TempDir()
	store := writeTestService(t, root, "the8020/demo/variables", `[scaling]
replicas_min = 4
replicas_max = 4
workers_per_replica_min = 1
workers_per_replica_max = 2
`)
	pools, router := newFakePools(), &fakeRouter{}
	manager := newTestManager(t, store, pools, router, filepath.Join(root, "node", "kernel", "services"))
	nodes := &fakeNodeRouter{local: "node-a", indexes: []int{0, 2}}
	manager.nodes = nodes
	status, err := manager.Start(context.Background(), "the8020/demo/variables")
	if err != nil || len(status.Instances) != 2 || status.Instances[0].Index != 0 || status.Instances[1].Index != 2 {
		t.Fatalf("initial status=%#v err=%v", status, err)
	}
	nodes.indexes = []int{1, 3}
	if err := manager.ReconcileAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	status, err = manager.Inspect("the8020/demo/variables")
	if err != nil || len(status.Instances) != 2 || status.Instances[0].Index != 1 || status.Instances[1].Index != 3 {
		t.Fatalf("reassigned status=%#v err=%v", status, err)
	}
}

func (r *fakeNodeRouter) Proxy(node string, writer http.ResponseWriter, _ *http.Request) error {
	r.node, r.calls = node, r.calls+1
	writer.WriteHeader(http.StatusAccepted)
	return nil
}
func (r *fakeNodeRouter) ProxyAvailable(http.ResponseWriter, *http.Request) (bool, error) {
	return false, nil
}

func (r *fakeRouter) RegisterServiceBoundary(handler http.Handler) error {
	if r.boundary != nil {
		return errors.New("boundary already registered")
	}
	r.boundary = handler
	return nil
}

type fakeAuthentication struct {
	calls int
}

func (a *fakeAuthentication) CookieName() string { return "the8020_auth" }
func (a *fakeAuthentication) ValidateCookie(value string) (platformauth.AuthContext, error) {
	a.calls++
	if value != "valid-opaque-cookie" {
		return platformauth.AuthContext{}, platformauth.ErrUnauthenticated
	}
	return platformauth.AuthContext{Authenticated: true, Realm: platformauth.BootstrapRealm, UserID: "bootstrap-admin:Admin", Username: "Admin", AuthVersion: 7, SessionID: "kernel-only-session"}, nil
}

type dispatchedRequest struct {
	poolID string
	scheme string
	host   string
	method string
	path   string
	query  string
	body   string
	header http.Header
}

type fakePools struct {
	mu              sync.Mutex
	records         map[string]executionservices.Record
	options         map[string]executionservices.Options
	events          []string
	failGeneration  map[uint64]error
	failStart       map[string]error
	failStop        map[string]error
	dispatched      chan dispatchedRequest
	release         chan struct{}
	dispatchEntered chan struct{}
	responseStatus  int
	responseBody    string
	responseStream  io.ReadCloser
	workerStatus    map[string]int
	workerBody      map[string]string
	workerErrors    map[string]error
	capacityErrors  map[string]error
	occupiedSlots   map[string]int
	websockets      []dispatchedRequest
}

func newFakePools() *fakePools {
	return &fakePools{
		records:        map[string]executionservices.Record{},
		options:        map[string]executionservices.Options{},
		failGeneration: map[uint64]error{},
		failStart:      map[string]error{},
		failStop:       map[string]error{},
		responseStatus: http.StatusOK,
		responseBody:   "ok",
		workerStatus:   map[string]int{},
		workerBody:     map[string]string{},
		workerErrors:   map[string]error{},
		capacityErrors: map[string]error{},
		occupiedSlots:  map[string]int{},
	}
}

func (p *fakePools) List() ([]executionservices.Record, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	result := make([]executionservices.Record, 0, len(p.records))
	for _, record := range p.records {
		result = append(result, record)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ServiceID < result[j].ServiceID })
	return result, nil
}

func (p *fakePools) Start(_ context.Context, serviceID, entrypoint string, options executionservices.Options) (executionservices.Record, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, fmt.Sprintf("start:%s:%d", serviceID, options.Generation))
	if err := p.failStart[serviceID]; err != nil {
		return executionservices.Record{}, err
	}
	if err := p.failGeneration[options.Generation]; err != nil {
		return executionservices.Record{}, err
	}
	if existing, exists := p.records[serviceID]; exists && existing.State == "READY" {
		return executionservices.Record{}, fmt.Errorf("service %s is already started", serviceID)
	}
	workers := make([]string, options.MinimumWorkers)
	for index := range workers {
		workers[index] = fmt.Sprintf("%s-worker-%d", serviceID, index)
	}
	groupKey := options.GroupKey
	if groupKey == "" {
		groupKey = "owner:" + serviceID
	}
	record := executionservices.Record{
		ServiceID: serviceID, LogicalServiceID: options.LogicalServiceID,
		Entrypoint: entrypoint, RuntimeGroupID: "group:" + groupKey,
		SandboxID: "sandbox:" + serviceID, WorkerIDs: workers, State: "READY",
		ReleaseID: options.ReleaseID, Generation: options.Generation, MaximumWorkers: options.MaximumWorkers,
		MaximumInFlight: options.MaximumInFlight, ExecutionMode: options.ExecutionMode, Instance: options.Instance,
	}
	p.records[serviceID], p.options[serviceID] = record, options
	return record, nil
}

func (p *fakePools) Inspect(serviceID string) (executionservices.Record, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	record, exists := p.records[serviceID]
	if !exists {
		return record, os.ErrNotExist
	}
	return record, nil
}

func (p *fakePools) Scale(_ context.Context, serviceID string, count int) (executionservices.Record, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	record, exists := p.records[serviceID]
	if !exists {
		return record, os.ErrNotExist
	}
	p.events = append(p.events, fmt.Sprintf("scale:%s:%d", serviceID, count))
	record.WorkerIDs = make([]string, count)
	for index := range record.WorkerIDs {
		record.WorkerIDs[index] = fmt.Sprintf("%s-worker-%d", serviceID, index)
	}
	p.records[serviceID] = record
	return record, nil
}

func (p *fakePools) EnsureCapacity(_ context.Context, serviceID string) (executionservices.Record, error) {
	record, err := p.Inspect(serviceID)
	p.mu.Lock()
	capacityErr := p.capacityErrors[serviceID]
	p.mu.Unlock()
	return record, errors.Join(err, capacityErr)
}

func (p *fakePools) ReconcileCapacity(ctx context.Context, serviceID string) (executionservices.Record, error) {
	record, err := p.Inspect(serviceID)
	if err != nil {
		return record, err
	}
	record, err = p.Scale(ctx, serviceID, len(record.WorkerIDs))
	p.mu.Lock()
	record.OccupiedSlots = p.occupiedSlots[serviceID]
	p.mu.Unlock()
	record.CapacitySlots = len(record.WorkerIDs) * record.MaximumInFlight
	return record, err
}

func (p *fakePools) OpenAPI(_ context.Context, serviceID string) (map[string]any, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, exists := p.records[serviceID]; !exists {
		return nil, os.ErrNotExist
	}
	p.events = append(p.events, "openapi:"+serviceID)
	return map[string]any{"openapi": "3.1.0", "service": serviceID}, nil
}

func (p *fakePools) Dispatch(ctx context.Context, serviceID string, request *http.Request) (*http.Response, error) {
	p.mu.Lock()
	record, exists := p.records[serviceID]
	dispatched, release := p.dispatched, p.release
	entered := p.dispatchEntered
	status, body, responseStream := p.responseStatus, p.responseBody, p.responseStream
	p.mu.Unlock()
	if !exists || record.State != "READY" {
		return nil, errors.New("pool unavailable")
	}
	if entered != nil {
		entered <- struct{}{}
	}
	requestBody := ""
	if request.Body != nil {
		data, _ := io.ReadAll(request.Body)
		requestBody = string(data)
	}
	if dispatched != nil {
		dispatched <- dispatchedRequest{poolID: serviceID, scheme: request.URL.Scheme, host: request.URL.Host, method: request.Method, path: request.URL.Path, query: request.URL.RawQuery, body: requestBody, header: request.Header.Clone()}
	}
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if responseStream == nil {
		responseStream = io.NopCloser(strings.NewReader(body))
	}
	workerID := ""
	if len(record.WorkerIDs) > 0 {
		workerID = record.WorkerIDs[0]
	}
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"text/plain"}, "X-80-20-Internal-Selected-Worker-ID": []string{workerID}}, Body: responseStream}, nil
}

func (p *fakePools) DispatchWorker(ctx context.Context, serviceID, workerID string, request *http.Request) (*http.Response, error) {
	p.mu.Lock()
	workerErr := p.workerErrors[workerID]
	status, hasStatus := p.workerStatus[workerID]
	body := p.workerBody[workerID]
	p.mu.Unlock()
	if workerErr != nil {
		return nil, workerErr
	}
	if hasStatus {
		return &http.Response{
			StatusCode: status,
			Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	}
	request = request.Clone(ctx)
	request.Header.Set("X-80-20-Internal-Target-Worker-ID", workerID)
	return p.Dispatch(ctx, serviceID, request)
}

func (p *fakePools) ProxyWebSocket(_ context.Context, serviceID string, _ http.ResponseWriter, request *http.Request) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.websockets = append(p.websockets, dispatchedRequest{
		poolID: serviceID, path: request.URL.Path, query: request.URL.RawQuery, header: request.Header.Clone(),
	})
	return nil
}

func (p *fakePools) Stop(_ context.Context, serviceID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, "stop:"+serviceID)
	if err := p.failStop[serviceID]; err != nil {
		return err
	}
	record, exists := p.records[serviceID]
	if !exists {
		return os.ErrNotExist
	}
	record.State = "STOPPED"
	record.WorkerIDs = nil
	p.records[serviceID] = record
	return nil
}

func (p *fakePools) RemoveStopped(serviceID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	record, exists := p.records[serviceID]
	if !exists {
		return os.ErrNotExist
	}
	if record.State != "STOPPED" || len(record.WorkerIDs) != 0 {
		return errors.New("pool is not fully stopped")
	}
	p.events = append(p.events, "remove:"+serviceID)
	delete(p.records, serviceID)
	return nil
}

func TestReconcileRetriesPersistedStaleGenerationPoolCleanup(t *testing.T) {
	root := t.TempDir()
	store := writeTestService(t, root, "the8020/demo/variables", "")
	pools, router := newFakePools(), &fakeRouter{}
	staleID := "stale-generation-pool"
	pools.records[staleID] = executionservices.Record{
		ServiceID: staleID, LogicalServiceID: "the8020/demo/variables",
		ReleaseID: "service-generation-0", Generation: 0, State: "READY",
		WorkerIDs: []string{"stale-worker"},
	}
	validationID := "old-validation-pool"
	pools.records[validationID] = executionservices.Record{
		ServiceID: validationID, LogicalServiceID: "the8020/demo/variables",
		ReleaseID: "service-validation", Generation: 0, State: "STOPPED",
	}
	pools.failStop[staleID] = errors.New("temporary supervisor failure")
	manager := newTestManager(t, store, pools, router, filepath.Join(root, "node", "kernel", "node-a", "services"))

	status, err := manager.Start(context.Background(), "the8020/demo/variables")
	if err == nil || status.State != StateReady || !strings.Contains(err.Error(), "temporary supervisor failure") {
		t.Fatalf("start status=%#v err=%v", status, err)
	}
	if pools.records[staleID].State != "READY" {
		t.Fatalf("failed cleanup unexpectedly changed stale record: %#v", pools.records[staleID])
	}
	if _, exists := pools.records[validationID]; exists {
		t.Fatalf("stopped validation record was not removed: %#v", pools.records[validationID])
	}

	delete(pools.failStop, staleID)
	if err := manager.ReconcileAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, exists := pools.records[staleID]; exists {
		t.Fatalf("stale generation record was not removed on retry: %#v", pools.records[staleID])
	}
}

func TestBackgroundReconciliationLeavesEnabledServiceIdleUntilFirstRequest(t *testing.T) {
	root := t.TempDir()
	store := writeTestService(t, root, "the8020/demo/variables", "")
	if _, err := store.MutateState(context.Background(), "the8020/demo/variables", func(state *workspacepackages.DesiredServiceState) error {
		state.Enabled = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	pools, router := newFakePools(), &fakeRouter{}
	manager := newTestManager(t, store, pools, router, filepath.Join(root, "node", "kernel", "node-a", "services"))
	manager.StartReconciler(context.Background())
	deadline := time.Now().Add(time.Second)
	for {
		status, err := manager.Inspect("the8020/demo/variables")
		if err == nil && status.State == StateIdle {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("service did not enter lazy idle state: status=%#v err=%v", status, err)
		}
		time.Sleep(time.Millisecond)
	}
	pools.mu.Lock()
	eventsBeforeRequest := append([]string(nil), pools.events...)
	pools.mu.Unlock()
	for _, event := range eventsBeforeRequest {
		if strings.HasPrefix(event, "start:") {
			t.Fatalf("background reconciliation provisioned a sandbox: %#v", eventsBeforeRequest)
		}
	}
	response := httptest.NewRecorder()
	manager.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/the8020/demo/variables/value/6", nil))
	if response.Code != http.StatusOK || strings.TrimSpace(response.Body.String()) != "ok" {
		t.Fatalf("cold-start request status=%d body=%q", response.Code, response.Body.String())
	}
	status, err := manager.Inspect("the8020/demo/variables")
	if err != nil || status.State != StateReady || status.InstanceCount != 1 {
		t.Fatalf("cold-started status=%#v err=%v", status, err)
	}
}

func TestBackgroundMaintenanceDoesNotRediscoverPackageCatalog(t *testing.T) {
	root := t.TempDir()
	store := writeTestService(t, root, "the8020/demo/variables", "")
	pools, router := newFakePools(), &fakeRouter{}
	manager := newTestManager(t, store, pools, router, filepath.Join(root, "node", "kernel", "node-a", "services"))
	manager.StartReconciler(context.Background())
	deadline := time.Now().Add(time.Second)
	for {
		manager.mu.Lock()
		_, initialized := manager.services["the8020/demo/variables"]
		manager.mu.Unlock()
		if initialized {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("initial service discovery did not complete")
		}
		time.Sleep(time.Millisecond)
	}

	serviceRoot := filepath.Join(root, "packages", "the8020", "demo", "services", "static")
	writeTestFile(t, filepath.Join(serviceRoot, "service.toml"), "schema = 1\nentrypoint = \"service.ts\"\n")
	writeTestFile(t, filepath.Join(serviceRoot, "service.ts"), "export default {};\n")
	time.Sleep(50 * time.Millisecond)
	manager.mu.Lock()
	_, polled := manager.services["the8020/demo/static"]
	manager.mu.Unlock()
	if polled {
		t.Fatal("background maintenance rediscovered the package catalog")
	}
	if err := manager.ReconcileAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	_, explicitlyDiscovered := manager.services["the8020/demo/static"]
	manager.mu.Unlock()
	if !explicitlyDiscovered {
		t.Fatal("explicit reconciliation did not discover the new service")
	}
}

func TestIdenticalServiceFailuresAreRecordedOnce(t *testing.T) {
	var logs bytes.Buffer
	manager := &Manager{
		observed: filepath.Join(t.TempDir(), "observed"),
		logger:   slog.New(slog.NewJSONHandler(&logs, nil)),
		services: map[string]*runtimeService{},
	}
	if err := os.MkdirAll(manager.observed, 0o700); err != nil {
		t.Fatal(err)
	}
	cause := errors.New("package is unavailable")
	if _, err := manager.retainFailedGeneration("the8020/demo/static", 3, cause); !errors.Is(err, cause) {
		t.Fatal(err)
	}
	status, err := manager.retainFailedGeneration("the8020/demo/static", 3, cause)
	if !errors.Is(err, cause) {
		t.Fatal(err)
	}
	if status.Metrics.StartupFailures != 1 {
		t.Fatalf("identical failure count = %d", status.Metrics.StartupFailures)
	}
	if count := strings.Count(logs.String(), "service generation failed"); count != 1 {
		t.Fatalf("identical failure logs = %d: %s", count, logs.String())
	}
}

func TestRejectedServiceGenerationDoesNotEnterCapacityRetryLoop(t *testing.T) {
	root := t.TempDir()
	store := writeTestService(t, root, "the8020/demo/variables", "")
	pools, router := newFakePools(), &fakeRouter{}
	manager := newTestManager(t, store, pools, router, filepath.Join(root, "node", "kernel", "services"))
	rejection := fmt.Errorf("%w: type check failed", executionservices.ErrInvalidServiceDefinition)
	pools.failGeneration[1] = rejection

	status, err := manager.Start(context.Background(), "the8020/demo/variables")
	if !errors.Is(err, executionservices.ErrInvalidServiceDefinition) || status.State != StateFailed || status.FailedGeneration != 1 || status.Metrics.StartupFailures != 1 {
		t.Fatalf("status=%#v err=%v", status, err)
	}
	pools.mu.Lock()
	starts := len(pools.events)
	pools.mu.Unlock()
	if err := manager.reconcileMaintained(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertHTTPStatus(t, router.boundary, "/the8020/demo/variables/", http.StatusServiceUnavailable)
	pools.mu.Lock()
	afterMaintenanceAndRequest := len(pools.events)
	pools.mu.Unlock()
	if afterMaintenanceAndRequest != starts {
		t.Fatalf("rejected generation retried: before=%d after=%d", starts, afterMaintenanceAndRequest)
	}

	pools.failGeneration[2] = rejection
	if retried, err := manager.Restart(context.Background(), "the8020/demo/variables"); !errors.Is(err, executionservices.ErrInvalidServiceDefinition) || retried.FailedGeneration != 2 {
		t.Fatalf("explicit retry status=%#v err=%v", retried, err)
	}
	pools.mu.Lock()
	afterExplicitRestart := len(pools.events)
	pools.mu.Unlock()
	if afterExplicitRestart <= starts {
		t.Fatal("explicit service restart did not retry the rejected generation")
	}
}

func TestColdStartUsesDegradedCapacityAndFillsMissingReplicaInPlace(t *testing.T) {
	root := t.TempDir()
	store := writeTestService(t, root, "the8020/demo/variables", `[scaling]
replicas_min = 2
replicas_max = 2
workers_per_replica_min = 1
workers_per_replica_max = 1
`)
	if _, err := store.MutateState(context.Background(), "the8020/demo/variables", func(state *workspacepackages.DesiredServiceState) error {
		state.Enabled = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	definition, err := store.ReadService("the8020/demo/variables")
	if err != nil {
		t.Fatal(err)
	}
	firstPoolID := generationPoolID(definition.Identity.ServiceID(), definition.State.Generation, 0)
	missingPoolID := generationPoolID(definition.Identity.ServiceID(), definition.State.Generation, 1)
	pools := newFakePools()
	pools.failStart[missingPoolID] = errors.New("node CPU capacity exhausted")
	manager := newTestManager(t, store, pools, &fakeRouter{}, filepath.Join(root, "node", "kernel", "node-a", "services"))

	response := httptest.NewRecorder()
	manager.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/the8020/demo/variables/value/6", nil))
	if response.Code != http.StatusOK || strings.TrimSpace(response.Body.String()) != "ok" {
		t.Fatalf("degraded cold start status=%d body=%q", response.Code, response.Body.String())
	}
	degraded, err := manager.Inspect("the8020/demo/variables")
	if err != nil || degraded.State != StateDegraded || len(degraded.Instances) != 1 || degraded.Instances[0].PoolID != firstPoolID {
		t.Fatalf("degraded=%#v err=%v", degraded, err)
	}
	if err := manager.ReconcileAll(context.Background()); err == nil || !strings.Contains(err.Error(), "CPU capacity") {
		t.Fatalf("missing replica retry error=%v", err)
	}
	for _, event := range pools.events {
		if event == "stop:"+firstPoolID {
			t.Fatalf("healthy degraded replica was stopped during retry: %#v", pools.events)
		}
	}

	delete(pools.failStart, missingPoolID)
	if err := manager.ReconcileAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	ready, err := manager.Inspect("the8020/demo/variables")
	if err != nil || ready.State != StateReady || len(ready.Instances) != 2 || ready.Instances[0].PoolID != firstPoolID || ready.Instances[1].PoolID != missingPoolID {
		t.Fatalf("ready=%#v err=%v", ready, err)
	}
}

func TestReconcileChecksWorkerHealthWhenRecordedCountStillMatches(t *testing.T) {
	root := t.TempDir()
	store := writeTestService(t, root, "the8020/demo/variables", `[scaling]
workers_per_replica_min = 2
workers_per_replica_max = 2
`)
	pools := newFakePools()
	manager := newTestManager(t, store, pools, &fakeRouter{}, filepath.Join(root, "node", "kernel", "node-a", "services"))
	started, err := manager.Start(context.Background(), "the8020/demo/variables")
	if err != nil {
		t.Fatal(err)
	}
	poolID := started.Instances[0].PoolID
	pools.mu.Lock()
	record := pools.records[poolID]
	record.WorkerIDs = []string{"stale-worker-a", "stale-worker-b"}
	pools.records[poolID] = record
	pools.events = nil
	pools.mu.Unlock()

	if err := manager.ReconcileAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	status, err := manager.Inspect("the8020/demo/variables")
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Instances) != 1 || len(status.Instances[0].WorkerIDs) != 2 || status.Instances[0].WorkerIDs[0] == "stale-worker-a" {
		t.Fatalf("status=%#v", status)
	}
	pools.mu.Lock()
	events := append([]string(nil), pools.events...)
	pools.mu.Unlock()
	foundScale := false
	for _, event := range events {
		if event == "scale:"+poolID+":2" {
			foundScale = true
			break
		}
	}
	if !foundScale {
		t.Fatalf("same-count Worker reconciliation was skipped: %#v", events)
	}
}

func TestLifecyclePersistsGenerationsRollsCapacityAndRetainsBrokenReplacement(t *testing.T) {
	root := t.TempDir()
	store := writeTestService(t, root, "the8020/demo/variables", `[scaling]
replicas_min = 1
replicas_max = 1
workers_per_replica_min = 2
workers_per_replica_max = 4
`)
	pools, router := newFakePools(), &fakeRouter{}
	manager := newTestManager(t, store, pools, router, filepath.Join(root, "node", "kernel", "node-a", "services"))
	var logs bytes.Buffer
	manager.logger = slog.New(slog.NewJSONHandler(&logs, nil))

	started, err := manager.Start(context.Background(), "the8020/demo/variables")
	if err != nil {
		t.Fatal(err)
	}
	if started.State != StateReady || started.DesiredGeneration != 1 || started.LoadedGeneration != 1 || started.InstanceCount != 1 || started.WorkerCount != 2 {
		t.Fatalf("started = %#v", started)
	}
	state, exists, err := store.ReadState("the8020/demo/variables")
	if err != nil || !exists || !state.Enabled || state.Generation != 1 {
		t.Fatalf("state=%#v exists=%t err=%v", state, exists, err)
	}
	firstPool := started.Instances[0].PoolID

	restarted, err := manager.Restart(context.Background(), "the8020/demo/variables")
	if err != nil {
		t.Fatal(err)
	}
	if restarted.LoadedGeneration != 2 || restarted.Instances[0].PoolID == firstPool {
		t.Fatalf("restarted = %#v", restarted)
	}
	assertEventBefore(t, pools.events, "start:"+restarted.Instances[0].PoolID+":2", "stop:"+firstPool)

	pools.failGeneration[3] = errors.New("entrypoint initialization failed")
	degraded, err := manager.Restart(context.Background(), "the8020/demo/variables")
	if err == nil || degraded.State != StateDegraded || degraded.LoadedGeneration != 2 || degraded.DesiredGeneration != 3 || degraded.FailedGeneration != 3 {
		t.Fatalf("degraded=%#v err=%v", degraded, err)
	}
	if record, inspectErr := pools.Inspect(restarted.Instances[0].PoolID); inspectErr != nil || record.State != "READY" {
		t.Fatalf("old capacity was not retained: record=%#v err=%v", record, inspectErr)
	}
	delete(pools.failGeneration, 3)
	if err := manager.ReconcileAll(context.Background()); err != nil {
		t.Fatal(err)
	}

	minimumReplicas, maximumReplicas, minimumWorkers, maximumWorkers, concurrency := 2, 2, 3, 5, 8
	targetUtilization, keepAlive, sandboxGroup := 0.65, "3m", "shared-proof"
	scaled, err := manager.Scale(context.Background(), "the8020/demo/variables", ScaleOptions{
		ReplicasMinimum:          &minimumReplicas,
		ReplicasMaximum:          &maximumReplicas,
		WorkersPerReplicaMinimum: &minimumWorkers,
		WorkersPerReplicaMaximum: &maximumWorkers,
		ConcurrencyPerWorker:     &concurrency,
		TargetUtilization:        &targetUtilization,
		KeepAlive:                &keepAlive,
		SandboxGroup:             &sandboxGroup,
	})
	if err != nil {
		t.Fatal(err)
	}
	if scaled.LoadedGeneration != 4 || scaled.InstanceCount != 2 || scaled.WorkerCount != 6 {
		t.Fatalf("scaled = %#v", scaled)
	}
	if scaled.Effective.Execution.ConcurrencyPerWorker != 8 || scaled.Effective.Execution.KeepAlive != 3*time.Minute || scaled.Effective.Scaling.TargetUtilization != 0.65 || scaled.Effective.Placement.SandboxGroup != "shared-proof" {
		t.Fatalf("scaled effective configuration = %#v", scaled.Effective)
	}
	stopped, err := manager.Stop(context.Background(), "the8020/demo/variables")
	if err != nil {
		t.Fatal(err)
	}
	if stopped.State != StateStopped || stopped.Enabled || stopped.DesiredGeneration != 5 || stopped.InstanceCount != 0 {
		t.Fatalf("stopped = %#v", stopped)
	}
	if err := manager.ReconcileAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := manager.ReconcileAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if count := strings.Count(logs.String(), `"msg":"service stopped"`); count != 1 {
		t.Fatalf("service stopped log count = %d; logs=%s", count, logs.String())
	}
	if _, err := os.Stat(filepath.Join(root, "node", "kernel", "node-a", "services", "the8020", "demo", "variables", "status.json")); err != nil {
		t.Fatal(err)
	}
}

func TestCanonicalBoundaryRereadsDiskStripsPrefixAndUsesTrustedMetadata(t *testing.T) {
	root := t.TempDir()
	store := writeTestService(t, root, "the8020/demo/variables", "")
	pools, router := newFakePools(), &fakeRouter{}
	pools.responseStatus, pools.responseBody = http.StatusTeapot, "service-body"
	pools.dispatched = make(chan dispatchedRequest, 1)
	manager := newTestManager(t, store, pools, router, filepath.Join(root, "node", "kernel", "services"))

	assertHTTPStatus(t, manager, "/missing/package/service/path", http.StatusNotFound)
	assertHTTPStatus(t, manager, "/the8020/demo/variables/path", http.StatusServiceUnavailable)
	assertHTTPStatus(t, manager, "/the8020/demo/variables%2fescape/path", http.StatusBadRequest)
	if _, err := manager.Start(context.Background(), "the8020/demo/variables"); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPatch, "http://example.test/the8020/demo/variables/orders/7?expand=yes", bytes.NewBufferString("stream me"))
	request.Header.Set("X-80-20-Internal-Service-ID", "attacker/service/value")
	request.Header.Set("X-Custom", "preserved")
	response := httptest.NewRecorder()
	manager.ServeHTTP(response, request)
	if response.Code != http.StatusTeapot || response.Body.String() != "service-body" {
		t.Fatalf("response = %d %q", response.Code, response.Body.String())
	}
	dispatched := <-pools.dispatched
	if dispatched.scheme != "http" || dispatched.host != "service" || dispatched.method != http.MethodPatch || dispatched.path != "/orders/7" || dispatched.query != "expand=yes" || dispatched.body != "stream me" || dispatched.header.Get("X-Custom") != "preserved" {
		t.Fatalf("forwarded request = %#v", dispatched)
	}
	if dispatched.header.Get("X-80-20-Internal-Service-Id") != "the8020/demo/variables" || dispatched.header.Get("X-80-20-Internal-Canonical-Base-Path") != "/the8020/demo/variables" || dispatched.header.Get("X-80-20-Internal-Original-Host") != "example.test" {
		t.Fatalf("trusted headers = %#v", dispatched.header)
	}
	adminResult, err := manager.Request(context.Background(), "the8020/demo/variables", http.MethodGet, "/admin", RequestOptions{})
	if err != nil || adminResult.StatusCode != http.StatusTeapot || adminResult.Body != "service-body" {
		t.Fatalf("administrative request=%#v err=%v", adminResult, err)
	}
	<-pools.dispatched
	exact := httptest.NewRecorder()
	manager.ServeHTTP(exact, httptest.NewRequest(http.MethodGet, "/the8020/demo/variables", nil))
	if exact.Code != http.StatusTeapot || (<-pools.dispatched).path != "/" {
		t.Fatalf("exact canonical prefix response=%d", exact.Code)
	}
	assertHTTPStatus(t, manager, "/the8020/demo/variables/%2e%2e/secret", http.StatusBadRequest)
	assertHTTPStatus(t, manager, "/the8020/demo/variables/%5cescape", http.StatusBadRequest)

	statePath := filepath.Join(root, "state", "services", "the8020", "demo", "variables", "state.toml")
	writeTestFile(t, statePath, "schema = 1\nenabled = false\ngeneration = 1\n")
	assertHTTPStatus(t, manager, "/the8020/demo/variables/path", http.StatusServiceUnavailable)
	writeTestFile(t, statePath, "schema = 1\nenabled = true\ngeneration = 1\n")
	writeTestFile(t, filepath.Join(root, "packages", "the8020", "demo", "services", "variables", "service.toml"), "schema = [\n")
	assertHTTPStatus(t, manager, "/the8020/demo/variables/path", http.StatusServiceUnavailable)
	select {
	case request := <-pools.dispatched:
		t.Fatalf("request with unreadable current access policy reached a Worker: %#v", request)
	default:
	}
}

func TestAuthenticatedBoundaryRejectsOrRedirectsBeforeDispatchAndAttachesTrustedContext(t *testing.T) {
	root := t.TempDir()
	store := writeTestService(t, root, "the8020/demo/protected", `[access]
mode = "authenticated"
[access.unauthenticated]
action = "reject"
status = 401
message = "Sign in first."
`)
	pools, router := newFakePools(), &fakeRouter{}
	pools.dispatched = make(chan dispatchedRequest, 1)
	manager := newTestManager(t, store, pools, router, filepath.Join(root, "node", "kernel", "services"))
	authentication := &fakeAuthentication{}
	manager.authentication = authentication
	if _, err := manager.Start(context.Background(), "the8020/demo/protected"); err != nil {
		t.Fatal(err)
	}

	unauthenticated := httptest.NewRecorder()
	spoofed := httptest.NewRequest(http.MethodGet, "/the8020/demo/protected/value", nil)
	spoofed.Header.Set("X-80-20-Internal-Auth-Username", "attacker")
	manager.ServeHTTP(unauthenticated, spoofed)
	if unauthenticated.Code != 401 || unauthenticated.Body.String() != "Sign in first.\n" || authentication.calls != 0 {
		t.Fatalf("missing-cookie response=%d %q calls=%d", unauthenticated.Code, unauthenticated.Body.String(), authentication.calls)
	}
	select {
	case request := <-pools.dispatched:
		t.Fatalf("unauthenticated request reached a Worker: %#v", request)
	default:
	}

	invalid := httptest.NewRecorder()
	invalidRequest := httptest.NewRequest(http.MethodGet, "/the8020/demo/protected/value", nil)
	invalidRequest.AddCookie(&http.Cookie{Name: "the8020_auth", Value: "invalid-cookie"})
	manager.ServeHTTP(invalid, invalidRequest)
	if invalid.Code != 401 || authentication.calls != 1 {
		t.Fatalf("invalid-cookie response=%d calls=%d", invalid.Code, authentication.calls)
	}

	authorized := httptest.NewRecorder()
	authorizedRequest := httptest.NewRequest(http.MethodGet, "/the8020/demo/protected/value", nil)
	authorizedRequest.AddCookie(&http.Cookie{Name: "other", Value: "preserved"})
	authorizedRequest.AddCookie(&http.Cookie{Name: "the8020_auth", Value: "valid-opaque-cookie"})
	authorizedRequest.Header.Set("X-80-20-Internal-Auth-Username", "attacker")
	manager.ServeHTTP(authorized, authorizedRequest)
	if authorized.Code != http.StatusOK || authentication.calls != 2 {
		t.Fatalf("authorized response=%d calls=%d body=%q", authorized.Code, authentication.calls, authorized.Body.String())
	}
	forwarded := <-pools.dispatched
	for name, want := range map[string]string{
		"X-80-20-Internal-Auth-Authenticated": "true",
		"X-80-20-Internal-Auth-Realm":         platformauth.BootstrapRealm,
		"X-80-20-Internal-Auth-User-Id":       "bootstrap-admin:Admin",
		"X-80-20-Internal-Auth-Username":      "Admin",
		"X-80-20-Internal-Auth-Version":       "7",
	} {
		if got := forwarded.header.Get(name); got != want {
			t.Errorf("forwarded %s = %q, want %q", name, got, want)
		}
	}
	if cookie := forwarded.header.Get("Cookie"); cookie != "other=preserved" || strings.Contains(cookie, "valid-opaque-cookie") {
		t.Fatalf("forwarded cookies = %q", cookie)
	}
	if serialized := fmt.Sprint(forwarded.header); strings.Contains(serialized, "kernel-only-session") {
		t.Fatalf("kernel-only session identity escaped: %s", serialized)
	}

	serviceManifest := filepath.Join(root, "packages", "the8020", "demo", "services", "protected", "service.toml")
	writeTestFile(t, serviceManifest, `schema = 1
description = "Test service"
entrypoint = "service.ts"
[access]
mode = "authenticated"
[access.unauthenticated]
action = "redirect"
status = 307
redirect_url = "https://identity.example.test/login?return=fixed"
`)
	redirected := httptest.NewRecorder()
	manager.ServeHTTP(redirected, httptest.NewRequest(http.MethodGet, "/the8020/demo/protected/value?return=https://attacker.test", nil))
	if redirected.Code != 307 || redirected.Header().Get("Location") != "https://identity.example.test/login?return=fixed" {
		t.Fatalf("redirect response=%d location=%q", redirected.Code, redirected.Header().Get("Location"))
	}
	select {
	case request := <-pools.dispatched:
		t.Fatalf("redirected request reached a Worker: %#v", request)
	default:
	}
}

func TestPublicServiceAcceptsOptionalAuthenticationForLogoutWithoutRequiringIt(t *testing.T) {
	root := t.TempDir()
	store := writeTestService(t, root, "example/auth/login", "")
	pools, router := newFakePools(), &fakeRouter{}
	pools.dispatched = make(chan dispatchedRequest, 2)
	manager := newTestManager(t, store, pools, router, filepath.Join(root, "node", "kernel", "services"))
	manager.authentication = &fakeAuthentication{}
	if _, err := manager.Start(context.Background(), "example/auth/login"); err != nil {
		t.Fatal(err)
	}

	authenticated := httptest.NewRequest(http.MethodPost, "/example/auth/login/logout", nil)
	authenticated.AddCookie(&http.Cookie{Name: "the8020_auth", Value: "valid-opaque-cookie"})
	authenticatedResponse := httptest.NewRecorder()
	manager.ServeHTTP(authenticatedResponse, authenticated)
	if authenticatedResponse.Code != http.StatusOK {
		t.Fatalf("authenticated public response = %d", authenticatedResponse.Code)
	}
	trusted := <-pools.dispatched
	if trusted.header.Get("X-80-20-Internal-Auth-Authenticated") != "true" || trusted.header.Get("X-80-20-Internal-Auth-Username") != "Admin" || trusted.header.Get("Cookie") != "" {
		t.Fatalf("optional trusted authentication = %#v", trusted.header)
	}

	anonymous := httptest.NewRequest(http.MethodGet, "/example/auth/login/", nil)
	anonymous.AddCookie(&http.Cookie{Name: "the8020_auth", Value: "invalid-cookie"})
	anonymousResponse := httptest.NewRecorder()
	manager.ServeHTTP(anonymousResponse, anonymous)
	if anonymousResponse.Code != http.StatusOK {
		t.Fatalf("invalid optional cookie blocked public service: %d", anonymousResponse.Code)
	}
	untrusted := <-pools.dispatched
	if untrusted.header.Get("X-80-20-Internal-Auth-Authenticated") != "false" || untrusted.header.Get("Cookie") != "" {
		t.Fatalf("anonymous public metadata = %#v", untrusted.header)
	}
}

func TestRequestServiceWebSocketUsesCanonicalRoutingAndTrustedMetadata(t *testing.T) {
	root := t.TempDir()
	store := writeTestService(t, root, "the8020/demo/events", "")
	pools, router := newFakePools(), &fakeRouter{}
	manager := newTestManager(t, store, pools, router, filepath.Join(root, "node", "kernel", "services"))
	if _, err := manager.Start(context.Background(), "the8020/demo/events"); err != nil {
		t.Fatal(err)
	}

	request := websocketRequest("/the8020/demo/events/echo/main?format=text", "")
	request.Header.Set("Sec-WebSocket-Protocol", "the8020.echo")
	request.Header.Set("X-80-20-Internal-Auth-Username", "attacker")
	response := httptest.NewRecorder()
	manager.ServeHTTP(response, request)
	if response.Code != http.StatusOK || len(pools.websockets) != 1 {
		t.Fatalf("request WebSocket status=%d proxies=%#v", response.Code, pools.websockets)
	}
	proxied := pools.websockets[0]
	if proxied.path != "/echo/main" || proxied.query != "format=text" || proxied.header.Get("Sec-WebSocket-Protocol") != "the8020.echo" {
		t.Fatalf("request WebSocket route = %#v", proxied)
	}
	if proxied.header.Get("X-80-20-Internal-Service-Id") != "the8020/demo/events" || proxied.header.Get("X-80-20-Internal-Canonical-Base-Path") != "/the8020/demo/events" || proxied.header.Get("X-80-20-Internal-Auth-Authenticated") != "false" {
		t.Fatalf("request WebSocket trusted metadata = %#v", proxied.header)
	}
	if proxied.header.Get("X-80-20-Internal-Auth-Username") != "" {
		t.Fatalf("request WebSocket leaked session or spoofed metadata = %#v", proxied.header)
	}
	status, err := manager.Inspect("the8020/demo/events")
	if err != nil || status.Metrics.ActiveRequests != 0 || status.Metrics.RequestCount != 1 || status.Metrics.ResponseStatus["101"] != 1 {
		t.Fatalf("request WebSocket accounting = %#v err=%v", status.Metrics, err)
	}
}

func TestPersistentServiceEstablishesHTTPRouteThenReconnectsWebSocketToExactReplica(t *testing.T) {
	root := t.TempDir()
	store := writeTestService(t, root, "example/realtime/channel", persistentServiceManifest)
	pools, router := newFakePools(), &fakeRouter{}
	manager := newTestManager(t, store, pools, router, filepath.Join(root, "node", "kernel", "services"))
	manager.authentication = &fakeAuthentication{}
	status, err := manager.Start(context.Background(), "example/realtime/channel")
	if err != nil {
		t.Fatal(err)
	}
	if status.ExecutionMode != workspacepackages.ExecutionModePersistent || status.InstanceCount != 2 || status.WorkerCount != 2 {
		t.Fatalf("persistent service status = %#v", status)
	}

	assertHTTPStatus(t, manager, "/example/realtime/channel/other", http.StatusUnauthorized)
	unauthenticatedUpgrade := websocketRequest("/example/realtime/channel/connect", "")
	unauthenticated := httptest.NewRecorder()
	manager.ServeHTTP(unauthenticated, unauthenticatedUpgrade)
	if unauthenticated.Code != http.StatusUnauthorized || len(pools.websockets) != 0 {
		t.Fatalf("unauthenticated upgrade = %d; proxies=%#v", unauthenticated.Code, pools.websockets)
	}
	pools.dispatched = make(chan dispatchedRequest, 1)
	establish := httptest.NewRequest(http.MethodPost, "/example/realtime/channel/connect", nil)
	establish.AddCookie(&http.Cookie{Name: "the8020_auth", Value: "valid-opaque-cookie"})
	established := httptest.NewRecorder()
	manager.ServeHTTP(established, establish)
	route := established.Header().Get(RouteHeader)
	initial := <-pools.dispatched
	if established.Code != http.StatusOK || route == "" || initial.header.Get("X-80-20-Internal-Persistent-Execution-Id") == "" || initial.header.Get("X-80-20-Internal-Persistent-Keep-Alive-Ms") != "120000" {
		t.Fatalf("establishment status=%d route=%q dispatch=%#v", established.Code, route, initial)
	}

	upgrade := websocketRequest("/example/realtime/channel/connect?route="+route, "")
	upgrade.AddCookie(&http.Cookie{Name: "the8020_auth", Value: "valid-opaque-cookie"})
	upgrade.Header.Set("X-80-20-Internal-Auth-Username", "attacker")
	response := httptest.NewRecorder()
	manager.ServeHTTP(response, upgrade)
	if response.Code != http.StatusOK || len(pools.websockets) != 1 {
		t.Fatalf("upgrade status=%d proxies=%#v", response.Code, pools.websockets)
	}
	proxied := pools.websockets[0]
	if proxied.poolID != initial.poolID || proxied.path != "/connect" || proxied.query != "" || proxied.header.Get("X-80-20-Internal-Auth-Username") != "Admin" || proxied.header.Get("X-80-20-Internal-Persistent-Execution-Id") == "" || proxied.header.Get("Cookie") != "" {
		t.Fatalf("proxied persistent request = %#v", proxied)
	}

	poolRecord := pools.records[initial.poolID]
	staleToken, _, err := manager.persistentRoutes.create("example/realtime/channel", initial.poolID, poolRecord.RuntimeGroupID, poolRecord.SandboxID, "bootstrap-admin:Admin", 2*time.Minute, false)
	if err != nil {
		t.Fatal(err)
	}
	manager.persistentRoutes.succeed(staleToken, "worker-from-before-kernel-restart")
	stale := httptest.NewRequest(http.MethodPost, "/example/realtime/channel/connect", nil)
	stale.Header.Set(RouteHeader, staleToken)
	stale.AddCookie(&http.Cookie{Name: "the8020_auth", Value: "valid-opaque-cookie"})
	staleResponse := httptest.NewRecorder()
	manager.ServeHTTP(staleResponse, stale)
	if staleResponse.Code != http.StatusConflict {
		t.Fatalf("stale route status=%d body=%q", staleResponse.Code, staleResponse.Body.String())
	}
	if _, err := manager.persistentRoutes.lookup(staleToken, "example/realtime/channel", "bootstrap-admin:Admin"); !errors.Is(err, errRouteNotFound) {
		t.Fatalf("stale route remained registered: %v", err)
	}
}

func TestPersistentRouteKeepsOldGenerationUntilKeepaliveExpires(t *testing.T) {
	root := t.TempDir()
	store := writeTestService(t, root, "example/realtime/channel", persistentServiceManifest)
	pools, router := newFakePools(), &fakeRouter{}
	manager := newTestManager(t, store, pools, router, filepath.Join(root, "node", "kernel", "services"))
	manager.authentication = &fakeAuthentication{}
	base := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	manager.persistentRoutes.now = func() time.Time { return base }
	initial, err := manager.Start(context.Background(), "example/realtime/channel")
	if err != nil {
		t.Fatal(err)
	}
	oldPool := initial.Instances[0].PoolID
	pools.dispatched = make(chan dispatchedRequest, 1)
	establish := httptest.NewRequest(http.MethodPost, "/example/realtime/channel/connect", nil)
	establish.AddCookie(&http.Cookie{Name: "the8020_auth", Value: "valid-opaque-cookie"})
	established := httptest.NewRecorder()
	manager.ServeHTTP(established, establish)
	route := established.Header().Get(RouteHeader)
	establishedDispatch := <-pools.dispatched
	oldPool = establishedDispatch.poolID
	if route == "" {
		t.Fatal("persistent establishment did not return a route")
	}

	restarted, err := manager.Restart(context.Background(), "example/realtime/channel")
	if err != nil {
		t.Fatal(err)
	}
	if restarted.LoadedGeneration == initial.LoadedGeneration || restarted.Instances[0].PoolID == oldPool {
		t.Fatalf("restart did not switch generation: initial=%#v restarted=%#v", initial, restarted)
	}
	if pools.records[oldPool].State != "READY" {
		t.Fatalf("old persistent replica was not retained: %#v", pools.records[oldPool])
	}

	resume := websocketRequest("/example/realtime/channel/connect?route="+route, "")
	resume.AddCookie(&http.Cookie{Name: "the8020_auth", Value: "valid-opaque-cookie"})
	manager.ServeHTTP(httptest.NewRecorder(), resume)
	if got := pools.websockets[len(pools.websockets)-1].poolID; got != oldPool {
		t.Fatalf("route selected %s, want old replica %s", got, oldPool)
	}

	manager.persistentRoutes.now = func() time.Time { return base.Add(3 * time.Minute) }
	if err := manager.ReconcileAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, exists := pools.records[oldPool]; exists {
		t.Fatalf("empty draining generation record was not removed: %#v", pools.records[oldPool])
	}
}

func TestPersistentRouteReceivedByAnotherNodeForwardsToOwner(t *testing.T) {
	root := t.TempDir()
	store := writeTestService(t, root, "example/realtime/channel", persistentServiceManifest)
	statePath := filepath.Join(root, "state", "services", "persistent-routes.json")
	owner := newPersistentRouteRegistry("node-a", statePath)
	token, _, err := owner.create("example/realtime/channel", "remote-pool", "remote-group", "remote-sandbox", "bootstrap-admin:Admin", 2*time.Minute, false)
	if err != nil {
		t.Fatal(err)
	}
	pools, router := newFakePools(), &fakeRouter{}
	nodeRouter := &fakeNodeRouter{local: "node-b"}
	manager, err := New(Config{Definitions: store, Pools: pools, Router: router, ObservedRoot: filepath.Join(root, "node", "kernel", "services"), NodeID: "node-b", PersistentRouteStatePath: statePath, Nodes: nodeRouter})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	manager.authentication = &fakeAuthentication{}
	if _, err := manager.Start(context.Background(), "example/realtime/channel"); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/example/realtime/channel/connect", nil)
	request.Header.Set(RouteHeader, token)
	request.AddCookie(&http.Cookie{Name: "the8020_auth", Value: "valid-opaque-cookie"})
	response := httptest.NewRecorder()
	manager.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || nodeRouter.calls != 1 || nodeRouter.node != "node-a" {
		t.Fatalf("status=%d router=%#v", response.Code, nodeRouter)
	}
}

func TestRequestSchemeRecognizesDirectAndForwardedHTTPS(t *testing.T) {
	direct := httptest.NewRequest(http.MethodGet, "https://example.test/path", nil)
	if requestScheme(direct) != "https" {
		t.Fatalf("direct HTTPS scheme = %q", requestScheme(direct))
	}
	forwarded := httptest.NewRequest(http.MethodGet, "http://example.test/path", nil)
	forwarded.Header.Set("X-Forwarded-Proto", "https, http")
	if requestScheme(forwarded) != "https" || originalURL(forwarded) != "https://example.test/path" {
		t.Fatalf("forwarded scheme=%q original=%q", requestScheme(forwarded), originalURL(forwarded))
	}
}

func TestCanonicalIdentitiesCannotConflictAcrossNamespaceOrRepository(t *testing.T) {
	root := t.TempDir()
	store := writeTestService(t, root, "namespace-a/repository/service", "")
	writeTestService(t, root, "namespace-b/repository/service", "")
	writeTestService(t, root, "namespace-a/other-repository/service", "")
	pools, router := newFakePools(), &fakeRouter{}
	pools.dispatched = make(chan dispatchedRequest, 1)
	manager := newTestManager(t, store, pools, router, filepath.Join(root, "node", "kernel", "services"))
	for _, serviceID := range []string{"namespace-a/repository/service", "namespace-b/repository/service", "namespace-a/other-repository/service"} {
		if _, err := manager.Start(context.Background(), serviceID); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]bool{}
	for _, path := range []string{
		"/namespace-a/repository/service/path",
		"/namespace-b/repository/service/path",
		"/namespace-a/other-repository/service/path",
	} {
		response := httptest.NewRecorder()
		manager.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusOK {
			t.Fatalf("%s status=%d", path, response.Code)
		}
		seen[(<-pools.dispatched).poolID] = true
	}
	if len(seen) != 3 {
		t.Fatalf("canonical identities selected conflicting pools: %#v", seen)
	}
}

func TestCompatibleServicesShareRuntimeGroupButKeepIndependentPoolsAndLifecycle(t *testing.T) {
	root := t.TempDir()
	store := writeTestService(t, root, "the8020/demo/variables", "")
	writeTestService(t, root, "the8020/demo/variables-import", "")
	pools, router := newFakePools(), &fakeRouter{}
	pools.dispatched = make(chan dispatchedRequest, 1)
	manager := newTestManager(t, store, pools, router, filepath.Join(root, "node", "kernel", "services"))
	first, err := manager.Start(context.Background(), "the8020/demo/variables")
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.Start(context.Background(), "the8020/demo/variables-import")
	if err != nil {
		t.Fatal(err)
	}
	if first.Instances[0].RuntimeGroupID == second.Instances[0].RuntimeGroupID {
		t.Fatal("default service identities unexpectedly shared a runtime group")
	}
	shared := "shared-proof"
	first, err = manager.Scale(context.Background(), first.ServiceID, ScaleOptions{SandboxGroup: &shared})
	if err != nil {
		t.Fatal(err)
	}
	second, err = manager.Scale(context.Background(), second.ServiceID, ScaleOptions{SandboxGroup: &shared})
	if err != nil {
		t.Fatal(err)
	}
	if first.Instances[0].RuntimeGroupID != second.Instances[0].RuntimeGroupID || first.Instances[0].PoolID == second.Instances[0].PoolID {
		t.Fatalf("first=%#v second=%#v", first.Instances, second.Instances)
	}
	for _, serviceID := range []string{first.ServiceID, second.ServiceID} {
		response := httptest.NewRecorder()
		identity := mustIdentity(t, serviceID)
		manager.ServeHTTP(response, httptest.NewRequest(http.MethodGet, identity.CanonicalBasePath()+"/value", nil))
		if response.Code != http.StatusOK || (<-pools.dispatched).poolID != manager.services[serviceID].instances[0].status.PoolID {
			t.Fatalf("request reached wrong pool for %s", serviceID)
		}
	}
	if _, err := manager.Restart(context.Background(), first.ServiceID); err != nil {
		t.Fatal(err)
	}
	if status, err := manager.Inspect(second.ServiceID); err != nil || status.State != StateReady {
		t.Fatalf("sibling after restart=%#v err=%v", status, err)
	}
	if _, err := manager.Stop(context.Background(), first.ServiceID); err != nil {
		t.Fatal(err)
	}
	if status, err := manager.Inspect(second.ServiceID); err != nil || status.State != StateReady || status.InstanceCount != 1 {
		t.Fatalf("sibling after stop=%#v err=%v", status, err)
	}
}

func TestLeastInFlightSelectionAndTimeout(t *testing.T) {
	root := t.TempDir()
	store := writeTestService(t, root, "the8020/demo/variables", `[scaling]
replicas_min = 2
replicas_max = 2
`)
	pools, router := newFakePools(), &fakeRouter{}
	pools.dispatched = make(chan dispatchedRequest, 3)
	pools.release = make(chan struct{})
	manager := newTestManager(t, store, pools, router, filepath.Join(root, "node", "kernel", "services"))
	started, err := manager.Start(context.Background(), "the8020/demo/variables")
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{}, 2)
	for index := 0; index < 2; index++ {
		go func() {
			manager.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/the8020/demo/variables/wait", nil))
			done <- struct{}{}
		}()
		first := <-pools.dispatched
		if first.poolID != started.Instances[index].PoolID {
			t.Fatalf("dispatch %d selected %s, want %s", index, first.poolID, started.Instances[index].PoolID)
		}
	}
	status, err := manager.Inspect("the8020/demo/variables")
	if err != nil || status.Metrics.ActiveRequests != 2 || status.Instances[0].ActiveRequests != 1 || status.Instances[1].ActiveRequests != 1 {
		t.Fatalf("in-flight status=%#v err=%v", status, err)
	}
	close(pools.release)
	<-done
	<-done

	pools.release = make(chan struct{})
	timeoutRequest := httptest.NewRequest(http.MethodGet, "/the8020/demo/variables/timeout", nil)
	timeoutRequest = timeoutRequest.WithContext(context.Background())
	timeoutResponse := httptest.NewRecorder()
	result := make(chan struct{})
	go func() {
		runtime := manager.services["the8020/demo/variables"]
		manager.dispatch(timeoutResponse, timeoutRequest, mustIdentity(t, "the8020/demo/variables"), "/timeout", runtime, runtime.instances[0], time.Millisecond, platformauth.AuthContext{}, nil)
		close(result)
	}()
	<-pools.dispatched
	<-result
	if timeoutResponse.Code != http.StatusGatewayTimeout {
		t.Fatalf("timeout status = %d", timeoutResponse.Code)
	}
}

func TestTargetCapacityAddsReplicaAfterWorkersReachMaximum(t *testing.T) {
	root := t.TempDir()
	store := writeTestService(t, root, "the8020/demo/variables", `[execution]
concurrency_per_worker = 1
[scaling]
replicas_min = 1
replicas_max = 2
workers_per_replica_min = 1
workers_per_replica_max = 1
target_utilization = 0.7
`)
	pools, router := newFakePools(), &fakeRouter{}
	pools.dispatched = make(chan dispatchedRequest, 1)
	manager := newTestManager(t, store, pools, router, filepath.Join(root, "node", "kernel", "services"))
	started, err := manager.Start(context.Background(), "the8020/demo/variables")
	if err != nil {
		t.Fatal(err)
	}
	pools.mu.Lock()
	pools.capacityErrors[started.Instances[0].PoolID] = &executionservices.ReplicaCapacityError{Occupied: 1, Slots: 1, Reason: "all Worker slots are occupied"}
	pools.mu.Unlock()
	response := httptest.NewRecorder()
	manager.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/the8020/demo/variables/value", nil))
	request := <-pools.dispatched
	status, inspectErr := manager.Inspect("the8020/demo/variables")
	if inspectErr != nil || response.Code != http.StatusOK || status.InstanceCount != 2 || request.poolID == started.Instances[0].PoolID {
		t.Fatalf("response=%d request=%#v status=%#v err=%v", response.Code, request, status, inspectErr)
	}
}

func TestIdleReplicaAboveMinimumIsRemovedAfterCooldown(t *testing.T) {
	root := t.TempDir()
	store := writeTestService(t, root, "the8020/demo/variables", `[execution]
concurrency_per_worker = 1
[scaling]
replicas_min = 1
replicas_max = 2
workers_per_replica_min = 1
workers_per_replica_max = 1
target_utilization = 0.7
`)
	pools, router := newFakePools(), &fakeRouter{}
	manager := newTestManager(t, store, pools, router, filepath.Join(root, "node", "kernel", "services"))
	manager.replicaScaleDownCooldown = time.Nanosecond
	started, err := manager.Start(context.Background(), "the8020/demo/variables")
	if err != nil {
		t.Fatal(err)
	}
	pools.mu.Lock()
	pools.capacityErrors[started.Instances[0].PoolID] = &executionservices.ReplicaCapacityError{Occupied: 1, Slots: 1, Reason: "all Worker slots are occupied"}
	pools.mu.Unlock()
	runtime := manager.services["the8020/demo/variables"]
	definition, err := store.ReadService("the8020/demo/variables")
	if err != nil {
		t.Fatal(err)
	}
	added, err := manager.addCapacityReplica(context.Background(), runtime, definition)
	if err != nil {
		t.Fatal(err)
	}
	pools.mu.Lock()
	delete(pools.capacityErrors, started.Instances[0].PoolID)
	pools.occupiedSlots[started.Instances[0].PoolID] = 1
	pools.occupiedSlots[added.status.PoolID] = 0
	pools.mu.Unlock()
	if err := manager.ReconcileAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)
	if err := manager.ReconcileAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	status, err := manager.Inspect("the8020/demo/variables")
	if err != nil || status.InstanceCount != 1 || status.Instances[0].PoolID != started.Instances[0].PoolID {
		t.Fatalf("status=%#v err=%v", status, err)
	}
	if _, err := pools.Inspect(added.status.PoolID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("retired replica record still exists: %v", err)
	}
}

type streamCaptureWriter struct {
	header http.Header
	status int
	writes chan string
	mu     sync.Mutex
	body   strings.Builder
}

func (w *streamCaptureWriter) Header() http.Header    { return w.header }
func (w *streamCaptureWriter) WriteHeader(status int) { w.status = status }
func (w *streamCaptureWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	w.body.Write(data)
	w.mu.Unlock()
	w.writes <- string(data)
	return len(data), nil
}

func TestCanonicalBoundaryStreamsRequestAndResponseWithoutCompleteBuffering(t *testing.T) {
	root := t.TempDir()
	store := writeTestService(t, root, "the8020/demo/variables", "")
	pools, router := newFakePools(), &fakeRouter{}
	pools.dispatchEntered = make(chan struct{}, 1)
	responseReader, responseWriter := io.Pipe()
	pools.responseStream = responseReader
	manager := newTestManager(t, store, pools, router, filepath.Join(root, "node", "kernel", "services"))
	if _, err := manager.Start(context.Background(), "the8020/demo/variables"); err != nil {
		t.Fatal(err)
	}

	requestReader, requestWriter := io.Pipe()
	request := httptest.NewRequest(http.MethodPost, "/the8020/demo/variables/upload", requestReader)
	output := &streamCaptureWriter{header: make(http.Header), writes: make(chan string, 4)}
	done := make(chan struct{})
	go func() {
		manager.ServeHTTP(output, request)
		close(done)
	}()
	select {
	case <-pools.dispatchEntered:
	case <-time.After(time.Second):
		t.Fatal("runtime pool did not receive the request stream")
	}
	go func() {
		_, _ = requestWriter.Write([]byte("upload-one"))
		_, _ = requestWriter.Write([]byte("-upload-two"))
		_ = requestWriter.Close()
	}()
	go func() { _, _ = responseWriter.Write([]byte("response-one")) }()
	select {
	case first := <-output.writes:
		if first != "response-one" {
			t.Fatalf("first streamed response chunk = %q", first)
		}
	case <-time.After(time.Second):
		t.Fatal("first response chunk was buffered")
	}
	if _, err := responseWriter.Write([]byte("-response-two")); err != nil {
		t.Fatal(err)
	}
	if err := responseWriter.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("streaming request did not complete")
	}
	output.mu.Lock()
	body := output.body.String()
	output.mu.Unlock()
	if output.status != http.StatusOK || body != "response-one-response-two" {
		t.Fatalf("status=%d body=%q", output.status, body)
	}
}

func TestFirstValidationCreatesStateAndTwoNodesReconcileSharedDesiredState(t *testing.T) {
	root := t.TempDir()
	storeA := writeTestService(t, root, "the8020/demo/variables", "")
	storeB, err := workspacepackages.New(workspacepackages.Config{WorkspaceRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	poolsA, poolsB := newFakePools(), newFakePools()
	managerA := newTestManager(t, storeA, poolsA, &fakeRouter{}, filepath.Join(root, "node-a", "kernel", "runtime", "services"))
	managerB := newTestManager(t, storeB, poolsB, &fakeRouter{}, filepath.Join(root, "node-b", "kernel", "runtime", "services"))

	validation := managerA.Validate(context.Background(), "the8020/demo/variables")
	if !validation.Valid || validation.OpenAPI["openapi"] != "3.1.0" {
		t.Fatalf("validation = %#v", validation)
	}
	if len(poolsA.records) != 0 {
		t.Fatalf("validation pool record leaked: %#v", poolsA.records)
	}
	statePath := filepath.Join(root, "state", "services", "the8020", "demo", "variables", "state.toml")
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("validation did not initialize shared state: %v", err)
	}
	if _, err := managerA.Start(context.Background(), "the8020/demo/variables"); err != nil {
		t.Fatal(err)
	}
	if err := managerB.ReconcileAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	for name, manager := range map[string]*Manager{"node-a": managerA, "node-b": managerB} {
		status, err := manager.Inspect("the8020/demo/variables")
		if err != nil || status.State != StateReady || status.LoadedGeneration != 1 || status.InstanceCount != 1 {
			t.Fatalf("%s status=%#v err=%v", name, status, err)
		}
	}
	if status, err := managerB.Restart(context.Background(), "the8020/demo/variables"); err != nil || status.LoadedGeneration != 2 {
		t.Fatalf("node B restart=%#v err=%v", status, err)
	}
	if err := managerA.ReconcileAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if status, err := managerA.Inspect("the8020/demo/variables"); err != nil || status.LoadedGeneration != 2 {
		t.Fatalf("node A did not observe restart: %#v err=%v", status, err)
	}
	if status, err := managerA.Stop(context.Background(), "the8020/demo/variables"); err != nil || status.State != StateStopped {
		t.Fatalf("node A stop=%#v err=%v", status, err)
	}
	if err := managerB.ReconcileAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if status, err := managerB.Inspect("the8020/demo/variables"); err != nil || status.State != StateStopped || status.DesiredGeneration != 3 {
		t.Fatalf("node B did not observe stop: %#v err=%v", status, err)
	}
	for _, path := range []string{
		filepath.Join(root, "node-a", "kernel", "runtime", "services", "the8020", "demo", "variables", "status.json"),
		filepath.Join(root, "node-b", "kernel", "runtime", "services", "the8020", "demo", "variables", "status.json"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatal(err)
		}
	}
}

func newTestManager(t *testing.T, store *workspacepackages.Store, pools *fakePools, router *fakeRouter, observed string) *Manager {
	t.Helper()
	manager, err := New(Config{Definitions: store, Pools: pools, Router: router, ObservedRoot: observed, ReconcileInterval: 10 * time.Millisecond, StartupTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	return manager
}

func writeTestService(t *testing.T, root, serviceID, defaults string) *workspacepackages.Store {
	t.Helper()
	identity, err := workspacepackages.ParseServiceID(serviceID)
	if err != nil {
		t.Fatal(err)
	}
	packageRoot := filepath.Join(root, "packages", identity.Namespace, identity.Repository)
	serviceRoot := filepath.Join(packageRoot, "services", identity.Service)
	writeTestFile(t, filepath.Join(packageRoot, "package.toml"), "schema = 1\ndescription = \"Test package\"\n")
	writeTestFile(t, filepath.Join(serviceRoot, "service.toml"), "schema = 1\ndescription = \"Test service\"\nentrypoint = \"service.ts\"\n"+defaults)
	writeTestFile(t, filepath.Join(serviceRoot, "service.ts"), "export default {};\n")
	store, err := workspacepackages.New(workspacepackages.Config{WorkspaceRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func writeTestFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

func assertHTTPStatus(t *testing.T, handler http.Handler, path string, status int) {
	t.Helper()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
	if response.Code != status {
		t.Fatalf("%s status = %d, want %d; body=%q", path, response.Code, status, response.Body.String())
	}
}

func websocketRequest(path, protocol string) *http.Request {
	request := httptest.NewRequest(http.MethodGet, path, nil)
	request.Header.Set("Connection", "keep-alive, Upgrade")
	request.Header.Set("Upgrade", "websocket")
	request.Header.Set("Sec-WebSocket-Protocol", "example.protocol.v1"+protocol)
	return request
}

func assertEventBefore(t *testing.T, events []string, first, second string) {
	t.Helper()
	firstIndex, secondIndex := -1, -1
	for index, event := range events {
		if event == first && firstIndex < 0 {
			firstIndex = index
		}
		if event == second && secondIndex < 0 {
			secondIndex = index
		}
	}
	if firstIndex < 0 || secondIndex < 0 || firstIndex >= secondIndex {
		t.Fatalf("events = %#v; want %q before %q", events, first, second)
	}
}

func mustIdentity(t *testing.T, serviceID string) workspacepackages.Identity {
	t.Helper()
	identity, err := workspacepackages.ParseServiceID(serviceID)
	if err != nil {
		t.Fatal(err)
	}
	return identity
}
