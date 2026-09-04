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
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	platformauth "the8020/kernel/auth"
	"the8020/kernel/database"
	executionservices "the8020/kernel/execution/services"
	executionworkers "the8020/kernel/execution/workers"
	workspacepackages "the8020/kernel/packages"
)

const persistentServiceManifest = `[lifecycle]
service_type = "session"
session_keep_alive = "2m"
[access]
mode = "authenticated"
[access.unauthenticated]
action = "reject"
status = 401
message = "Authentication is required."
[scaling]
minimum_workers = 2
maximum_workers = 16
concurrency_per_worker = 1
target_utilization = 0.7
worker_keep_alive = "2m"
[placement]
sandbox_group = "realtime"
minimum_sandboxes = 2
workers_per_sandbox = 8
`

type fakeRouter struct{ boundary http.Handler }

type fakeNodeRouter struct {
	local   string
	node    string
	calls   int
	indexes []int
}

func (r *fakeNodeRouter) LocalNodeID() string { return r.local }
func (r *fakeNodeRouter) LocalIndexes(limit int) []int {
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
func (r *fakeNodeRouter) OwnsIndex(index int) bool {
	if r.indexes == nil {
		return index >= 0
	}
	return slices.Contains(r.indexes, index)
}

func TestMinimumSandboxesUseOnlyIndexesAssignedToLocalNode(t *testing.T) {
	root := t.TempDir()
	store := writeTestService(t, root, "the8020/demo/variables", `[scaling]
minimum_workers = 4
maximum_workers = 16
[placement]
minimum_sandboxes = 4
workers_per_sandbox = 2
`)
	pools, router := newFakePools(), &fakeRouter{}
	manager := newTestManager(t, store, pools, router, filepath.Join(root, "node", "kernel", "services"))
	manager.nodes = &fakeNodeRouter{local: "node-b", indexes: []int{1, 3, 5, 7}}
	status, err := manager.Start(context.Background(), "the8020/demo/variables")
	if err != nil || status.State != StateReady || status.SandboxCount != 2 || status.Sandboxes[0].Index != 1 || status.Sandboxes[1].Index != 3 {
		t.Fatalf("status=%#v err=%v", status, err)
	}
}

func TestReconcileMovesMinimumSandboxesWhenNodeAssignmentChanges(t *testing.T) {
	root := t.TempDir()
	store := writeTestService(t, root, "the8020/demo/variables", `[scaling]
minimum_workers = 4
maximum_workers = 8
[placement]
minimum_sandboxes = 4
workers_per_sandbox = 2
`)
	pools, router := newFakePools(), &fakeRouter{}
	manager := newTestManager(t, store, pools, router, filepath.Join(root, "node", "kernel", "services"))
	nodes := &fakeNodeRouter{local: "node-a", indexes: []int{0, 2}}
	manager.nodes = nodes
	status, err := manager.Start(context.Background(), "the8020/demo/variables")
	if err != nil || len(status.Sandboxes) != 2 || status.Sandboxes[0].Index != 0 || status.Sandboxes[1].Index != 2 {
		t.Fatalf("initial status=%#v err=%v", status, err)
	}
	nodes.indexes = []int{1, 3}
	if err := manager.ReconcileAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	status, err = manager.Inspect("the8020/demo/variables")
	if err != nil || len(status.Sandboxes) != 2 || status.Sandboxes[0].Index != 1 || status.Sandboxes[1].Index != 3 {
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
func (a *fakeAuthentication) ValidateCookieContext(_ context.Context, value string) (platformauth.AuthContext, error) {
	a.calls++
	if value != "valid-opaque-cookie" {
		return platformauth.AuthContext{}, platformauth.ErrUnauthenticated
	}
	return platformauth.AuthContext{Authenticated: true, Realm: platformauth.UserRealm, UserID: "user:Admin", Username: "Admin", AuthVersion: 7, SessionID: "kernel-only-session"}, nil
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
	failVersion     map[uint64]error
	failStart       map[string]error
	failScale       map[string]error
	failStop        map[string]error
	dispatched      chan dispatchedRequest
	release         chan struct{}
	startEntered    chan string
	startRelease    chan struct{}
	stopEntered     chan string
	stopRelease     chan struct{}
	dispatchEntered chan struct{}
	responseStatus  int
	responseBody    string
	responseStream  io.ReadCloser
	workerStatus    map[string]int
	workerBody      map[string]string
	workerErrors    map[string]error
	capacityErrors  map[string]error
	occupiedSlots   map[string]int
	occupiedFloors  map[string][]int
	capacityCalls   map[string]int
	ensureCalls     map[string]int
	websockets      []dispatchedRequest
}

func newFakePools() *fakePools {
	return &fakePools{
		records:        map[string]executionservices.Record{},
		options:        map[string]executionservices.Options{},
		failVersion:    map[uint64]error{},
		failStart:      map[string]error{},
		failScale:      map[string]error{},
		failStop:       map[string]error{},
		responseStatus: http.StatusOK,
		responseBody:   "ok",
		workerStatus:   map[string]int{},
		workerBody:     map[string]string{},
		workerErrors:   map[string]error{},
		capacityErrors: map[string]error{},
		occupiedSlots:  map[string]int{},
		occupiedFloors: map[string][]int{},
		capacityCalls:  map[string]int{},
		ensureCalls:    map[string]int{},
	}
}

func (p *fakePools) ListForService(logicalServiceID string) ([]executionservices.Record, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	result := make([]executionservices.Record, 0)
	for _, record := range p.records {
		if record.LogicalServiceID == logicalServiceID {
			result = append(result, record)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ServiceID < result[j].ServiceID })
	return result, nil
}

func (p *fakePools) Start(_ context.Context, serviceID, entrypoint string, options executionservices.Options) (executionservices.Record, error) {
	p.mu.Lock()
	startEntered, startRelease := p.startEntered, p.startRelease
	p.mu.Unlock()
	if startEntered != nil {
		startEntered <- serviceID
	}
	if startRelease != nil {
		<-startRelease
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, fmt.Sprintf("start:%s:%d", serviceID, options.Generation))
	if err := p.failStart[serviceID]; err != nil {
		return executionservices.Record{}, err
	}
	if err := p.failVersion[options.Generation]; err != nil {
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
		ConcurrencyPerWorker: options.ConcurrencyPerWorker, ExecutionMode: options.ExecutionMode, SandboxIndex: options.SandboxIndex,
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

func (p *fakePools) Capacity(_ context.Context, serviceID string) (executionservices.Record, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	record, exists := p.records[serviceID]
	if !exists {
		return record, os.ErrNotExist
	}
	p.capacityCalls[serviceID]++
	occupied := p.occupiedSlots[serviceID]
	var capacity *executionservices.SandboxCapacityError
	if errors.As(p.capacityErrors[serviceID], &capacity) {
		occupied = max(occupied, capacity.Occupied)
	}
	record.OccupiedSlots = occupied
	record.CapacitySlots = len(record.WorkerIDs) * record.ConcurrencyPerWorker
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
	if err := p.failScale[serviceID]; err != nil {
		return record, err
	}
	record.WorkerIDs = make([]string, count)
	for index := range record.WorkerIDs {
		record.WorkerIDs[index] = fmt.Sprintf("%s-worker-%d", serviceID, index)
	}
	p.records[serviceID] = record
	return record, nil
}

func (p *fakePools) EnsureCapacity(ctx context.Context, serviceID string, growthLimit, occupiedFloor int) (executionservices.Record, error) {
	record, err := p.Inspect(serviceID)
	p.mu.Lock()
	capacityErr := p.capacityErrors[serviceID]
	p.ensureCalls[serviceID]++
	p.occupiedFloors[serviceID] = append(p.occupiedFloors[serviceID], occupiedFloor)
	p.mu.Unlock()
	if err == nil && capacityErr == nil && len(record.WorkerIDs) == 0 && growthLimit > 0 {
		record, err = p.Scale(ctx, serviceID, 1)
	}
	return record, errors.Join(err, capacityErr)
}

func (p *fakePools) ReconcileCapacity(ctx context.Context, serviceID string, minimumWorkers int) (executionservices.Record, error) {
	record, err := p.Inspect(serviceID)
	if err != nil {
		return record, err
	}
	p.mu.Lock()
	occupied := p.occupiedSlots[serviceID]
	p.mu.Unlock()
	target := max(len(record.WorkerIDs), minimumWorkers)
	if occupied == 0 && target > minimumWorkers {
		target = minimumWorkers
	}
	record, err = p.Scale(ctx, serviceID, target)
	record.OccupiedSlots = occupied
	record.CapacitySlots = len(record.WorkerIDs) * record.ConcurrencyPerWorker
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

func (p *fakePools) Stop(_ context.Context, serviceID string) (bool, error) {
	p.mu.Lock()
	stopEntered, stopRelease := p.stopEntered, p.stopRelease
	p.mu.Unlock()
	if stopEntered != nil {
		stopEntered <- serviceID
	}
	if stopRelease != nil {
		<-stopRelease
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, "stop:"+serviceID)
	if err := p.failStop[serviceID]; err != nil {
		return false, err
	}
	record, exists := p.records[serviceID]
	if !exists {
		return false, os.ErrNotExist
	}
	if p.occupiedSlots[serviceID] > 0 {
		record.State = "DRAINING"
		p.records[serviceID] = record
		return false, nil
	}
	record.State = "STOPPED"
	record.WorkerIDs = nil
	p.records[serviceID] = record
	return true, nil
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

func TestReconcileRetriesPersistedStaleVersionPoolCleanup(t *testing.T) {
	root := t.TempDir()
	store := writeTestService(t, root, "the8020/demo/variables", "")
	pools, router := newFakePools(), &fakeRouter{}
	staleID := "stale-version-pool"
	pools.records[staleID] = executionservices.Record{
		ServiceID: staleID, LogicalServiceID: "the8020/demo/variables",
		ReleaseID: "service-version-0", Generation: 0, State: "READY",
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
		t.Fatalf("stale version record was not removed on retry: %#v", pools.records[staleID])
	}
}

func TestBackgroundReconciliationLeavesEnabledServiceIdleUntilFirstRequest(t *testing.T) {
	root := t.TempDir()
	store := writeTestService(t, root, "the8020/demo/variables", "")
	if _, err := store.MutateState(context.Background(), "the8020/demo/variables", func(state *workspacepackages.DesiredServiceState) error {
		zero := 0
		state.Enabled = true
		state.Scaling.MinimumWorkers = &zero
		state.Placement.MinimumSandboxes = &zero
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
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	manager.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/the8020/demo/variables/value/6", nil))
	if response.Code != http.StatusOK || strings.TrimSpace(response.Body.String()) != "ok" {
		t.Fatalf("cold-start request status=%d body=%q", response.Code, response.Body.String())
	}
	status, err := manager.Inspect("the8020/demo/variables")
	if err != nil || status.State != StateReady || status.SandboxCount != 1 {
		t.Fatalf("cold-started status=%#v err=%v", status, err)
	}
	if err := manager.ReconcileAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	status, err = manager.Inspect("the8020/demo/variables")
	if err != nil || status.State != StateIdle || status.SandboxCount != 0 || status.WorkerCount != 0 {
		t.Fatalf("scaled-to-zero status=%#v err=%v", status, err)
	}
	response = httptest.NewRecorder()
	manager.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/the8020/demo/variables/value/7", nil))
	status, err = manager.Inspect("the8020/demo/variables")
	if err != nil || response.Code != http.StatusOK || status.State != StateReady || status.WorkerCount != 1 {
		t.Fatalf("restarted status=%#v response=%d err=%v", status, response.Code, err)
	}
}

func TestColdStartRollsBackFailedFirstWorkerAndRecoversOnRetry(t *testing.T) {
	root := t.TempDir()
	serviceID := "the8020/demo/variables"
	store := writeCanonicalTestService(t, root, serviceID, 0, 0, 0, 4, workspacepackages.ServiceTypeStateless)
	if _, err := store.MutateState(context.Background(), serviceID, func(state *workspacepackages.DesiredServiceState) error {
		state.Enabled = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	pools, router := newFakePools(), &fakeRouter{}
	manager := newTestManager(t, store, pools, router, filepath.Join(root, "node", "kernel", "services"))
	if status, err := manager.reconcileOne(context.Background(), serviceID); err != nil || status.State != StateIdle {
		t.Fatalf("initial status=%#v err=%v", status, err)
	}
	definition, err := store.ReadService(serviceID)
	if err != nil {
		t.Fatal(err)
	}
	poolID := versionPoolID(serviceID, definition.State.Generation, 0)
	pools.failScale[poolID] = errors.New("sandbox Worker capacity is exhausted")
	failed := httptest.NewRecorder()
	manager.ServeHTTP(failed, httptest.NewRequest(http.MethodGet, "/the8020/demo/variables/fail", nil))
	if failed.Code != http.StatusServiceUnavailable {
		t.Fatalf("failed cold start status=%d body=%q", failed.Code, failed.Body.String())
	}
	if _, err := pools.Inspect(poolID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed pool retained sandbox ownership: %v", err)
	}
	delete(pools.failScale, poolID)
	recovered := httptest.NewRecorder()
	manager.ServeHTTP(recovered, httptest.NewRequest(http.MethodGet, "/the8020/demo/variables/recover", nil))
	status, inspectErr := manager.Inspect(serviceID)
	if recovered.Code != http.StatusOK || inspectErr != nil || status.State != StateReady || status.WorkerCount != 1 {
		t.Fatalf("recovery status=%d service=%#v err=%v", recovered.Code, status, inspectErr)
	}
}

func TestTargetHeadroomFailureFallsBackToAvailableHardSlot(t *testing.T) {
	root := t.TempDir()
	serviceID := "the8020/demo/variables"
	store := writeCanonicalTestService(t, root, serviceID, 1, 2, 1, 2, workspacepackages.ServiceTypeStateless)
	pools, router := newFakePools(), &fakeRouter{}
	pools.dispatched = make(chan dispatchedRequest, 1)
	manager := newTestManager(t, store, pools, router, filepath.Join(root, "node", "kernel", "services"))
	started, err := manager.Start(context.Background(), serviceID)
	if err != nil {
		t.Fatal(err)
	}
	existing := started.Sandboxes[0]
	pools.capacityErrors[existing.PoolID] = &executionservices.SandboxCapacityError{
		Occupied: 0,
		Slots:    1,
		Reason:   "target-utilization growth failed: sandbox Worker capacity is exhausted",
	}
	newPoolID := versionPoolID(serviceID, started.DesiredVersion, 1)
	pools.failScale[newPoolID] = errors.New("sandbox Worker capacity is exhausted")

	response := httptest.NewRecorder()
	manager.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/the8020/demo/variables/value", nil))
	dispatched := <-pools.dispatched
	status, inspectErr := manager.Inspect(serviceID)
	if response.Code != http.StatusOK || dispatched.poolID != existing.PoolID || inspectErr != nil || status.WorkerCount != 1 || status.SandboxCount != 1 {
		t.Fatalf("response=%d dispatch=%#v status=%#v err=%v", response.Code, dispatched, status, inspectErr)
	}
	if _, err := pools.Inspect(newPoolID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed headroom sandbox was not rolled back: %v", err)
	}
}

func TestBackgroundReconciliationMaintainsWorkerAndWarmSandboxFloors(t *testing.T) {
	for _, test := range []struct {
		name             string
		minimumWorkers   int
		minimumSandboxes int
		wantState        State
		wantSandboxes    int
		wantWorkers      int
	}{
		{name: "Worker floor", minimumWorkers: 2, wantState: StateReady, wantSandboxes: 1, wantWorkers: 2},
		{name: "warm sandbox floor", minimumSandboxes: 2, wantState: StateIdle, wantSandboxes: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			store := writeCanonicalTestService(t, root, "the8020/demo/variables", test.minimumWorkers, 0, test.minimumSandboxes, 2, workspacepackages.ServiceTypeStateless)
			if _, err := store.MutateState(context.Background(), "the8020/demo/variables", func(state *workspacepackages.DesiredServiceState) error {
				state.Enabled = true
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			manager := newTestManager(t, store, newFakePools(), &fakeRouter{}, filepath.Join(root, "node", "kernel", "services"))
			manager.StartReconciler(context.Background())
			deadline := time.Now().Add(time.Second)
			for {
				status, err := manager.Inspect("the8020/demo/variables")
				if err == nil && status.State == test.wantState && status.SandboxCount == test.wantSandboxes && status.WorkerCount == test.wantWorkers {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("floor was not maintained: status=%#v err=%v", status, err)
				}
				time.Sleep(time.Millisecond)
			}
		})
	}
}

func TestMinimumWorkersSpillIntoAnotherSandboxWhenWorkerLimitBlocksPacking(t *testing.T) {
	root := t.TempDir()
	serviceID := "the8020/demo/variables"
	store := writeCanonicalTestService(t, root, serviceID, 2, 4, 0, 4, workspacepackages.ServiceTypeStateless)
	pools, router := newFakePools(), &fakeRouter{}
	firstPoolID := versionPoolID(serviceID, 1, 0)
	pools.failScale[firstPoolID] = fmt.Errorf("%w: Worker limit is reached", executionworkers.ErrSandboxCapacity)
	manager := newTestManager(t, store, pools, router, filepath.Join(root, "node", "kernel", "services"))

	status, err := manager.Start(context.Background(), serviceID)
	if err != nil || status.State != StateReady || status.WorkerCount != 2 || status.SandboxCount != 2 {
		t.Fatalf("status=%#v err=%v", status, err)
	}
	for _, sandbox := range status.Sandboxes {
		if len(sandbox.WorkerIDs) != 1 {
			t.Fatalf("minimum Workers were not isolated after Worker rejection: %#v", status.Sandboxes)
		}
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
	writeTestFile(t, filepath.Join(serviceRoot, "service.toml"), "schema = 2\nentrypoint = \"service.ts\"\n")
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
	if explicitlyDiscovered {
		t.Fatal("reconciliation discovered a service that package activation had not published")
	}
	if _, err := store.SynchronizePackageDefinitions(context.Background(), "the8020/demo", "test-commit-two"); err != nil {
		t.Fatal(err)
	}
	if err := manager.ReconcileAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	_, activated := manager.services["the8020/demo/static"]
	manager.mu.Unlock()
	if !activated {
		t.Fatal("reconciliation did not load the activated service declaration")
	}
}

func TestPackageActivationKeepsExistingVersionServingWithoutColdStarting(t *testing.T) {
	root := t.TempDir()
	store := writeTestService(t, root, "the8020/demo/variables", `[scaling]
minimum_workers = 1
`)
	pools, router := newFakePools(), &fakeRouter{}
	manager := newTestManager(t, store, pools, router, filepath.Join(root, "node", "kernel", "services"))
	status, err := manager.Start(context.Background(), "the8020/demo/variables")
	if err != nil || status.State != StateReady || status.WorkerCount != 1 {
		t.Fatalf("started status=%#v err=%v", status, err)
	}
	manifest := filepath.Join(root, "packages", "the8020", "demo", "services", "variables", "service.toml")
	if err := os.Rename(manifest, manifest+".unavailable"); err != nil {
		t.Fatal(err)
	}
	assertHTTPStatus(t, router.boundary, "/the8020/demo/variables/", http.StatusOK)

	db := database.New(database.Config{
		Backend: database.BackendSQLite, Location: filepath.Join(root, "database", "system.db"),
		MaximumOpenConnections: 2, MaximumIdleConnections: 1,
	})
	if _, err := db.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(context.Background(), `UPDATE "the8020__packages__packages" SET "state" = 'activating' WHERE "packageId" = $1`, "the8020/demo"); err != nil {
		t.Fatal(err)
	}

	if err := manager.ReconcileAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	status, err = manager.Inspect("the8020/demo/variables")
	if err != nil {
		t.Fatal(err)
	}
	if status.State != StateReady || status.WorkerCount != 1 || status.Metrics.StartupFailures != 0 {
		t.Fatalf("activation-gated status=%#v", status)
	}
	assertHTTPStatus(t, router.boundary, "/the8020/demo/variables/", http.StatusOK)

	if err := manager.Retire(context.Background(), "the8020/demo/variables"); err != nil {
		t.Fatal(err)
	}
	pools.mu.Lock()
	starts := countEventPrefix(pools.events, "start:")
	pools.mu.Unlock()
	assertHTTPStatus(t, router.boundary, "/the8020/demo/variables/", http.StatusServiceUnavailable)
	pools.mu.Lock()
	after := countEventPrefix(pools.events, "start:")
	pools.mu.Unlock()
	if after != starts {
		t.Fatalf("activation gate cold-started candidate code: starts before=%d after=%d", starts, after)
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
	if _, err := manager.retainFailedVersion("the8020/demo/static", 3, cause); !errors.Is(err, cause) {
		t.Fatal(err)
	}
	status, err := manager.retainFailedVersion("the8020/demo/static", 3, cause)
	if !errors.Is(err, cause) {
		t.Fatal(err)
	}
	if status.Metrics.StartupFailures != 1 {
		t.Fatalf("identical failure count = %d", status.Metrics.StartupFailures)
	}
	if count := strings.Count(logs.String(), "service version failed"); count != 1 {
		t.Fatalf("identical failure logs = %d: %s", count, logs.String())
	}
}

func TestRejectedServiceVersionDoesNotEnterCapacityRetryLoop(t *testing.T) {
	root := t.TempDir()
	store := writeTestService(t, root, "the8020/demo/variables", "")
	pools, router := newFakePools(), &fakeRouter{}
	manager := newTestManager(t, store, pools, router, filepath.Join(root, "node", "kernel", "services"))
	rejection := fmt.Errorf("%w: type check failed", executionservices.ErrInvalidServiceDefinition)
	pools.failVersion[1] = rejection

	status, err := manager.Start(context.Background(), "the8020/demo/variables")
	if !errors.Is(err, executionservices.ErrInvalidServiceDefinition) || status.State != StateFailed || status.FailedVersion != 1 || status.Metrics.StartupFailures != 1 {
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
		t.Fatalf("rejected version retried: before=%d after=%d", starts, afterMaintenanceAndRequest)
	}

	pools.failVersion[2] = rejection
	if retried, err := manager.Restart(context.Background(), "the8020/demo/variables"); !errors.Is(err, executionservices.ErrInvalidServiceDefinition) || retried.FailedVersion != 2 {
		t.Fatalf("explicit retry status=%#v err=%v", retried, err)
	}
	pools.mu.Lock()
	afterExplicitRestart := len(pools.events)
	pools.mu.Unlock()
	if afterExplicitRestart <= starts {
		t.Fatal("explicit service restart did not retry the rejected version")
	}
}

func TestColdStartUsesDegradedCapacityAndFillsMissingSandboxInPlace(t *testing.T) {
	root := t.TempDir()
	store := writeTestService(t, root, "the8020/demo/variables", `[scaling]
minimum_workers = 2
maximum_workers = 2
[placement]
minimum_sandboxes = 2
workers_per_sandbox = 1
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
	firstPoolID := versionPoolID(definition.Identity.ServiceID(), definition.State.Generation, 0)
	missingPoolID := versionPoolID(definition.Identity.ServiceID(), definition.State.Generation, 1)
	pools := newFakePools()
	pools.failStart[missingPoolID] = errors.New("node CPU capacity exhausted")
	manager := newTestManager(t, store, pools, &fakeRouter{}, filepath.Join(root, "node", "kernel", "node-a", "services"))

	response := httptest.NewRecorder()
	manager.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/the8020/demo/variables/value/6", nil))
	if response.Code != http.StatusOK || strings.TrimSpace(response.Body.String()) != "ok" {
		t.Fatalf("degraded cold start status=%d body=%q", response.Code, response.Body.String())
	}
	degraded, err := manager.Inspect("the8020/demo/variables")
	if err != nil || degraded.State != StateDegraded || len(degraded.Sandboxes) != 1 || degraded.Sandboxes[0].PoolID != firstPoolID {
		t.Fatalf("degraded=%#v err=%v", degraded, err)
	}
	if err := manager.ReconcileAll(context.Background()); err == nil || !strings.Contains(err.Error(), "CPU capacity") {
		t.Fatalf("missing sandbox retry error=%v", err)
	}
	for _, event := range pools.events {
		if event == "stop:"+firstPoolID {
			t.Fatalf("healthy degraded sandbox was stopped during retry: %#v", pools.events)
		}
	}

	delete(pools.failStart, missingPoolID)
	if err := manager.ReconcileAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	ready, err := manager.Inspect("the8020/demo/variables")
	if err != nil || ready.State != StateReady || len(ready.Sandboxes) != 2 || ready.Sandboxes[0].PoolID != firstPoolID || ready.Sandboxes[1].PoolID != missingPoolID {
		t.Fatalf("ready=%#v err=%v", ready, err)
	}
}

func TestReconcileChecksWorkerHealthWhenRecordedCountStillMatches(t *testing.T) {
	root := t.TempDir()
	store := writeTestService(t, root, "the8020/demo/variables", `[scaling]
minimum_workers = 2
maximum_workers = 2
[placement]
minimum_sandboxes = 1
workers_per_sandbox = 2
`)
	pools := newFakePools()
	manager := newTestManager(t, store, pools, &fakeRouter{}, filepath.Join(root, "node", "kernel", "node-a", "services"))
	started, err := manager.Start(context.Background(), "the8020/demo/variables")
	if err != nil {
		t.Fatal(err)
	}
	poolID := started.Sandboxes[0].PoolID
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
	if len(status.Sandboxes) != 1 || len(status.Sandboxes[0].WorkerIDs) != 2 || status.Sandboxes[0].WorkerIDs[0] == "stale-worker-a" {
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

func TestLifecyclePersistsVersionsRollsCapacityAndRetainsBrokenReplacement(t *testing.T) {
	root := t.TempDir()
	store := writeTestService(t, root, "the8020/demo/variables", `[scaling]
minimum_workers = 2
maximum_workers = 4
[placement]
minimum_sandboxes = 1
workers_per_sandbox = 4
`)
	pools, router := newFakePools(), &fakeRouter{}
	manager := newTestManager(t, store, pools, router, filepath.Join(root, "node", "kernel", "node-a", "services"))
	var logs bytes.Buffer
	manager.logger = slog.New(slog.NewJSONHandler(&logs, nil))

	started, err := manager.Start(context.Background(), "the8020/demo/variables")
	if err != nil {
		t.Fatal(err)
	}
	if started.State != StateReady || started.DesiredVersion != 1 || started.LoadedVersion != 1 || started.SandboxCount != 1 || started.WorkerCount != 2 {
		t.Fatalf("started = %#v", started)
	}
	state, exists, err := store.ReadState("the8020/demo/variables")
	if err != nil || !exists || !state.Enabled || state.Generation != 1 {
		t.Fatalf("state=%#v exists=%t err=%v", state, exists, err)
	}
	firstPool := started.Sandboxes[0].PoolID

	restarted, err := manager.Restart(context.Background(), "the8020/demo/variables")
	if err != nil {
		t.Fatal(err)
	}
	if restarted.LoadedVersion != 2 || restarted.Sandboxes[0].PoolID == firstPool {
		t.Fatalf("restarted = %#v", restarted)
	}
	assertEventBefore(t, pools.events, "start:"+restarted.Sandboxes[0].PoolID+":2", "stop:"+firstPool)

	pools.failVersion[3] = errors.New("entrypoint initialization failed")
	degraded, err := manager.Restart(context.Background(), "the8020/demo/variables")
	if err == nil || degraded.State != StateDegraded || degraded.LoadedVersion != 2 || degraded.DesiredVersion != 3 || degraded.FailedVersion != 3 {
		t.Fatalf("degraded=%#v err=%v", degraded, err)
	}
	if record, inspectErr := pools.Inspect(restarted.Sandboxes[0].PoolID); inspectErr != nil || record.State != "READY" {
		t.Fatalf("old capacity was not retained: record=%#v err=%v", record, inspectErr)
	}
	delete(pools.failVersion, 3)
	if err := manager.ReconcileAll(context.Background()); err != nil {
		t.Fatal(err)
	}

	minimumWorkers, maximumWorkers, concurrency, minimumSandboxes, workersPerSandbox := 6, 10, 8, 2, 5
	targetUtilization, workerKeepAlive, sessionKeepAlive, sandboxGroup := 0.65, "3m", "4m", "shared-proof"
	scaled, err := manager.Scale(context.Background(), "the8020/demo/variables", ScaleOptions{
		MinimumWorkers:       &minimumWorkers,
		MaximumWorkers:       &maximumWorkers,
		ConcurrencyPerWorker: &concurrency,
		TargetUtilization:    &targetUtilization,
		WorkerKeepAlive:      &workerKeepAlive,
		WorkersPerSandbox:    &workersPerSandbox,
		SandboxGroup:         &sandboxGroup,
		MinimumSandboxes:     &minimumSandboxes,
		SessionKeepAlive:     &sessionKeepAlive,
	})
	if err != nil {
		t.Fatal(err)
	}
	if scaled.LoadedVersion != 4 || scaled.SandboxCount != 2 || scaled.WorkerCount != 6 {
		t.Fatalf("scaled = %#v", scaled)
	}
	if scaled.Effective.Scaling.ConcurrencyPerWorker != 8 || scaled.Effective.Scaling.WorkerKeepAlive != 3*time.Minute || scaled.Effective.Lifecycle.SessionKeepAlive != 4*time.Minute || scaled.Effective.Scaling.TargetUtilization != 0.65 || scaled.Effective.Placement.SandboxGroup != "shared-proof" {
		t.Fatalf("scaled effective configuration = %#v", scaled.Effective)
	}
	stopped, err := manager.Stop(context.Background(), "the8020/demo/variables")
	if err != nil {
		t.Fatal(err)
	}
	if stopped.State != StateStopped || stopped.Enabled || stopped.DesiredVersion != 5 || stopped.SandboxCount != 0 {
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

func TestServiceStatusDistinguishesStartupRestartAndDrainFromObservedCapacity(t *testing.T) {
	root := t.TempDir()
	store := writeCanonicalTestService(t, root, "the8020/demo/variables", 1, 2, 0, 2, workspacepackages.ServiceTypeStateless)
	pools, router := newFakePools(), &fakeRouter{}
	manager := newTestManager(t, store, pools, router, filepath.Join(root, "node", "kernel", "services"))

	startEntered, startRelease := make(chan string, 1), make(chan struct{})
	pools.startEntered, pools.startRelease = startEntered, startRelease
	started := make(chan Status, 1)
	startErrors := make(chan error, 1)
	go func() {
		status, err := manager.Start(context.Background(), "the8020/demo/variables")
		started <- status
		startErrors <- err
	}()
	<-startEntered
	status, err := manager.Inspect("the8020/demo/variables")
	if err != nil || status.State != StateStarting || status.DesiredVersion != 1 || status.LoadedVersion != 0 || status.SandboxCount != 0 {
		t.Fatalf("starting status=%#v err=%v", status, err)
	}
	close(startRelease)
	status, err = <-started, <-startErrors
	if err != nil || status.State != StateReady || status.LoadedVersion != 1 || status.SandboxCount != 1 || status.WorkerCount != 1 {
		t.Fatalf("ready status=%#v err=%v", status, err)
	}

	pools.mu.Lock()
	restartEntered, restartRelease := make(chan string, 1), make(chan struct{})
	pools.startEntered, pools.startRelease = restartEntered, restartRelease
	pools.mu.Unlock()
	restarted := make(chan Status, 1)
	restartErrors := make(chan error, 1)
	go func() {
		status, err := manager.Restart(context.Background(), "the8020/demo/variables")
		restarted <- status
		restartErrors <- err
	}()
	<-restartEntered
	status, err = manager.Inspect("the8020/demo/variables")
	if err != nil || status.State != StateRestarting || status.DesiredVersion != 2 || status.LoadedVersion != 1 || status.SandboxCount != 1 || status.WorkerCount != 1 {
		t.Fatalf("restarting status=%#v err=%v", status, err)
	}
	close(restartRelease)
	status, err = <-restarted, <-restartErrors
	if err != nil || status.State != StateReady || status.LoadedVersion != 2 {
		t.Fatalf("restarted status=%#v err=%v", status, err)
	}

	pools.mu.Lock()
	stopEntered, stopRelease := make(chan string, 1), make(chan struct{})
	pools.startEntered, pools.startRelease = nil, nil
	pools.stopEntered, pools.stopRelease = stopEntered, stopRelease
	pools.mu.Unlock()
	stopped := make(chan Status, 1)
	stopErrors := make(chan error, 1)
	go func() {
		status, err := manager.Stop(context.Background(), "the8020/demo/variables")
		stopped <- status
		stopErrors <- err
	}()
	<-stopEntered
	status, err = manager.Inspect("the8020/demo/variables")
	if err != nil || status.State != StateDraining || status.Enabled || status.DesiredVersion != 3 || status.LoadedVersion != 2 || status.SandboxCount != 1 {
		t.Fatalf("draining status=%#v err=%v", status, err)
	}
	close(stopRelease)
	status, err = <-stopped, <-stopErrors
	if err != nil || status.State != StateStopped || status.Enabled || status.SandboxCount != 0 || status.WorkerCount != 0 {
		t.Fatalf("stopped status=%#v err=%v", status, err)
	}
}

func TestCanonicalBoundaryReadsDatabaseStateStripsPrefixAndUsesTrustedMetadata(t *testing.T) {
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
	request.Header.Set("X-80-20-Internal-Client-IP-Address", "198.51.100.99")
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
	if dispatched.header.Get("X-80-20-Internal-Client-Ip-Address") != "192.0.2.1" || dispatched.header.Get("X-80-20-Internal-Client-Network-Scope") != "public" {
		t.Fatalf("trusted client metadata = %#v", dispatched.header)
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

	if _, err := manager.Stop(context.Background(), "the8020/demo/variables"); err != nil {
		t.Fatal(err)
	}
	assertHTTPStatus(t, manager, "/the8020/demo/variables/path", http.StatusServiceUnavailable)
	if _, err := manager.Start(context.Background(), "the8020/demo/variables"); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(root, "packages", "the8020", "demo", "services", "variables", "service.toml"), "schema = [\n")
	assertHTTPStatus(t, manager, "/the8020/demo/variables/path", http.StatusTeapot)
	<-pools.dispatched
}

func TestClientNetworkScope(t *testing.T) {
	tests := map[string]string{
		"127.0.0.1":    "loopback",
		"::1":          "loopback",
		"172.17.0.1":   "private",
		"192.168.1.10": "private",
		"fe80::1":      "link_local",
		"0.0.0.0":      "special",
		"8.8.8.8":      "public",
	}
	for input, want := range tests {
		address, err := netip.ParseAddr(input)
		if err != nil {
			t.Fatal(err)
		}
		if got := clientNetworkScope(address); got != want {
			t.Errorf("clientNetworkScope(%q) = %q, want %q", input, got, want)
		}
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
		"X-80-20-Internal-Auth-Realm":         platformauth.UserRealm,
		"X-80-20-Internal-Auth-User-Id":       "user:Admin",
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
	writeTestFile(t, serviceManifest, `schema = 2
description = "Test service"
entrypoint = "service.ts"
[access]
mode = "authenticated"
[access.unauthenticated]
action = "redirect"
status = 307
redirect_url = "https://identity.example.test/login?return=fixed"
`)
	if _, err := store.SynchronizePackageDefinitions(context.Background(), "the8020/demo", "test-commit-two"); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Reconcile(context.Background(), "the8020/demo/protected"); err != nil {
		t.Fatal(err)
	}
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

func TestSessionServiceEstablishesHTTPRouteThenReconnectsWebSocketToExactSandbox(t *testing.T) {
	root := t.TempDir()
	store := writeTestService(t, root, "example/realtime/channel", persistentServiceManifest)
	pools, router := newFakePools(), &fakeRouter{}
	manager := newTestManager(t, store, pools, router, filepath.Join(root, "node", "kernel", "services"))
	manager.authentication = &fakeAuthentication{}
	status, err := manager.Start(context.Background(), "example/realtime/channel")
	if err != nil {
		t.Fatal(err)
	}
	if status.ServiceType != workspacepackages.ServiceTypeSession || status.SandboxCount != 2 || status.WorkerCount != 2 {
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
	staleToken, _, err := manager.persistentRoutes.create("example/realtime/channel", initial.poolID, poolRecord.RuntimeGroupID, poolRecord.SandboxID, "user:Admin", 2*time.Minute, false)
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
	if _, err := manager.persistentRoutes.lookup(staleToken, "example/realtime/channel", "user:Admin"); !errors.Is(err, errRouteNotFound) {
		t.Fatalf("stale route remained registered: %v", err)
	}
}

func TestObservedSandboxesCountUniqueResourcesAndVersions(t *testing.T) {
	current := &runtimeSandbox{status: ServiceSandboxStatus{Version: 2, SandboxID: "sandbox-current", WorkerIDs: []string{"worker-a", "worker-b"}}}
	duplicate := &runtimeSandbox{status: ServiceSandboxStatus{Version: 2, SandboxID: "sandbox-current", WorkerIDs: []string{"worker-b"}}}
	retained := &runtimeSandbox{status: ServiceSandboxStatus{Version: 1, SandboxID: "sandbox-retained", WorkerIDs: []string{"worker-c"}}}

	sandboxes, workers, versions := observedSandboxes([]*runtimeSandbox{current, duplicate, retained}, Status{LoadedVersion: 2, VersionCount: 1})
	if len(sandboxes) != 2 || workers != 3 || versions != 2 {
		t.Fatalf("observed sandboxes=%#v workers=%d versions=%d", sandboxes, workers, versions)
	}
	if sandboxes[0].Version != 2 || len(sandboxes[0].WorkerIDs) != 2 || sandboxes[1].Version != 1 {
		t.Fatalf("ordered versioned sandboxes = %#v", sandboxes)
	}
}

func TestReloadRoutesOnlyToCurrentGenerationWhileOldPoolDrains(t *testing.T) {
	root := t.TempDir()
	store := writeTestService(t, root, "example/realtime/channel", persistentServiceManifest)
	pools, router := newFakePools(), &fakeRouter{}
	manager := newTestManager(t, store, pools, router, filepath.Join(root, "node", "kernel", "services"))
	manager.authentication = &fakeAuthentication{}
	initial, err := manager.Start(context.Background(), "example/realtime/channel")
	if err != nil {
		t.Fatal(err)
	}
	oldPool := initial.Sandboxes[0].PoolID
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
	pools.occupiedSlots[oldPool] = 1

	reloaded, err := manager.Reload(context.Background(), "example/realtime/channel")
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.LoadedVersion == initial.LoadedVersion || reloaded.Sandboxes[0].PoolID == oldPool {
		t.Fatalf("reload did not switch version: initial=%#v reloaded=%#v", initial, reloaded)
	}
	versions := map[uint64]bool{}
	foundOldPool := false
	for _, sandbox := range reloaded.Sandboxes {
		versions[sandbox.Version] = true
		foundOldPool = foundOldPool || sandbox.PoolID == oldPool
	}
	if reloaded.VersionCount != 2 || !versions[initial.LoadedVersion] || !versions[reloaded.LoadedVersion] || !foundOldPool {
		t.Fatalf("running versions are not fully observable: %#v", reloaded)
	}
	listed, err := manager.List()
	if err != nil || len(listed) != 1 || listed[0].VersionCount != 2 || listed[0].SandboxCount != reloaded.SandboxCount || listed[0].WorkerCount != reloaded.WorkerCount {
		t.Fatalf("logical service list did not aggregate versions: %#v err=%v", listed, err)
	}
	if pools.records[oldPool].State != "DRAINING" {
		t.Fatalf("old occupied pool was not draining: %#v", pools.records[oldPool])
	}

	resume := websocketRequest("/example/realtime/channel/connect?route="+route, "")
	resume.AddCookie(&http.Cookie{Name: "the8020_auth", Value: "valid-opaque-cookie"})
	response := httptest.NewRecorder()
	manager.ServeHTTP(response, resume)
	if response.Code != http.StatusConflict || len(pools.websockets) != 0 {
		t.Fatalf("old route status=%d proxies=%#v", response.Code, pools.websockets)
	}

	pools.dispatched = make(chan dispatchedRequest, 1)
	currentRequest := httptest.NewRequest(http.MethodPost, "/example/realtime/channel/connect", nil)
	currentRequest.AddCookie(&http.Cookie{Name: "the8020_auth", Value: "valid-opaque-cookie"})
	manager.ServeHTTP(httptest.NewRecorder(), currentRequest)
	if dispatched := <-pools.dispatched; dispatched.poolID == oldPool {
		t.Fatalf("new request reached draining pool %s", oldPool)
	}

	pools.occupiedSlots[oldPool] = 0
	if err := manager.reconcileMaintained(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, exists := pools.records[oldPool]; exists {
		t.Fatalf("empty draining version record was not removed: %#v", pools.records[oldPool])
	}
	settled, err := manager.Inspect("example/realtime/channel")
	if err != nil || settled.VersionCount != 1 {
		t.Fatalf("settled version status=%#v err=%v", settled, err)
	}
	for _, sandbox := range settled.Sandboxes {
		if sandbox.PoolID == oldPool {
			t.Fatalf("retired pool remains visible after removal: %#v", settled)
		}
	}
}

func TestRequestMetricsRemainLogicalAcrossVersionReplacement(t *testing.T) {
	root := t.TempDir()
	serviceID := "the8020/demo/variables"
	store := writeTestService(t, root, serviceID, `[scaling]
minimum_workers = 1
maximum_workers = 2
concurrency_per_worker = 1
[placement]
minimum_sandboxes = 1
workers_per_sandbox = 1
`)
	pools, router := newFakePools(), &fakeRouter{}
	pools.dispatched = make(chan dispatchedRequest, 1)
	pools.release = make(chan struct{})
	manager := newTestManager(t, store, pools, router, filepath.Join(root, "node", "kernel", "services"))
	initial, err := manager.Start(context.Background(), serviceID)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		manager.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/the8020/demo/variables/slow", nil))
		close(done)
	}()
	oldPool := (<-pools.dispatched).poolID
	pools.mu.Lock()
	pools.occupiedSlots[oldPool] = 1
	pools.mu.Unlock()

	reloaded, err := manager.Reload(context.Background(), serviceID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.LoadedVersion == initial.LoadedVersion || reloaded.VersionCount != 2 || reloaded.Metrics.ActiveRequests != 1 {
		t.Fatalf("replacement status=%#v", reloaded)
	}
	close(pools.release)
	<-done
	pools.mu.Lock()
	pools.occupiedSlots[oldPool] = 0
	pools.mu.Unlock()
	if err := manager.reconcileMaintained(context.Background()); err != nil {
		t.Fatal(err)
	}
	settled, err := manager.Inspect(serviceID)
	if err != nil || settled.VersionCount != 1 || settled.Metrics.ActiveRequests != 0 || settled.Metrics.RequestCount != 1 {
		t.Fatalf("settled logical metrics=%#v err=%v", settled, err)
	}
}

func TestPersistentRouteReceivedByAnotherNodeForwardsToOwner(t *testing.T) {
	root := t.TempDir()
	store := writeTestService(t, root, "example/realtime/channel", persistentServiceManifest)
	db := newTestRouteDatabase(t, root)
	owner := newPersistentRouteRegistry("node-a", db)
	token, _, err := owner.create("example/realtime/channel", "remote-pool", "remote-group", "remote-sandbox", "user:Admin", 2*time.Minute, false)
	if err != nil {
		t.Fatal(err)
	}
	pools, router := newFakePools(), &fakeRouter{}
	nodeRouter := &fakeNodeRouter{local: "node-b"}
	manager, err := New(Config{Definitions: store, Pools: pools, Router: router, ObservedRoot: filepath.Join(root, "node", "kernel", "services"), NodeID: "node-b", Database: db, Nodes: nodeRouter})
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
	if first.Sandboxes[0].RuntimeGroupID == second.Sandboxes[0].RuntimeGroupID {
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
	if first.Sandboxes[0].RuntimeGroupID != second.Sandboxes[0].RuntimeGroupID || first.Sandboxes[0].PoolID == second.Sandboxes[0].PoolID {
		t.Fatalf("first=%#v second=%#v", first.Sandboxes, second.Sandboxes)
	}
	for _, serviceID := range []string{first.ServiceID, second.ServiceID} {
		response := httptest.NewRecorder()
		identity := mustIdentity(t, serviceID)
		manager.ServeHTTP(response, httptest.NewRequest(http.MethodGet, identity.CanonicalBasePath()+"/value", nil))
		if response.Code != http.StatusOK || (<-pools.dispatched).poolID != manager.services[serviceID].sandboxes[0].status.PoolID {
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
	if status, err := manager.Inspect(second.ServiceID); err != nil || status.State != StateReady || status.SandboxCount != 1 {
		t.Fatalf("sibling after stop=%#v err=%v", status, err)
	}
}

func TestLeastInFlightSelectionAndTimeout(t *testing.T) {
	root := t.TempDir()
	store := writeTestService(t, root, "the8020/demo/variables", `[scaling]
minimum_workers = 2
maximum_workers = 8
[placement]
minimum_sandboxes = 2
workers_per_sandbox = 4
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
		if first.poolID != started.Sandboxes[index].PoolID {
			t.Fatalf("dispatch %d selected %s, want %s", index, first.poolID, started.Sandboxes[index].PoolID)
		}
	}
	status, err := manager.Inspect("the8020/demo/variables")
	if err != nil || status.Metrics.ActiveRequests != 2 || status.Sandboxes[0].ActiveRequests != 0 || status.Sandboxes[1].ActiveRequests != 0 {
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
		manager.dispatch(timeoutResponse, timeoutRequest, mustIdentity(t, "the8020/demo/variables"), "/timeout", runtime, runtime.sandboxes[0], time.Millisecond, platformauth.AuthContext{}, nil)
		close(result)
	}()
	<-pools.dispatched
	<-result
	if timeoutResponse.Code != http.StatusGatewayTimeout {
		t.Fatalf("timeout status = %d", timeoutResponse.Code)
	}
}

func TestConcurrentRequestsFeedReservedDemandIntoWorkerScaling(t *testing.T) {
	root := t.TempDir()
	store := writeCanonicalTestService(t, root, "the8020/demo/variables", 1, 4, 1, 4, workspacepackages.ServiceTypeStateless)
	pools, router := newFakePools(), &fakeRouter{}
	pools.dispatched = make(chan dispatchedRequest, 2)
	pools.release = make(chan struct{})
	manager := newTestManager(t, store, pools, router, filepath.Join(root, "node", "kernel", "services"))
	started, err := manager.Start(context.Background(), "the8020/demo/variables")
	if err != nil {
		t.Fatal(err)
	}
	poolID := started.Sandboxes[0].PoolID
	done := make(chan struct{}, 2)
	for range 2 {
		go func() {
			manager.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/the8020/demo/variables/wait", nil))
			done <- struct{}{}
		}()
		<-pools.dispatched
	}
	pools.mu.Lock()
	floors := append([]int(nil), pools.occupiedFloors[poolID]...)
	pools.mu.Unlock()
	if len(floors) == 0 || floors[len(floors)-1] < 1 {
		t.Fatalf("occupied-slot floors=%#v, want a reservation-backed demand floor", floors)
	}
	close(pools.release)
	<-done
	<-done
}

func TestHealthyWarmRequestUsesCachedCapacityWithoutReconciliation(t *testing.T) {
	root := t.TempDir()
	store := writeCanonicalTestService(t, root, "the8020/demo/variables", 1, 1, 1, 1, workspacepackages.ServiceTypeStateless)
	pools, router := newFakePools(), &fakeRouter{}
	manager := newTestManager(t, store, pools, router, filepath.Join(root, "node", "kernel", "services"))
	status, err := manager.Start(context.Background(), "the8020/demo/variables")
	if err != nil || len(status.Sandboxes) != 1 {
		t.Fatalf("start=%#v err=%v", status, err)
	}
	poolID := status.Sandboxes[0].PoolID
	pools.mu.Lock()
	pools.capacityCalls[poolID], pools.ensureCalls[poolID] = 0, 0
	pools.events = nil
	pools.mu.Unlock()

	manifest := filepath.Join(root, "packages", "the8020", "demo", "services", "variables", "service.toml")
	if err := os.Rename(manifest, manifest+".offline"); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	manager.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/the8020/demo/variables/warm", nil))
	pools.mu.Lock()
	capacityCalls, ensureCalls := pools.capacityCalls[poolID], pools.ensureCalls[poolID]
	events := append([]string(nil), pools.events...)
	pools.mu.Unlock()
	if response.Code != http.StatusOK || capacityCalls != 1 || ensureCalls != 0 || len(events) != 0 {
		t.Fatalf("warm response=%d capacity=%d ensure=%d lifecycle=%#v", response.Code, capacityCalls, ensureCalls, events)
	}
}

func TestUnrelatedWarmServiceDispatchesDoNotSerialize(t *testing.T) {
	root := t.TempDir()
	writeTestService(t, root, "the8020/demo/variables", "")
	store := writeTestService(t, root, "the8020/demo/variables-import", "")
	pools, router := newFakePools(), &fakeRouter{}
	manager := newTestManager(t, store, pools, router, filepath.Join(root, "node", "kernel", "services"))
	for _, serviceID := range []string{"the8020/demo/variables", "the8020/demo/variables-import"} {
		if _, err := manager.Start(context.Background(), serviceID); err != nil {
			t.Fatal(err)
		}
	}
	pools.mu.Lock()
	pools.dispatchEntered = make(chan struct{}, 2)
	pools.release = make(chan struct{})
	entered, release := pools.dispatchEntered, pools.release
	pools.mu.Unlock()

	done := make(chan int, 2)
	request := func(path string) {
		response := httptest.NewRecorder()
		manager.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		done <- response.Code
	}
	go request("/the8020/demo/variables/slow")
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("first service did not reach dispatch")
	}
	go request("/the8020/demo/variables-import/fast")
	select {
	case <-entered:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("unrelated service serialized behind the first dispatch")
	}
	close(release)
	for range 2 {
		if status := <-done; status != http.StatusOK {
			t.Fatalf("dispatch status = %d", status)
		}
	}
}

func TestUnrelatedServiceReconciliationDoesNotSerialize(t *testing.T) {
	root := t.TempDir()
	writeTestService(t, root, "the8020/demo/variables", "")
	store := writeTestService(t, root, "the8020/demo/variables-import", "")
	pools, router := newFakePools(), &fakeRouter{}
	pools.startEntered = make(chan string, 2)
	pools.startRelease = make(chan struct{})
	manager := newTestManager(t, store, pools, router, filepath.Join(root, "node", "kernel", "services"))

	type result struct {
		serviceID string
		err       error
	}
	results := make(chan result, 2)
	start := func(serviceID string) {
		_, err := manager.Start(context.Background(), serviceID)
		results <- result{serviceID: serviceID, err: err}
	}
	go start("the8020/demo/variables")
	first := <-pools.startEntered
	go start("the8020/demo/variables-import")
	select {
	case second := <-pools.startEntered:
		if first == second {
			t.Fatalf("both startups entered the same service: %s", first)
		}
	case <-time.After(time.Second):
		close(pools.startRelease)
		t.Fatal("unrelated service reconciliation serialized behind startup I/O")
	}
	close(pools.startRelease)
	for range 2 {
		if result := <-results; result.err != nil {
			t.Fatalf("start %s: %v", result.serviceID, result.err)
		}
	}
}

func TestServiceMaintenanceQueueIsDeduplicatedAndBounded(t *testing.T) {
	manager := &Manager{maintenanceSet: map[string]bool{}}
	total := maximumServiceMaintenancePerPass + 7
	for index := 0; index < total; index++ {
		serviceID := fmt.Sprintf("example/catalog/service-%d", index)
		manager.scheduleMaintenance(serviceID)
		manager.scheduleMaintenance(serviceID)
	}
	first := manager.takeMaintenance(maximumServiceMaintenancePerPass)
	second := manager.takeMaintenance(maximumServiceMaintenancePerPass)
	if len(first) != maximumServiceMaintenancePerPass || len(second) != total-maximumServiceMaintenancePerPass {
		t.Fatalf("maintenance batches=%d,%d want=%d,%d", len(first), len(second), maximumServiceMaintenancePerPass, total-maximumServiceMaintenancePerPass)
	}
	if remaining := manager.takeMaintenance(maximumServiceMaintenancePerPass); len(remaining) != 0 {
		t.Fatalf("maintenance queue retained duplicates: %#v", remaining)
	}
}

func TestAbandonedDispatchReservationExpires(t *testing.T) {
	root := t.TempDir()
	store := writeCanonicalTestService(t, root, "the8020/demo/variables", 1, 1, 1, 1, workspacepackages.ServiceTypeStateless)
	pools, router := newFakePools(), &fakeRouter{}
	manager := newTestManager(t, store, pools, router, filepath.Join(root, "node", "kernel", "services"))
	if _, err := manager.Start(context.Background(), "the8020/demo/variables"); err != nil {
		t.Fatal(err)
	}
	definition, err := store.ReadService("the8020/demo/variables")
	if err != nil {
		t.Fatal(err)
	}
	runtime := manager.services["the8020/demo/variables"]
	selected, _ := manager.reserveCachedCapacity(context.Background(), runtime, definition)
	if selected == nil {
		t.Fatal("first strict reservation was not admitted")
	}
	if second, _ := manager.reserveCachedCapacity(context.Background(), runtime, definition); second != nil {
		t.Fatal("strict concurrency admitted a second live reservation")
	}
	manager.mu.Lock()
	selected.reservations[0] = time.Now().Add(-time.Second)
	manager.mu.Unlock()
	if recovered, _ := manager.reserveCachedCapacity(context.Background(), runtime, definition); recovered != selected {
		t.Fatal("expired reservation continued to block warm dispatch")
	}
	manager.finishRequest(runtime, selected, 0, 0, 0, false)
	manager.finishRequest(runtime, selected, 0, 0, 0, false)
}

func TestDispatchFailureReleasesReservation(t *testing.T) {
	root := t.TempDir()
	store := writeCanonicalTestService(t, root, "the8020/demo/variables", 1, 1, 1, 1, workspacepackages.ServiceTypeStateless)
	pools, router := newFakePools(), &fakeRouter{}
	manager := newTestManager(t, store, pools, router, filepath.Join(root, "node", "kernel", "services"))
	status, err := manager.Start(context.Background(), "the8020/demo/variables")
	if err != nil {
		t.Fatal(err)
	}
	poolID := status.Sandboxes[0].PoolID
	pools.mu.Lock()
	failed := pools.records[poolID]
	failed.State = "FAILED"
	pools.records[poolID] = failed
	pools.mu.Unlock()

	response := httptest.NewRecorder()
	manager.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/the8020/demo/variables/fail", nil))
	manager.mu.Lock()
	reservations := activeReservations(manager.services["the8020/demo/variables"].sandboxes[0], time.Now())
	active := manager.services["the8020/demo/variables"].status.Metrics.ActiveRequests
	manager.mu.Unlock()
	if response.Code != http.StatusBadGateway || reservations != 0 || active != 0 {
		t.Fatalf("failed dispatch status=%d reservations=%d active=%d", response.Code, reservations, active)
	}
}

func TestHigherConcurrencyAllowsOnlyOneTemporaryExtraPerWorker(t *testing.T) {
	root := t.TempDir()
	store := writeTestService(t, root, "the8020/demo/variables", `[scaling]
minimum_workers = 1
maximum_workers = 1
concurrency_per_worker = 2
target_utilization = 0.7
[placement]
minimum_sandboxes = 1
workers_per_sandbox = 1
`)
	pools, router := newFakePools(), &fakeRouter{}
	manager := newTestManager(t, store, pools, router, filepath.Join(root, "node", "kernel", "services"))
	if _, err := manager.Start(context.Background(), "the8020/demo/variables"); err != nil {
		t.Fatal(err)
	}
	definition, err := store.ReadService("the8020/demo/variables")
	if err != nil {
		t.Fatal(err)
	}
	runtime := manager.services["the8020/demo/variables"]
	for request := 1; request <= 3; request++ {
		if sandbox, _ := manager.reserveCachedCapacity(context.Background(), runtime, definition); sandbox == nil {
			t.Fatalf("reservation %d was rejected before the bounded overload allowance", request)
		}
	}
	if sandbox, _ := manager.reserveCachedCapacity(context.Background(), runtime, definition); sandbox != nil {
		t.Fatal("routing exceeded the one-extra-request-per-Worker allowance")
	}
	for range 3 {
		manager.finishRequest(runtime, runtime.sandboxes[0], 0, 0, 0, false)
	}
}

func TestTargetCapacityAddsSandboxAfterWorkersReachPackingLimit(t *testing.T) {
	root := t.TempDir()
	store := writeTestService(t, root, "the8020/demo/variables", `[scaling]
minimum_workers = 1
maximum_workers = 2
concurrency_per_worker = 1
target_utilization = 0.7
[placement]
minimum_sandboxes = 1
workers_per_sandbox = 1
`)
	pools, router := newFakePools(), &fakeRouter{}
	pools.dispatched = make(chan dispatchedRequest, 1)
	manager := newTestManager(t, store, pools, router, filepath.Join(root, "node", "kernel", "services"))
	started, err := manager.Start(context.Background(), "the8020/demo/variables")
	if err != nil {
		t.Fatal(err)
	}
	pools.mu.Lock()
	pools.capacityErrors[started.Sandboxes[0].PoolID] = &executionservices.SandboxCapacityError{Occupied: 1, Slots: 1, Strict: true, Reason: "all Worker slots are occupied"}
	pools.mu.Unlock()
	response := httptest.NewRecorder()
	manager.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/the8020/demo/variables/value", nil))
	request := <-pools.dispatched
	status, inspectErr := manager.Inspect("the8020/demo/variables")
	if inspectErr != nil || response.Code != http.StatusOK || status.SandboxCount != 2 || request.poolID == started.Sandboxes[0].PoolID {
		t.Fatalf("response=%d request=%#v status=%#v err=%v", response.Code, request, status, inspectErr)
	}
}

func TestFiniteMaximumWorkersPreventsAdditionalSandboxCapacity(t *testing.T) {
	root := t.TempDir()
	store := writeCanonicalTestService(t, root, "the8020/demo/variables", 0, 2, 0, 1, workspacepackages.ServiceTypeStateless)
	pools, router := newFakePools(), &fakeRouter{}
	manager := newTestManager(t, store, pools, router, filepath.Join(root, "node", "kernel", "services"))
	if _, err := manager.Start(context.Background(), "the8020/demo/variables"); err != nil {
		t.Fatal(err)
	}

	first := httptest.NewRecorder()
	manager.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/the8020/demo/variables/one", nil))
	status, err := manager.Inspect("the8020/demo/variables")
	if err != nil || first.Code != http.StatusOK || status.WorkerCount != 1 {
		t.Fatalf("first request=%d status=%#v err=%v", first.Code, status, err)
	}
	pools.mu.Lock()
	pools.capacityErrors[status.Sandboxes[0].PoolID] = &executionservices.SandboxCapacityError{Occupied: 1, Slots: 1, Strict: true, Reason: "occupied"}
	pools.mu.Unlock()

	second := httptest.NewRecorder()
	manager.ServeHTTP(second, httptest.NewRequest(http.MethodGet, "/the8020/demo/variables/two", nil))
	status, err = manager.Inspect("the8020/demo/variables")
	if err != nil || second.Code != http.StatusOK || status.WorkerCount != 2 || status.SandboxCount != 2 {
		t.Fatalf("second request=%d status=%#v err=%v", second.Code, status, err)
	}
	pools.mu.Lock()
	for _, sandbox := range status.Sandboxes {
		if pools.options[sandbox.PoolID].GroupKey != "the8020/demo/variables" {
			pools.mu.Unlock()
			t.Fatalf("sandbox %s did not retain its dedicated group", sandbox.PoolID)
		}
		pools.capacityErrors[sandbox.PoolID] = &executionservices.SandboxCapacityError{Occupied: 1, Slots: 1, Strict: true, Reason: "occupied"}
	}
	pools.mu.Unlock()

	third := httptest.NewRecorder()
	manager.ServeHTTP(third, httptest.NewRequest(http.MethodGet, "/the8020/demo/variables/three", nil))
	status, err = manager.Inspect("the8020/demo/variables")
	if err != nil || third.Code != http.StatusServiceUnavailable || status.WorkerCount != 2 || status.SandboxCount != 2 {
		t.Fatalf("maximum response=%d status=%#v err=%v", third.Code, status, err)
	}
}

func TestEmptySandboxAboveMinimumIsRemovedAfterWorkerScaleDown(t *testing.T) {
	root := t.TempDir()
	store := writeTestService(t, root, "the8020/demo/variables", `[scaling]
minimum_workers = 1
maximum_workers = 2
concurrency_per_worker = 1
target_utilization = 0.7
[placement]
minimum_sandboxes = 1
workers_per_sandbox = 1
`)
	pools, router := newFakePools(), &fakeRouter{}
	manager := newTestManager(t, store, pools, router, filepath.Join(root, "node", "kernel", "services"))
	started, err := manager.Start(context.Background(), "the8020/demo/variables")
	if err != nil {
		t.Fatal(err)
	}
	pools.mu.Lock()
	pools.capacityErrors[started.Sandboxes[0].PoolID] = &executionservices.SandboxCapacityError{Occupied: 1, Slots: 1, Strict: true, Reason: "all Worker slots are occupied"}
	pools.mu.Unlock()
	runtime := manager.services["the8020/demo/variables"]
	definition, err := store.ReadService("the8020/demo/variables")
	if err != nil {
		t.Fatal(err)
	}
	added, err := manager.addCapacitySandbox(context.Background(), runtime, definition)
	if err != nil {
		t.Fatal(err)
	}
	pools.mu.Lock()
	delete(pools.capacityErrors, started.Sandboxes[0].PoolID)
	pools.occupiedSlots[started.Sandboxes[0].PoolID] = 1
	pools.occupiedSlots[added.status.PoolID] = 0
	pools.mu.Unlock()
	if err := manager.ReconcileAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	status, err := manager.Inspect("the8020/demo/variables")
	if err != nil || status.SandboxCount != 1 || status.Sandboxes[0].PoolID != started.Sandboxes[0].PoolID {
		t.Fatalf("status=%#v err=%v", status, err)
	}
	if _, err := pools.Inspect(added.status.PoolID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("retired sandbox pool record still exists: %v", err)
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

func TestTwoNodesAutomaticallyObserveSharedDesiredStateRevisions(t *testing.T) {
	root := t.TempDir()
	storeA := writeTestService(t, root, "the8020/demo/variables", `[scaling]
minimum_workers = 0
maximum_workers = 4
concurrency_per_worker = 32
target_utilization = 0.7
worker_keep_alive = "2m"
[placement]
minimum_sandboxes = 0
workers_per_sandbox = 4
`)
	storeB := newTestWorkspaceStore(t, root)
	poolsA, poolsB := newFakePools(), newFakePools()
	managerA := newTestManager(t, storeA, poolsA, &fakeRouter{}, filepath.Join(root, "node-a", "kernel", "runtime", "services"))
	managerB := newTestManager(t, storeB, poolsB, &fakeRouter{}, filepath.Join(root, "node-b", "kernel", "runtime", "services"))
	followerA, err := workspacepackages.NewServiceRevisionFollower(context.Background(), storeA)
	if err != nil {
		t.Fatal(err)
	}
	followerB, err := workspacepackages.NewServiceRevisionFollower(context.Background(), storeB)
	if err != nil {
		t.Fatal(err)
	}
	watchContext, stopWatching := context.WithCancel(context.Background())
	defer stopWatching()
	errorsA := observeServiceRevisions(watchContext, followerA, managerA)
	errorsB := observeServiceRevisions(watchContext, followerB, managerB)

	validation := managerA.Validate(context.Background(), "the8020/demo/variables")
	if !validation.Valid || validation.OpenAPI["openapi"] != "3.1.0" {
		t.Fatalf("validation = %#v", validation)
	}
	if len(poolsA.records) != 0 {
		t.Fatalf("validation pool record leaked: %#v", poolsA.records)
	}
	if state, exists, err := storeB.ReadState("the8020/demo/variables"); err != nil || !exists || state.Generation != 0 {
		t.Fatalf("activated database state=%#v exists=%t err=%v", state, exists, err)
	}
	if _, err := managerA.Start(context.Background(), "the8020/demo/variables"); err != nil {
		t.Fatal(err)
	}
	waitForServiceStatus(t, managerB, func(status Status) bool {
		return status.State == StateIdle && status.LoadedVersion == 1 && status.SandboxCount == 0 && status.WorkerCount == 0
	})
	minimumWorkers := 2
	if status, err := managerA.Scale(context.Background(), "the8020/demo/variables", ScaleOptions{MinimumWorkers: &minimumWorkers}); err != nil || status.LoadedVersion != 2 || status.WorkerCount != 2 {
		t.Fatalf("node A scale=%#v err=%v", status, err)
	}
	waitForServiceStatus(t, managerB, func(status Status) bool {
		return status.State == StateReady && status.LoadedVersion == 2 && status.WorkerCount == 2
	})
	if status, err := managerB.Restart(context.Background(), "the8020/demo/variables"); err != nil || status.LoadedVersion != 3 {
		t.Fatalf("node B restart=%#v err=%v", status, err)
	}
	waitForServiceStatus(t, managerA, func(status Status) bool { return status.LoadedVersion == 3 })
	if status, err := managerA.Stop(context.Background(), "the8020/demo/variables"); err != nil || status.State != StateStopped {
		t.Fatalf("node A stop=%#v err=%v", status, err)
	}
	waitForServiceStatus(t, managerB, func(status Status) bool {
		return status.State == StateStopped && status.DesiredVersion == 4
	})
	for name, failures := range map[string]<-chan error{"node-a": errorsA, "node-b": errorsB} {
		select {
		case err := <-failures:
			t.Fatalf("%s revision observer: %v", name, err)
		default:
		}
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

func observeServiceRevisions(ctx context.Context, follower *workspacepackages.ServiceRevisionFollower, manager *Manager) <-chan error {
	failures := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				update, err := follower.Poll(ctx)
				if err != nil {
					failures <- err
					return
				}
				if update.Revision == 0 {
					continue
				}
				for _, serviceID := range update.RetireServices {
					if err := manager.Retire(ctx, serviceID); err != nil {
						failures <- err
						return
					}
				}
				for _, serviceID := range update.ReconcileServices {
					if _, err := manager.Reconcile(ctx, serviceID); err != nil {
						failures <- err
						return
					}
				}
				if err := follower.Acknowledge(update.Revision); err != nil {
					failures <- err
					return
				}
			}
		}
	}()
	return failures
}

func waitForServiceStatus(t *testing.T, manager *Manager, ready func(Status) bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		status, err := manager.Inspect("the8020/demo/variables")
		if err == nil && ready(status) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("service state did not converge: status=%#v err=%v", status, err)
		}
		time.Sleep(time.Millisecond)
	}
}

func newTestManager(t testing.TB, store *workspacepackages.Store, pools *fakePools, router *fakeRouter, observed string) *Manager {
	t.Helper()
	manager, err := New(Config{Definitions: store, Pools: pools, Router: router, ObservedRoot: observed, ReconcileInterval: 10 * time.Millisecond, StartupTimeout: time.Second, Database: newTestRouteDatabase(t, t.TempDir())})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	return manager
}

func writeTestService(t testing.TB, root, serviceID, defaults string) *workspacepackages.Store {
	t.Helper()
	identity, err := workspacepackages.ParseServiceID(serviceID)
	if err != nil {
		t.Fatal(err)
	}
	packageRoot := filepath.Join(root, "packages", identity.Namespace, identity.Repository)
	serviceRoot := filepath.Join(packageRoot, "services", identity.Service)
	writeTestFile(t, filepath.Join(packageRoot, "package.toml"), "schema = 1\ndescription = \"Test package\"\n")
	manifest := "schema = 2\ndescription = \"Test service\"\nentrypoint = \"service.ts\"\n" + defaults
	if !strings.Contains(defaults, "[lifecycle]") {
		manifest += "\n[lifecycle]\nservice_type = \"stateless\"\nsession_keep_alive = \"10m\"\n"
	}
	if !strings.Contains(defaults, "[scaling]") {
		manifest += "\n[scaling]\nminimum_workers = 1\nmaximum_workers = 4\nconcurrency_per_worker = 32\ntarget_utilization = 0.7\nworker_keep_alive = \"2m\"\n"
	}
	if !strings.Contains(defaults, "[placement]") {
		manifest += "\n[placement]\nminimum_sandboxes = 1\nworkers_per_sandbox = 4\n"
	}
	writeTestFile(t, filepath.Join(serviceRoot, "service.toml"), manifest)
	writeTestFile(t, filepath.Join(serviceRoot, "service.ts"), "export default {};\n")
	store := newTestWorkspaceStore(t, root)
	if _, err := store.SynchronizePackageDefinitions(context.Background(), identity.PackageID(), "test-commit"); err != nil {
		t.Fatal(err)
	}
	return store
}

func writeCanonicalTestService(t testing.TB, root, serviceID string, minimumWorkers, maximumWorkers, minimumSandboxes, workersPerSandbox int, serviceType string) *workspacepackages.Store {
	t.Helper()
	identity, err := workspacepackages.ParseServiceID(serviceID)
	if err != nil {
		t.Fatal(err)
	}
	packageRoot := filepath.Join(root, "packages", identity.Namespace, identity.Repository)
	serviceRoot := filepath.Join(packageRoot, "services", identity.Service)
	writeTestFile(t, filepath.Join(packageRoot, "package.toml"), "schema = 1\ndescription = \"Test package\"\n")
	manifest := fmt.Sprintf(`schema = 2
description = "Test service"
entrypoint = "service.ts"

[lifecycle]
service_type = %q
session_keep_alive = "10m"

[scaling]
minimum_workers = %d
maximum_workers = %d
concurrency_per_worker = 1
target_utilization = 0.7
worker_keep_alive = "2m"

[placement]
sandbox_group = %q
minimum_sandboxes = %d
workers_per_sandbox = %d
`, serviceType, minimumWorkers, maximumWorkers, serviceID, minimumSandboxes, workersPerSandbox)
	writeTestFile(t, filepath.Join(serviceRoot, "service.toml"), manifest)
	writeTestFile(t, filepath.Join(serviceRoot, "service.ts"), "export default {};\n")
	store := newTestWorkspaceStore(t, root)
	if _, err := store.SynchronizePackageDefinitions(context.Background(), identity.PackageID(), "test-commit"); err != nil {
		t.Fatal(err)
	}
	return store
}

func newTestWorkspaceStore(t testing.TB, root string) *workspacepackages.Store {
	t.Helper()
	db := database.New(database.Config{
		Backend: database.BackendSQLite, Location: filepath.Join(root, "database", "system.db"),
		MaximumOpenConnections: 8, MaximumIdleConnections: 2,
	})
	if _, err := db.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE TABLE IF NOT EXISTS "the8020__packages__packages" ("packageId" TEXT PRIMARY KEY, "author" TEXT NOT NULL, "repository" TEXT NOT NULL, "source" TEXT, "requestedCommit" TEXT, "requestedTag" TEXT, "secretName" TEXT, "local" INTEGER NOT NULL, "activeCommit" TEXT, "state" TEXT NOT NULL, "error" TEXT, "revision" INTEGER NOT NULL, "createdAt" TEXT NOT NULL, "updatedAt" TEXT NOT NULL) STRICT`,
		`CREATE TABLE IF NOT EXISTS "the8020__system__revisions" ("domain" TEXT PRIMARY KEY, "revision" INTEGER NOT NULL, "updatedAt" TEXT NOT NULL) STRICT`,
		`CREATE INDEX IF NOT EXISTS "the8020__system__revisions__revision__index" ON "the8020__system__revisions" ("revision")`,
		`CREATE TABLE IF NOT EXISTS "the8020__services__services" ("serviceId" TEXT PRIMARY KEY, "packageId" TEXT NOT NULL, "packageCommit" TEXT NOT NULL, "manifestHash" TEXT NOT NULL, "description" TEXT NOT NULL, "entrypoint" TEXT NOT NULL, "accessMode" TEXT NOT NULL, "unauthenticatedAction" TEXT NOT NULL, "unauthenticatedStatus" INTEGER NOT NULL, "unauthenticatedMessage" TEXT NOT NULL, "unauthenticatedRedirectUrl" TEXT NOT NULL, "declaredServiceType" TEXT, "declaredSessionKeepAliveMs" INTEGER, "declaredMinimumWorkers" INTEGER, "declaredMaximumWorkers" INTEGER, "declaredConcurrencyPerWorker" INTEGER, "declaredTargetUtilization" REAL, "declaredWorkerKeepAliveMs" INTEGER, "declaredSandboxGroup" TEXT, "declaredMinimumSandboxes" INTEGER, "declaredWorkersPerSandbox" INTEGER, "enabled" INTEGER NOT NULL, "active" INTEGER NOT NULL, "desiredVersion" INTEGER NOT NULL, "createdAt" TEXT NOT NULL, "updatedAt" TEXT NOT NULL) STRICT`,
		`CREATE TABLE IF NOT EXISTS "the8020__services__overrides" ("serviceId" TEXT PRIMARY KEY, "serviceType" TEXT, "sessionKeepAliveMs" INTEGER, "minimumWorkers" INTEGER, "maximumWorkers" INTEGER, "concurrencyPerWorker" INTEGER, "targetUtilization" REAL, "workerKeepAliveMs" INTEGER, "sandboxGroup" TEXT, "minimumSandboxes" INTEGER, "workersPerSandbox" INTEGER, "updatedAt" TEXT NOT NULL) STRICT`,
		`CREATE TABLE IF NOT EXISTS "the8020__services__versions" ("serviceId" TEXT NOT NULL, "version" INTEGER NOT NULL, "packageCommit" TEXT NOT NULL, "manifestHash" TEXT NOT NULL, "policyHash" TEXT NOT NULL, "serviceType" TEXT NOT NULL, "sessionKeepAliveMs" INTEGER NOT NULL, "minimumWorkers" INTEGER NOT NULL, "maximumWorkers" INTEGER NOT NULL, "concurrencyPerWorker" INTEGER NOT NULL, "targetUtilization" REAL NOT NULL, "workerKeepAliveMs" INTEGER NOT NULL, "sandboxGroup" TEXT NOT NULL, "minimumSandboxes" INTEGER NOT NULL, "workersPerSandbox" INTEGER NOT NULL, "createdAt" TEXT NOT NULL, PRIMARY KEY ("serviceId", "version")) STRICT`,
	} {
		if _, err := db.ExecContext(context.Background(), statement); err != nil {
			t.Fatal(err)
		}
	}
	now := database.EncodeTime(db, time.Now().UTC())
	namespaces, err := os.ReadDir(filepath.Join(root, "packages"))
	if err != nil {
		t.Fatal(err)
	}
	for _, namespace := range namespaces {
		if !namespace.IsDir() {
			continue
		}
		repositories, err := os.ReadDir(filepath.Join(root, "packages", namespace.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, repository := range repositories {
			if !repository.IsDir() {
				continue
			}
			packageID := namespace.Name() + "/" + repository.Name()
			if _, err := db.ExecContext(context.Background(), `INSERT INTO "the8020__packages__packages" ("packageId", "author", "repository", "source", "requestedCommit", "requestedTag", "secretName", "local", "activeCommit", "state", "error", "revision", "createdAt", "updatedAt") VALUES ($1, $2, $3, NULL, NULL, NULL, NULL, $4, 'test-commit', 'ready', NULL, 1, $5, $5) ON CONFLICT ("packageId") DO NOTHING`, packageID, namespace.Name(), repository.Name(), true, now); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Cleanup(func() { _ = db.Close() })
	store, err := workspacepackages.New(workspacepackages.Config{WorkspaceRoot: root, Database: db})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func writeTestFile(t testing.TB, path, contents string) {
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

func countEventPrefix(events []string, prefix string) int {
	count := 0
	for _, event := range events {
		if strings.HasPrefix(event, prefix) {
			count++
		}
	}
	return count
}

func mustIdentity(t *testing.T, serviceID string) workspacepackages.Identity {
	t.Helper()
	identity, err := workspacepackages.ParseServiceID(serviceID)
	if err != nil {
		t.Fatal(err)
	}
	return identity
}
