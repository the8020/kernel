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
	"the8020/kernel/execution"
	executionservices "the8020/kernel/execution/services"
	executionworkers "the8020/kernel/execution/workers"
	workspacepackages "the8020/kernel/packages"
)

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
	store := newTestServiceIndex(t, root, "the8020/demo/variables", func(spec *Specification) {
		spec.Effective.Scaling.MinimumWorkers = 4
		spec.Effective.Scaling.MaximumWorkers = 16
		spec.Effective.Placement.MinimumSandboxes = 4
		spec.Effective.Placement.WorkersPerSandbox = 2
	})
	pools, router := newFakePools(), &fakeRouter{}
	manager := newTestManager(t, store, pools, router, filepath.Join(root, "node", "kernel", "services"))
	manager.nodes = &fakeNodeRouter{local: "node-b", indexes: []int{1, 3, 5, 7}}
	status, err := manager.Reconcile(context.Background(), "the8020/demo/variables")
	if err != nil || status.State != StateReady || status.SandboxCount != 2 || status.Sandboxes[0].Index != 1 || status.Sandboxes[1].Index != 3 {
		t.Fatalf("status=%#v err=%v", status, err)
	}
}

func TestReconcileMovesMinimumSandboxesWhenNodeAssignmentChanges(t *testing.T) {
	root := t.TempDir()
	store := newTestServiceIndex(t, root, "the8020/demo/variables", func(spec *Specification) {
		spec.Effective.Scaling.MinimumWorkers = 4
		spec.Effective.Scaling.MaximumWorkers = 8
		spec.Effective.Placement.MinimumSandboxes = 4
		spec.Effective.Placement.WorkersPerSandbox = 2
	})
	pools, router := newFakePools(), &fakeRouter{}
	manager := newTestManager(t, store, pools, router, filepath.Join(root, "node", "kernel", "services"))
	nodes := &fakeNodeRouter{local: "node-a", indexes: []int{0, 2}}
	manager.nodes = nodes
	status, err := manager.Reconcile(context.Background(), "the8020/demo/variables")
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

func (a *fakeAuthentication) VerifyToken(value string) (platformauth.TokenClaims, error) {
	a.calls++
	if value != "valid-jwt" {
		return nil, platformauth.ErrInvalidToken
	}
	return platformauth.TokenClaims{"sub": "user:admin", "sid": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "ver": 7}, nil
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
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"text/plain"}, http.CanonicalHeaderKey("the8020-internal-selected-worker-id"): []string{workerID}}, Body: responseStream}, nil
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
	request.Header.Set("the8020-internal-target-worker-id", workerID)
	return p.Dispatch(ctx, serviceID, request)
}

func (p *fakePools) ProxyWebSocket(_ context.Context, serviceID string, writer http.ResponseWriter, request *http.Request, modifyResponse func(*http.Response) error) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.websockets = append(p.websockets, dispatchedRequest{
		poolID: serviceID, path: request.URL.Path, query: request.URL.RawQuery, header: request.Header.Clone(),
	})
	response := &http.Response{StatusCode: http.StatusSwitchingProtocols, Header: make(http.Header)}
	record := p.records[serviceID]
	if len(record.WorkerIDs) > 0 {
		response.Header.Set("the8020-internal-selected-worker-id", record.WorkerIDs[0])
	}
	if modifyResponse != nil {
		if err := modifyResponse(response); err != nil {
			return err
		}
	}
	for name, values := range response.Header {
		writer.Header()[name] = values
	}

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
	store := newTestServiceIndex(t, root, "the8020/demo/variables", nil)
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

	status, err := manager.Reconcile(context.Background(), "the8020/demo/variables")
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
	store := newTestServiceIndex(t, root, "the8020/demo/variables", nil)
	if _, err := editTestSpecification(store, "the8020/demo/variables", func(state *Specification) error {
		zero := 0
		state.Enabled = true
		state.Effective.Scaling.MinimumWorkers = zero
		state.Effective.Placement.MinimumSandboxes = zero
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
	store := writeCanonicalTestService(t, root, serviceID, 0, 0, 0, 4, "stateless")
	if _, err := editTestSpecification(store, serviceID, func(state *Specification) error {
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
	poolID := versionPoolID(serviceID, definition.Release, 0)
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
	store := writeCanonicalTestService(t, root, serviceID, 1, 2, 1, 2, "stateless")
	pools, router := newFakePools(), &fakeRouter{}
	pools.dispatched = make(chan dispatchedRequest, 1)
	manager := newTestManager(t, store, pools, router, filepath.Join(root, "node", "kernel", "services"))
	started, err := manager.Reconcile(context.Background(), serviceID)
	if err != nil {
		t.Fatal(err)
	}
	existing := started.Sandboxes[0]
	pools.capacityErrors[existing.PoolID] = &executionservices.SandboxCapacityError{
		Occupied: 0,
		Slots:    1,
		Reason:   "target-utilization growth failed: sandbox Worker capacity is exhausted",
	}
	definition, _ := store.ReadService(serviceID)
	newPoolID := versionPoolID(serviceID, definition.Release, 1)
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
			store := writeCanonicalTestService(t, root, "the8020/demo/variables", test.minimumWorkers, 0, test.minimumSandboxes, 2, "stateless")
			if _, err := editTestSpecification(store, "the8020/demo/variables", func(state *Specification) error {
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
	store := writeCanonicalTestService(t, root, serviceID, 2, 4, 0, 4, "stateless")
	pools, router := newFakePools(), &fakeRouter{}
	definition, _ := store.ReadService(serviceID)
	firstPoolID := versionPoolID(serviceID, definition.Release, 0)
	pools.failScale[firstPoolID] = fmt.Errorf("%w: Worker limit is reached", executionworkers.ErrSandboxCapacity)
	manager := newTestManager(t, store, pools, router, filepath.Join(root, "node", "kernel", "services"))

	status, err := manager.Reconcile(context.Background(), serviceID)
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
	store := newTestServiceIndex(t, root, "the8020/demo/variables", nil)
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
	newTestServiceIndex(t, root, "the8020/demo/static", nil)
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

func TestRejectedIndexKeepsAcceptedVersionServingWithoutSourceReads(t *testing.T) {
	root := t.TempDir()
	const id = "the8020/demo/variables"
	index := newTestServiceIndex(t, root, id, nil)
	pools, router := newFakePools(), &fakeRouter{}
	manager := newTestManager(t, index, pools, router, filepath.Join(root, "observed"))
	if _, err := manager.Reconcile(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(root, "packages")); err != nil {
		t.Fatal(err)
	}
	invalid, _ := index.ReadService(id)
	invalid.Version++
	invalid.Effective.Placement.WorkersPerSandbox = 0
	if _, err := index.ReplacePackage("the8020/demo", []Specification{invalid}, "new-hooks"); err == nil {
		t.Fatal("invalid draft was published")
	}
	if err := manager.ReconcileAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	status, err := manager.Inspect(id)
	if err != nil || status.State != StateReady || status.LoadedVersion != 1 || status.Metrics.StartupFailures != 0 {
		t.Fatalf("accepted version=%#v error=%v", status, err)
	}
	assertHTTPStatus(t, router.boundary, "/"+id+"/", http.StatusOK)
	if _, err := index.ReplacePackage("the8020/demo", nil, "new-hooks"); err != nil {
		t.Fatal(err)
	}
	if err := manager.Retire(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	assertHTTPStatus(t, router.boundary, "/"+id+"/", http.StatusNotFound)
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
	store := newTestServiceIndex(t, root, "the8020/demo/variables", nil)
	pools, router := newFakePools(), &fakeRouter{}
	manager := newTestManager(t, store, pools, router, filepath.Join(root, "node", "kernel", "services"))
	rejection := fmt.Errorf("%w: type check failed", executionservices.ErrInvalidServiceDefinition)
	pools.failVersion[1] = rejection

	status, err := manager.Reconcile(context.Background(), "the8020/demo/variables")
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
	if retried, err := publishTestVersion(context.Background(), manager, "the8020/demo/variables", nil); !errors.Is(err, executionservices.ErrInvalidServiceDefinition) || retried.FailedVersion != 2 {
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
	store := newTestServiceIndex(t, root, "the8020/demo/variables", func(spec *Specification) {
		spec.Effective.Scaling.MinimumWorkers = 2
		spec.Effective.Scaling.MaximumWorkers = 2
		spec.Effective.Placement.MinimumSandboxes = 2
		spec.Effective.Placement.WorkersPerSandbox = 1
	})
	if _, err := editTestSpecification(store, "the8020/demo/variables", func(state *Specification) error {
		state.Enabled = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	definition, err := store.ReadService("the8020/demo/variables")
	if err != nil {
		t.Fatal(err)
	}
	firstPoolID := versionPoolID(definition.Identity.ServiceID(), definition.Release, 0)
	missingPoolID := versionPoolID(definition.Identity.ServiceID(), definition.Release, 1)
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
	store := newTestServiceIndex(t, root, "the8020/demo/variables", func(spec *Specification) {
		spec.Effective.Scaling.MinimumWorkers = 2
		spec.Effective.Scaling.MaximumWorkers = 2
		spec.Effective.Placement.WorkersPerSandbox = 2
	})
	pools := newFakePools()
	manager := newTestManager(t, store, pools, &fakeRouter{}, filepath.Join(root, "node", "kernel", "node-a", "services"))
	started, err := manager.Reconcile(context.Background(), "the8020/demo/variables")
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
	store := newTestServiceIndex(t, root, "the8020/demo/variables", func(spec *Specification) { spec.Effective.Scaling.MinimumWorkers = 2 })
	pools, router := newFakePools(), &fakeRouter{}
	manager := newTestManager(t, store, pools, router, filepath.Join(root, "node", "kernel", "node-a", "services"))
	var logs bytes.Buffer
	manager.logger = slog.New(slog.NewJSONHandler(&logs, nil))

	started, err := manager.Reconcile(context.Background(), "the8020/demo/variables")
	if err != nil {
		t.Fatal(err)
	}
	if started.State != StateReady || started.DesiredVersion != 1 || started.LoadedVersion != 1 || started.SandboxCount != 1 || started.WorkerCount != 2 {
		t.Fatalf("started = %#v", started)
	}
	state, err := store.ReadService("the8020/demo/variables")
	if err != nil || !state.Enabled || state.Version != 1 {
		t.Fatalf("specification=%#v error=%v", state, err)
	}
	firstPool := started.Sandboxes[0].PoolID

	restarted, err := publishTestVersion(context.Background(), manager, "the8020/demo/variables", nil)
	if err != nil {
		t.Fatal(err)
	}
	if restarted.LoadedVersion != 2 || restarted.Sandboxes[0].PoolID == firstPool {
		t.Fatalf("restarted = %#v", restarted)
	}
	assertEventBefore(t, pools.events, "start:"+restarted.Sandboxes[0].PoolID+":2", "stop:"+firstPool)

	pools.failVersion[3] = errors.New("entrypoint initialization failed")
	degraded, err := publishTestVersion(context.Background(), manager, "the8020/demo/variables", nil)
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

	scaled, err := publishTestVersion(context.Background(), manager, "the8020/demo/variables", func(spec *Specification) {
		spec.Effective.Scaling = ScalingConfiguration{MinimumWorkers: 6, MaximumWorkers: 10, ConcurrencyPerWorker: 8, TargetUtilization: 0.65, WorkerKeepAlive: 3 * time.Minute}
		spec.Effective.Placement = PlacementConfiguration{MinimumSandboxes: 2, WorkersPerSandbox: 5, SandboxGroup: "shared-proof"}
		spec.Effective.Lifecycle.SessionKeepAlive = 4 * time.Minute
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
	stopped, err := publishTestVersion(context.Background(), manager, "the8020/demo/variables", func(spec *Specification) { spec.Enabled = false })
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
	store := writeCanonicalTestService(t, root, "the8020/demo/variables", 1, 2, 0, 2, "stateless")
	pools, router := newFakePools(), &fakeRouter{}
	manager := newTestManager(t, store, pools, router, filepath.Join(root, "node", "kernel", "services"))

	startEntered, startRelease := make(chan string, 1), make(chan struct{})
	pools.startEntered, pools.startRelease = startEntered, startRelease
	started := make(chan Status, 1)
	startErrors := make(chan error, 1)
	go func() {
		status, err := manager.Reconcile(context.Background(), "the8020/demo/variables")
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
		status, err := publishTestVersion(context.Background(), manager, "the8020/demo/variables", nil)
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
		status, err := publishTestVersion(context.Background(), manager, "the8020/demo/variables", func(spec *Specification) { spec.Enabled = false })
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

func TestCanonicalBoundaryReadsAcceptedIndexStripsPrefixAndUsesTrustedMetadata(t *testing.T) {
	root := t.TempDir()
	store := newTestServiceIndex(t, root, "the8020/demo/variables", func(spec *Specification) { spec.Enabled = false })
	pools, router := newFakePools(), &fakeRouter{}
	pools.responseStatus, pools.responseBody = http.StatusTeapot, "service-body"
	pools.dispatched = make(chan dispatchedRequest, 1)
	manager := newTestManager(t, store, pools, router, filepath.Join(root, "node", "kernel", "services"))

	assertHTTPStatus(t, manager, "/missing/package/service/path", http.StatusNotFound)
	assertHTTPStatus(t, manager, "/the8020/demo/variables/path", http.StatusServiceUnavailable)
	assertHTTPStatus(t, manager, "/the8020/demo/variables%2fescape/path", http.StatusBadRequest)
	if _, err := publishTestVersion(context.Background(), manager, "the8020/demo/variables", func(spec *Specification) { spec.Enabled = true }); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPatch, "http://example.test/the8020/demo/variables/orders/7?expand=yes", bytes.NewBufferString("stream me"))
	request.Header.Set("the8020-internal-service-id", "attacker/service/value")
	request.Header.Set("the8020-internal-client-ip-address", "198.51.100.99")
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
	if dispatched.header.Get("the8020-internal-service-id") != "the8020/demo/variables" || dispatched.header.Get("the8020-internal-canonical-base-path") != "/the8020/demo/variables" || dispatched.header.Get("the8020-internal-original-host") != "example.test" {
		t.Fatalf("trusted headers = %#v", dispatched.header)
	}
	if dispatched.header.Get("the8020-internal-client-ip-address") != "192.0.2.1" || dispatched.header.Get("the8020-internal-client-network-scope") != "public" {
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

	if _, err := publishTestVersion(context.Background(), manager, "the8020/demo/variables", func(spec *Specification) { spec.Enabled = false }); err != nil {
		t.Fatal(err)
	}
	assertHTTPStatus(t, manager, "/the8020/demo/variables/path", http.StatusServiceUnavailable)
	if _, err := publishTestVersion(context.Background(), manager, "the8020/demo/variables", func(spec *Specification) { spec.Enabled = true }); err != nil {
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
	store := newTestServiceIndex(t, root, "the8020/demo/protected", func(spec *Specification) {
		spec.Access.Mode = "authenticated"
		spec.Access.Unauthenticated.Message = "Sign in first."
	})
	pools, router := newFakePools(), &fakeRouter{}
	pools.dispatched = make(chan dispatchedRequest, 1)
	manager := newTestManager(t, store, pools, router, filepath.Join(root, "node", "kernel", "services"))
	authentication := &fakeAuthentication{}
	manager.authentication = authentication
	manager.authenticator = "/p/the8020/users/mod.ts"
	if _, err := manager.Reconcile(context.Background(), "the8020/demo/protected"); err != nil {
		t.Fatal(err)
	}

	unauthenticated := httptest.NewRecorder()
	spoofed := httptest.NewRequest(http.MethodGet, "/the8020/demo/protected/value", nil)
	spoofed.Header.Set("the8020-internal-auth-username", "attacker")
	spoofed.Header.Set("the8020-internal-username", "attacker")
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
	authorizedRequest.AddCookie(&http.Cookie{Name: "the8020_auth", Value: "valid-jwt"})
	authorizedRequest.Header.Set("the8020-internal-auth-username", "attacker")
	authorizedRequest.Header.Set("the8020-internal-username", "attacker")
	manager.ServeHTTP(authorized, authorizedRequest)
	if authorized.Code != http.StatusOK || authentication.calls != 2 {
		t.Fatalf("authorized response=%d calls=%d body=%q", authorized.Code, authentication.calls, authorized.Body.String())
	}
	forwarded := <-pools.dispatched
	for name, want := range map[string]string{
		http.CanonicalHeaderKey("the8020-internal-auth-authenticated"): "",
		http.CanonicalHeaderKey("the8020-internal-auth-realm"):         "",
		http.CanonicalHeaderKey("the8020-internal-auth-user-id"):       "",
		http.CanonicalHeaderKey("the8020-internal-auth-username"):      "",
		http.CanonicalHeaderKey("the8020-internal-auth-version"):       "",
		http.CanonicalHeaderKey("the8020-internal-user-id"):            "user:admin",
		http.CanonicalHeaderKey("the8020-internal-username"):           "admin",
	} {
		if got := forwarded.header.Get(name); got != want {
			t.Errorf("forwarded %s = %q, want %q", name, got, want)
		}
	}
	if cookie := forwarded.header.Get("Cookie"); cookie != "other=preserved; the8020_auth=valid-jwt" {
		t.Fatalf("forwarded cookies = %q", cookie)
	}
	if forwarded.header.Get("the8020-internal-authentication") == "" {
		t.Fatal("verified token metadata missing")
	}

	if _, err := publishTestVersion(context.Background(), manager, "the8020/demo/protected", func(spec *Specification) {
		spec.Access.Unauthenticated = UnauthenticatedPolicy{Action: "redirect", Status: 307, RedirectURL: "https://identity.example.test/login?return=fixed"}
	}); err != nil {
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

func TestPublicServiceIgnoresTokensAndPreservesRawCredentials(t *testing.T) {
	root := t.TempDir()
	store := newTestServiceIndex(t, root, "the8020/demo/public", nil)
	pools := newFakePools()
	pools.dispatched = make(chan dispatchedRequest, 1)
	manager := newTestManager(t, store, pools, &fakeRouter{}, filepath.Join(root, "observed"))
	authentication := &fakeAuthentication{}
	manager.authentication = authentication
	if _, err := manager.Reconcile(context.Background(), "the8020/demo/public"); err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{"", "valid-jwt", "invalid-jwt", "expired-jwt"} {
		request := httptest.NewRequest(http.MethodGet, "/the8020/demo/public/", nil)
		if token != "" {
			request.AddCookie(&http.Cookie{Name: "the8020_auth", Value: token})
			request.Header.Set(platformauth.TokenHeader, "Bearer "+token)
		}
		request.Header.Set("the8020-internal-authentication", "forged")
		response := httptest.NewRecorder()
		manager.ServeHTTP(response, request)
		if response.Code != http.StatusOK || authentication.calls != 0 {
			t.Fatalf("public verification occurred: %d, %d", response.Code, authentication.calls)
		}
		forwarded := <-pools.dispatched
		if forwarded.header.Get("the8020-internal-authentication") != "" || forwarded.header.Get("the8020-internal-username") != "system" || forwarded.header.Get("Cookie") != request.Header.Get("Cookie") || forwarded.header.Get(platformauth.TokenHeader) != request.Header.Get(platformauth.TokenHeader) {
			t.Fatalf("public metadata/credentials changed: %#v", forwarded.header)
		}
	}
}

func TestRequestServiceWebSocketUsesCanonicalRoutingAndTrustedMetadata(t *testing.T) {
	root := t.TempDir()
	store := newTestServiceIndex(t, root, "the8020/demo/events", nil)
	pools, router := newFakePools(), &fakeRouter{}
	manager := newTestManager(t, store, pools, router, filepath.Join(root, "node", "kernel", "services"))
	if _, err := manager.Reconcile(context.Background(), "the8020/demo/events"); err != nil {
		t.Fatal(err)
	}

	request := websocketRequest("/the8020/demo/events/echo/main?format=text", "")
	request.Header.Set("Sec-WebSocket-Protocol", "the8020.echo")
	request.Header.Set("the8020-internal-auth-username", "attacker")
	request.Header.Set("the8020-internal-username", "attacker")
	response := httptest.NewRecorder()
	manager.ServeHTTP(response, request)
	if response.Code != http.StatusOK || len(pools.websockets) != 1 {
		t.Fatalf("request WebSocket status=%d proxies=%#v", response.Code, pools.websockets)
	}
	proxied := pools.websockets[0]
	if proxied.path != "/echo/main" || proxied.query != "format=text" || proxied.header.Get("Sec-WebSocket-Protocol") != "the8020.echo" {
		t.Fatalf("request WebSocket route = %#v", proxied)
	}
	if proxied.header.Get("the8020-internal-service-id") != "the8020/demo/events" || proxied.header.Get("the8020-internal-canonical-base-path") != "/the8020/demo/events" || proxied.header.Get("the8020-internal-authentication") != "" {
		t.Fatalf("request WebSocket trusted metadata = %#v", proxied.header)
	}
	if proxied.header.Get("the8020-internal-auth-username") != "" {
		t.Fatalf("request WebSocket leaked session or spoofed metadata = %#v", proxied.header)
	}
	if proxied.header.Get("the8020-internal-user-id") != "user:system" || proxied.header.Get("the8020-internal-username") != "system" {
		t.Fatalf("request WebSocket execution user = %#v", proxied.header)
	}
	status, err := manager.Inspect("the8020/demo/events")
	if err != nil || status.Metrics.ActiveRequests != 0 || status.Metrics.RequestCount != 1 || status.Metrics.ResponseStatus["101"] != 1 {
		t.Fatalf("request WebSocket accounting = %#v err=%v", status.Metrics, err)
	}
}

func TestSessionServiceEstablishesHTTPRouteThenReconnectsWebSocketToExactSandbox(t *testing.T) {
	root := t.TempDir()
	store := newTestServiceIndex(t, root, "example/realtime/channel", func(spec *Specification) {
		spec.Effective.Scaling.MinimumWorkers = 2
		spec.Effective.Scaling.MaximumWorkers = 16
		spec.Effective.Scaling.ConcurrencyPerWorker = 1
		spec.Effective.Placement.MinimumSandboxes = 2
		spec.Effective.Placement.WorkersPerSandbox = 8
		spec.Effective.Placement.SandboxGroup = "realtime"
		spec.Effective.Lifecycle.ServiceType = "session"
		spec.Effective.Lifecycle.SessionKeepAlive = 120000000000
		spec.Access.Mode = "authenticated"
	})
	pools, router := newFakePools(), &fakeRouter{}
	manager := newTestManager(t, store, pools, router, filepath.Join(root, "node", "kernel", "services"))
	manager.authentication = &fakeAuthentication{}
	manager.authenticator = "/p/the8020/users/mod.ts"
	status, err := manager.Reconcile(context.Background(), "example/realtime/channel")
	if err != nil {
		t.Fatal(err)
	}
	if status.ServiceType != "session" || status.SandboxCount != 2 || status.WorkerCount != 2 {
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
	establish.AddCookie(&http.Cookie{Name: "the8020_auth", Value: "valid-jwt"})
	established := httptest.NewRecorder()
	manager.ServeHTTP(established, establish)
	route := established.Header().Get(RouteHeader)
	initial := <-pools.dispatched
	if established.Code != http.StatusOK || route == "" || initial.header.Get("the8020-internal-persistent-execution-id") == "" || initial.header.Get("the8020-internal-persistent-keep-alive-ms") != "120000" {
		t.Fatalf("establishment status=%d route=%q dispatch=%#v", established.Code, route, initial)
	}

	upgrade := websocketRequest("/example/realtime/channel/connect?route="+route, "")
	upgrade.AddCookie(&http.Cookie{Name: "the8020_auth", Value: "valid-jwt"})
	upgrade.Header.Set("the8020-internal-auth-username", "attacker")
	response := httptest.NewRecorder()
	manager.ServeHTTP(response, upgrade)
	if response.Code != http.StatusOK || len(pools.websockets) != 1 {
		t.Fatalf("upgrade status=%d proxies=%#v", response.Code, pools.websockets)
	}
	proxied := pools.websockets[0]
	if proxied.poolID != initial.poolID || proxied.path != "/connect" || proxied.query != "" || proxied.header.Get("the8020-internal-authentication") == "" || proxied.header.Get("the8020-internal-username") != "admin" || proxied.header.Get("the8020-internal-persistent-execution-id") == "" || proxied.header.Get("Cookie") != "the8020_auth=valid-jwt" {
		t.Fatalf("proxied persistent request = %#v", proxied)
	}

	poolRecord := pools.records[initial.poolID]
	staleToken, err := manager.signing.SignRoute(platformauth.RouteTarget{
		NodeID: manager.nodeID, SandboxID: poolRecord.SandboxID,
		WorkerID: "worker-from-before-kernel-restart", ExecutionID: "persistent-stale",
	})
	if err != nil {
		t.Fatal(err)
	}
	stale := httptest.NewRequest(http.MethodPost, "/example/realtime/channel/connect", nil)
	stale.Header.Set(RouteHeader, staleToken)
	stale.AddCookie(&http.Cookie{Name: "the8020_auth", Value: "valid-jwt"})
	staleResponse := httptest.NewRecorder()
	manager.ServeHTTP(staleResponse, stale)
	if staleResponse.Code != http.StatusConflict {
		t.Fatalf("stale route status=%d body=%q", staleResponse.Code, staleResponse.Body.String())
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
	store := newTestServiceIndex(t, root, "example/realtime/channel", func(spec *Specification) {
		spec.Effective.Scaling.MinimumWorkers = 2
		spec.Effective.Scaling.MaximumWorkers = 16
		spec.Effective.Scaling.ConcurrencyPerWorker = 1
		spec.Effective.Placement.MinimumSandboxes = 2
		spec.Effective.Placement.WorkersPerSandbox = 8
		spec.Effective.Placement.SandboxGroup = "realtime"
		spec.Effective.Lifecycle.ServiceType = "session"
		spec.Effective.Lifecycle.SessionKeepAlive = 120000000000
		spec.Access.Mode = "authenticated"
	})
	pools, router := newFakePools(), &fakeRouter{}
	manager := newTestManager(t, store, pools, router, filepath.Join(root, "node", "kernel", "services"))
	manager.authentication = &fakeAuthentication{}
	manager.authenticator = "/p/the8020/users/mod.ts"
	initial, err := manager.Reconcile(context.Background(), "example/realtime/channel")
	if err != nil {
		t.Fatal(err)
	}
	oldPool := initial.Sandboxes[0].PoolID
	pools.dispatched = make(chan dispatchedRequest, 1)
	establish := httptest.NewRequest(http.MethodPost, "/example/realtime/channel/connect", nil)
	establish.AddCookie(&http.Cookie{Name: "the8020_auth", Value: "valid-jwt"})
	established := httptest.NewRecorder()
	manager.ServeHTTP(established, establish)
	route := established.Header().Get(RouteHeader)
	establishedDispatch := <-pools.dispatched
	oldPool = establishedDispatch.poolID
	if route == "" {
		t.Fatal("persistent establishment did not return a route")
	}
	pools.occupiedSlots[oldPool] = 1

	reloaded, err := publishTestVersion(context.Background(), manager, "example/realtime/channel", nil)
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
	resume.AddCookie(&http.Cookie{Name: "the8020_auth", Value: "valid-jwt"})
	response := httptest.NewRecorder()
	manager.ServeHTTP(response, resume)
	if response.Code != http.StatusConflict || len(pools.websockets) != 0 {
		t.Fatalf("old route status=%d proxies=%#v", response.Code, pools.websockets)
	}

	pools.dispatched = make(chan dispatchedRequest, 1)
	currentRequest := httptest.NewRequest(http.MethodPost, "/example/realtime/channel/connect", nil)
	currentRequest.AddCookie(&http.Cookie{Name: "the8020_auth", Value: "valid-jwt"})
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
	store := newTestServiceIndex(t, root, serviceID, func(spec *Specification) {
		spec.Effective.Scaling.MaximumWorkers = 2
		spec.Effective.Scaling.ConcurrencyPerWorker = 1
		spec.Effective.Placement.WorkersPerSandbox = 1
	})
	pools, router := newFakePools(), &fakeRouter{}
	pools.dispatched = make(chan dispatchedRequest, 1)
	pools.release = make(chan struct{})
	manager := newTestManager(t, store, pools, router, filepath.Join(root, "node", "kernel", "services"))
	initial, err := manager.Reconcile(context.Background(), serviceID)
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

	reloaded, err := publishTestVersion(context.Background(), manager, serviceID, nil)
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
	store := newTestServiceIndex(t, root, "example/realtime/channel", func(spec *Specification) {
		spec.Effective.Scaling.MinimumWorkers = 2
		spec.Effective.Scaling.MaximumWorkers = 16
		spec.Effective.Scaling.ConcurrencyPerWorker = 1
		spec.Effective.Placement.MinimumSandboxes = 2
		spec.Effective.Placement.WorkersPerSandbox = 8
		spec.Effective.Placement.SandboxGroup = "realtime"
		spec.Effective.Lifecycle.ServiceType = "session"
		spec.Effective.Lifecycle.SessionKeepAlive = 120000000000
		spec.Access.Mode = "authenticated"
	})
	owner := newTestRouteSigner(t)
	token, err := owner.SignRoute(platformauth.RouteTarget{NodeID: "node-a", SandboxID: "remote-sandbox", WorkerID: "remote-worker", ExecutionID: "persistent-remote"})
	if err != nil {
		t.Fatal(err)
	}
	pools, router := newFakePools(), &fakeRouter{}
	nodeRouter := &fakeNodeRouter{local: "node-b"}
	manager, err := New(Config{Index: store, Pools: pools, Router: router, ObservedRoot: filepath.Join(root, "node", "kernel", "services"), NodeID: "node-b", Signing: newTestRouteSigner(t), Nodes: nodeRouter})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	manager.authentication = &fakeAuthentication{}
	manager.authenticator = "/p/the8020/users/mod.ts"
	if _, err := manager.Reconcile(context.Background(), "example/realtime/channel"); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/example/realtime/channel/connect", nil)
	request.Header.Set(RouteHeader, token)
	request.AddCookie(&http.Cookie{Name: "the8020_auth", Value: "valid-jwt"})
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
	store := newTestServiceIndex(t, root, "namespace-a/repository/service", nil)
	newTestServiceIndex(t, root, "namespace-b/repository/service", nil)
	newTestServiceIndex(t, root, "namespace-a/other-repository/service", nil)
	pools, router := newFakePools(), &fakeRouter{}
	pools.dispatched = make(chan dispatchedRequest, 1)
	manager := newTestManager(t, store, pools, router, filepath.Join(root, "node", "kernel", "services"))
	for _, serviceID := range []string{"namespace-a/repository/service", "namespace-b/repository/service", "namespace-a/other-repository/service"} {
		if _, err := manager.Reconcile(context.Background(), serviceID); err != nil {
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
	store := newTestServiceIndex(t, root, "the8020/demo/variables", nil)
	newTestServiceIndex(t, root, "the8020/demo/variables-import", nil)
	pools, router := newFakePools(), &fakeRouter{}
	pools.dispatched = make(chan dispatchedRequest, 1)
	manager := newTestManager(t, store, pools, router, filepath.Join(root, "node", "kernel", "services"))
	first, err := manager.Reconcile(context.Background(), "the8020/demo/variables")
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.Reconcile(context.Background(), "the8020/demo/variables-import")
	if err != nil {
		t.Fatal(err)
	}
	if first.Sandboxes[0].RuntimeGroupID == second.Sandboxes[0].RuntimeGroupID {
		t.Fatal("default service identities unexpectedly shared a runtime group")
	}
	shared := "shared-proof"
	first, err = publishTestVersion(context.Background(), manager, first.ServiceID, func(spec *Specification) { spec.Effective.Placement.SandboxGroup = shared })
	if err != nil {
		t.Fatal(err)
	}
	second, err = publishTestVersion(context.Background(), manager, second.ServiceID, func(spec *Specification) { spec.Effective.Placement.SandboxGroup = shared })
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
	if _, err := publishTestVersion(context.Background(), manager, first.ServiceID, nil); err != nil {
		t.Fatal(err)
	}
	if status, err := manager.Inspect(second.ServiceID); err != nil || status.State != StateReady {
		t.Fatalf("sibling after restart=%#v err=%v", status, err)
	}
	if _, err := publishTestVersion(context.Background(), manager, first.ServiceID, func(spec *Specification) { spec.Enabled = false }); err != nil {
		t.Fatal(err)
	}
	if status, err := manager.Inspect(second.ServiceID); err != nil || status.State != StateReady || status.SandboxCount != 1 {
		t.Fatalf("sibling after stop=%#v err=%v", status, err)
	}
}

func TestLeastInFlightSelectionAndTimeout(t *testing.T) {
	root := t.TempDir()
	store := newTestServiceIndex(t, root, "the8020/demo/variables", func(spec *Specification) {
		spec.Effective.Scaling.MinimumWorkers = 2
		spec.Effective.Scaling.MaximumWorkers = 8
		spec.Effective.Placement.MinimumSandboxes = 2
	})
	pools, router := newFakePools(), &fakeRouter{}
	pools.dispatched = make(chan dispatchedRequest, 3)
	pools.release = make(chan struct{})
	manager := newTestManager(t, store, pools, router, filepath.Join(root, "node", "kernel", "services"))
	started, err := manager.Reconcile(context.Background(), "the8020/demo/variables")
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
		manager.dispatch(timeoutResponse, timeoutRequest, mustIdentity(t, "the8020/demo/variables"), "/timeout", runtime, runtime.sandboxes[0], time.Millisecond, nil, execution.SystemUser(), nil)
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
	store := writeCanonicalTestService(t, root, "the8020/demo/variables", 1, 4, 1, 4, "stateless")
	pools, router := newFakePools(), &fakeRouter{}
	pools.dispatched = make(chan dispatchedRequest, 2)
	pools.release = make(chan struct{})
	manager := newTestManager(t, store, pools, router, filepath.Join(root, "node", "kernel", "services"))
	started, err := manager.Reconcile(context.Background(), "the8020/demo/variables")
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
	store := writeCanonicalTestService(t, root, "the8020/demo/variables", 1, 1, 1, 1, "stateless")
	pools, router := newFakePools(), &fakeRouter{}
	manager := newTestManager(t, store, pools, router, filepath.Join(root, "node", "kernel", "services"))
	status, err := manager.Reconcile(context.Background(), "the8020/demo/variables")
	if err != nil || len(status.Sandboxes) != 1 {
		t.Fatalf("start=%#v err=%v", status, err)
	}
	poolID := status.Sandboxes[0].PoolID
	pools.mu.Lock()
	pools.capacityCalls[poolID], pools.ensureCalls[poolID] = 0, 0
	pools.events = nil
	pools.mu.Unlock()

	manifest := filepath.Join(root, "packages", "the8020", "demo", "services", "variables", "service.ts")
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
	newTestServiceIndex(t, root, "the8020/demo/variables", nil)
	store := newTestServiceIndex(t, root, "the8020/demo/variables-import", nil)
	pools, router := newFakePools(), &fakeRouter{}
	manager := newTestManager(t, store, pools, router, filepath.Join(root, "node", "kernel", "services"))
	for _, serviceID := range []string{"the8020/demo/variables", "the8020/demo/variables-import"} {
		if _, err := manager.Reconcile(context.Background(), serviceID); err != nil {
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
	newTestServiceIndex(t, root, "the8020/demo/variables", nil)
	store := newTestServiceIndex(t, root, "the8020/demo/variables-import", nil)
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
		_, err := manager.Reconcile(context.Background(), serviceID)
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
	store := writeCanonicalTestService(t, root, "the8020/demo/variables", 1, 1, 1, 1, "stateless")
	pools, router := newFakePools(), &fakeRouter{}
	manager := newTestManager(t, store, pools, router, filepath.Join(root, "node", "kernel", "services"))
	if _, err := manager.Reconcile(context.Background(), "the8020/demo/variables"); err != nil {
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
	store := writeCanonicalTestService(t, root, "the8020/demo/variables", 1, 1, 1, 1, "stateless")
	pools, router := newFakePools(), &fakeRouter{}
	manager := newTestManager(t, store, pools, router, filepath.Join(root, "node", "kernel", "services"))
	status, err := manager.Reconcile(context.Background(), "the8020/demo/variables")
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
	store := newTestServiceIndex(t, root, "the8020/demo/variables", func(spec *Specification) {
		spec.Effective.Scaling.MaximumWorkers = 1
		spec.Effective.Scaling.ConcurrencyPerWorker = 2
		spec.Effective.Placement.WorkersPerSandbox = 1
	})
	pools, router := newFakePools(), &fakeRouter{}
	manager := newTestManager(t, store, pools, router, filepath.Join(root, "node", "kernel", "services"))
	if _, err := manager.Reconcile(context.Background(), "the8020/demo/variables"); err != nil {
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
	store := newTestServiceIndex(t, root, "the8020/demo/variables", func(spec *Specification) {
		spec.Effective.Scaling.MaximumWorkers = 2
		spec.Effective.Scaling.ConcurrencyPerWorker = 1
		spec.Effective.Placement.WorkersPerSandbox = 1
	})
	pools, router := newFakePools(), &fakeRouter{}
	pools.dispatched = make(chan dispatchedRequest, 1)
	manager := newTestManager(t, store, pools, router, filepath.Join(root, "node", "kernel", "services"))
	started, err := manager.Reconcile(context.Background(), "the8020/demo/variables")
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
	store := writeCanonicalTestService(t, root, "the8020/demo/variables", 0, 2, 0, 1, "stateless")
	pools, router := newFakePools(), &fakeRouter{}
	manager := newTestManager(t, store, pools, router, filepath.Join(root, "node", "kernel", "services"))
	if _, err := manager.Reconcile(context.Background(), "the8020/demo/variables"); err != nil {
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
	store := newTestServiceIndex(t, root, "the8020/demo/variables", func(spec *Specification) {
		spec.Effective.Scaling.MaximumWorkers = 2
		spec.Effective.Scaling.ConcurrencyPerWorker = 1
		spec.Effective.Placement.WorkersPerSandbox = 1
	})
	pools, router := newFakePools(), &fakeRouter{}
	manager := newTestManager(t, store, pools, router, filepath.Join(root, "node", "kernel", "services"))
	started, err := manager.Reconcile(context.Background(), "the8020/demo/variables")
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
	store := newTestServiceIndex(t, root, "the8020/demo/variables", nil)
	pools, router := newFakePools(), &fakeRouter{}
	pools.dispatchEntered = make(chan struct{}, 1)
	responseReader, responseWriter := io.Pipe()
	pools.responseStream = responseReader
	manager := newTestManager(t, store, pools, router, filepath.Join(root, "node", "kernel", "services"))
	if _, err := manager.Reconcile(context.Background(), "the8020/demo/variables"); err != nil {
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

func newTestManager(t testing.TB, store *Index, pools *fakePools, router *fakeRouter, observed string) *Manager {
	t.Helper()
	manager, err := New(Config{Index: store, Pools: pools, Router: router, ObservedRoot: observed, Authenticator: "/p/the8020/users/mod.ts", ReconcileInterval: 10 * time.Millisecond, StartupTimeout: time.Second, Signing: newTestRouteSigner(t)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	return manager
}

// Fixtures publish already resolved runtime input. Configuration parsing and
// durable operator behavior are tested by the services Deno package.
var testIndexes sync.Map

func newTestServiceIndex(t testing.TB, root, serviceID string, configure func(*Specification)) *Index {
	t.Helper()
	value, loaded := testIndexes.LoadOrStore(root, NewIndex())
	if !loaded {
		t.Cleanup(func() { testIndexes.Delete(root) })
	}
	index := value.(*Index)
	spec := indexedTestSpecification(serviceID)
	spec.CodeRevision = "test-commit"
	identity, _ := workspacepackages.ParseServiceID(serviceID)
	spec.EntrypointURL = "file:///workspace/packages/" + identity.PackageID() + "/services/" + identity.Service + "/service.ts"
	spec.Description = "Test service"
	spec.Access = AccessPolicy{Mode: "public", Unauthenticated: UnauthenticatedPolicy{Action: "reject", Status: 401, Message: "Authentication is required."}}
	spec.Effective.Lifecycle.SessionKeepAlive = 10 * time.Minute
	spec.Effective.Scaling = ScalingConfiguration{MinimumWorkers: 1, MaximumWorkers: 4, ConcurrencyPerWorker: 32, TargetUtilization: 0.7, WorkerKeepAlive: 2 * time.Minute}
	spec.Effective.Placement = PlacementConfiguration{MinimumSandboxes: 1, WorkersPerSandbox: 4}
	spec.Effective.Timeouts = TimeoutConfiguration{Request: 30 * time.Second, Drain: 30 * time.Second}
	if configure != nil {
		configure(&spec)
	}
	var fragment []Specification
	for _, id := range index.ServiceIDs() {
		current, _ := index.ReadService(id)
		if current.Identity.PackageID() == identity.PackageID() && id != serviceID {
			fragment = append(fragment, current)
		}
	}
	if _, err := index.ReplacePackage(identity.PackageID(), append(fragment, spec), "test-hooks"); err != nil {
		t.Fatal(err)
	}
	serviceRoot := filepath.Join(root, "packages", identity.Namespace, identity.Repository, "services", identity.Service)
	writeTestFile(t, filepath.Join(serviceRoot, "service.ts"), "export default {};\n")
	return index
}

func writeCanonicalTestService(t testing.TB, root, serviceID string, minimumWorkers, maximumWorkers, minimumSandboxes, workersPerSandbox int, serviceType string) *Index {
	return newTestServiceIndex(t, root, serviceID, func(spec *Specification) {
		spec.Effective.Lifecycle.ServiceType = serviceType
		spec.Effective.Scaling.MinimumWorkers = minimumWorkers
		spec.Effective.Scaling.MaximumWorkers = maximumWorkers
		spec.Effective.Scaling.ConcurrencyPerWorker = 1
		spec.Effective.Placement = PlacementConfiguration{SandboxGroup: serviceID, MinimumSandboxes: minimumSandboxes, WorkersPerSandbox: workersPerSandbox}
	})
}

func editTestSpecification(index *Index, serviceID string, edit func(*Specification) error) (Specification, error) {
	spec, err := index.ReadService(serviceID)
	if err != nil {
		return spec, err
	}
	if edit != nil {
		if err := edit(&spec); err != nil {
			return spec, err
		}
	}
	var fragment []Specification
	for _, id := range index.ServiceIDs() {
		current, _ := index.ReadService(id)
		if current.Identity.PackageID() == spec.Identity.PackageID() && id != serviceID {
			fragment = append(fragment, current)
		}
	}
	if _, err := index.ReplacePackage(spec.Identity.PackageID(), append(fragment, spec), "test-hooks"); err != nil {
		return spec, err
	}
	return index.ReadService(serviceID)
}

func publishTestVersion(ctx context.Context, manager *Manager, serviceID string, edit func(*Specification)) (Status, error) {
	if _, err := editTestSpecification(manager.index, serviceID, func(spec *Specification) error {
		spec.Version++
		if edit != nil {
			edit(spec)
		}
		return nil
	}); err != nil {
		return Status{}, err
	}
	return manager.Reconcile(ctx, serviceID)
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

func TestRemovedServiceRetirementRetriesFailuresAndDrainingOnOrdinaryMaintenance(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	id := "the8020/demo/variables"
	index := newTestServiceIndex(t, root, id, func(spec *Specification) { spec.Effective.Scaling.MinimumWorkers = 1 })
	pools := newFakePools()
	manager := newTestManager(t, index, pools, &fakeRouter{}, filepath.Join(root, "observed"))
	status, err := manager.Reconcile(ctx, id)
	if err != nil || len(status.Sandboxes) != 1 {
		t.Fatalf("status=%#v error=%v", status, err)
	}
	pool := status.Sandboxes[0].PoolID
	if _, err := index.ReplacePackage("the8020/demo", nil, "removed"); err != nil {
		t.Fatal(err)
	}
	pools.failStop[pool] = errors.New("temporary supervisor failure")
	if err := manager.Retire(ctx, id); err == nil {
		t.Fatal("missing retirement error")
	}
	delete(pools.failStop, pool)
	pools.occupiedSlots[pool] = 1
	if err := manager.reconcileMaintained(ctx); err != nil {
		t.Fatal(err)
	}
	if pools.records[pool].State != "DRAINING" {
		t.Fatal("occupied execution was not retained for drain")
	}
	pools.occupiedSlots[pool] = 0
	if err := manager.reconcileMaintained(ctx); err != nil {
		t.Fatal(err)
	}
	if records, _ := pools.ListForService(id); len(records) != 0 {
		t.Fatalf("retirement left pools: %#v", records)
	}
	if len(manager.takeMaintenance(256)) != 0 {
		t.Fatal("completed retirement stayed queued")
	}
}
