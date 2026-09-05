package workers

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"the8020/kernel/execution"
	"the8020/kernel/execution/supervisor"
	"the8020/kernel/nodes"
	"the8020/kernel/sandbox/manager"
	"the8020/kernel/sandbox/model"
)

type fakeSandboxes struct {
	items      []manager.Inspection
	listCalls  int
	inspectIDs []string
	resolveIDs []string
}

func (f *fakeSandboxes) ResolveRuntimeGroup(id string) (model.SandboxSpec, error) {
	f.resolveIDs = append(f.resolveIDs, id)
	for _, item := range f.items {
		if item.Spec.RuntimeGroupID == id {
			return item.Spec, nil
		}
	}
	return model.SandboxSpec{}, context.Canceled
}

func (f *fakeSandboxes) List() ([]manager.Inspection, error) {
	f.listCalls++
	result := append([]manager.Inspection(nil), f.items...)
	for index := range result {
		if result[index].Status.ObservedState == "" {
			result[index].Status.ObservedState = model.StateReady
		}
	}
	return result, nil
}
func (f *fakeSandboxes) Inspect(_ context.Context, id string) (manager.Inspection, error) {
	f.inspectIDs = append(f.inspectIDs, id)
	for _, item := range f.items {
		if item.Spec.RuntimeGroupID == id || item.Spec.SandboxID == id {
			if item.Status.ObservedState == "" {
				item.Status.ObservedState = model.StateReady
			}
			return item, nil
		}
	}
	return manager.Inspection{}, context.Canceled
}

type fakeControl struct {
	workers      map[string][]supervisor.WorkerStatus
	lists        int
	immediate    bool
	stoppedIn    string
	started      supervisor.StartWorkerRequest
	invokedIn    model.SandboxSpec
	invokedID    string
	persistentID string
	function     string
	input        any
	user         execution.User
	invoke       supervisor.WorkerInvocationResult
	invokeErr    error
}

func jobMetadata(workerID, executionID string) supervisor.ExecutionMetadata {
	return supervisor.ExecutionMetadata{
		WorkerID: workerID, ExecutionID: executionID, WorkloadType: model.WorkloadJob,
		OwnerID: "job", Entrypoint: "file:///programs/main.ts",
		User: execution.SystemUser(), Origin: execution.Origin{Type: execution.OriginJob, ID: "job"},
	}
}

func (f *fakeControl) Workers(_ context.Context, spec model.SandboxSpec) ([]supervisor.WorkerStatus, error) {
	f.lists++
	return f.workers[spec.RuntimeGroupID], nil
}
func (f *fakeControl) StartWorker(_ context.Context, _ model.SandboxSpec, request supervisor.StartWorkerRequest) (supervisor.WorkerStatus, error) {
	f.started = request
	return supervisor.WorkerStatus{WorkerID: request.Metadata.WorkerID}, nil
}
func (f *fakeControl) StopWorker(_ context.Context, spec model.SandboxSpec, _ string, immediate bool) error {
	f.immediate = immediate
	f.stoppedIn = spec.RuntimeGroupID
	return nil
}
func (f *fakeControl) InvokeWorker(_ context.Context, spec model.SandboxSpec, workerID, persistentID, function string, input any, user execution.User) (supervisor.WorkerInvocationResult, error) {
	f.invokedIn, f.invokedID, f.persistentID, f.function, f.input, f.user = spec, workerID, persistentID, function, input, user
	if f.invokeErr != nil {
		return supervisor.WorkerInvocationResult{}, f.invokeErr
	}
	if f.invoke.OK || f.invoke.Error != nil {
		return f.invoke, nil
	}
	return supervisor.WorkerInvocationResult{OK: true, Output: "controlled"}, nil
}
func (f *fakeControl) RunJob(context.Context, model.SandboxSpec, string, []any, map[string]string, []string) (supervisor.JobResult, error) {
	return supervisor.JobResult{Result: "job"}, nil
}
func (f *fakeControl) ConfigureService(context.Context, model.SandboxSpec, string, []string, int) error {
	return nil
}
func (f *fakeControl) ServiceOpenAPI(context.Context, model.SandboxSpec, string) (map[string]any, error) {
	return map[string]any{"openapi": "3.1.0"}, nil
}
func (f *fakeControl) DispatchService(context.Context, model.SandboxSpec, string, *http.Request) (*http.Response, error) {
	return nil, nil
}
func (f *fakeControl) ProxyServiceWebSocket(context.Context, model.SandboxSpec, string, http.ResponseWriter, *http.Request, func(*http.Response) error) error {
	return nil
}
func TestWorkerValidationLookupAndTermination(t *testing.T) {
	spec := model.SandboxSpec{SandboxID: "sandbox", RuntimeGroupID: "group", WorkloadType: model.WorkloadJob, DependencyMode: model.DependencyCachedOnly, Permissions: model.Permissions{ReadPaths: []string{"/programs"}, WritePaths: []string{"/data"}, SystemInfo: true}}
	sandboxes := &fakeSandboxes{items: []manager.Inspection{{Spec: spec, Workers: []supervisor.WorkerStatus{{WorkerID: "worker", ExecutionID: "execution"}}}}}
	control := &fakeControl{workers: map[string][]supervisor.WorkerStatus{"group": {{WorkerID: "worker", ExecutionID: "execution"}}}}
	manager, err := New(sandboxes, control, 0, 64, "sqlite")
	if err != nil {
		t.Fatal(err)
	}
	request := supervisor.StartWorkerRequest{Metadata: jobMetadata("new-worker", "new-execution"), Permissions: supervisor.WorkerPermissions{Read: []string{"/programs/module"}, Write: []string{"/data/file"}, Sys: []string{"hostname"}}}
	started, err := manager.Start(context.Background(), "group", request)
	if err != nil {
		t.Fatal(err)
	}
	if started.Worker.WorkerID != "new-worker" || !strings.HasPrefix(control.started.Metadata.DebuggerName, "job:job:new-execution:new-worker") || control.started.Metadata.DatabaseBackend != "sqlite" {
		t.Fatalf("started=%#v request=%#v", started, control.started)
	}
	items, err := manager.List(context.Background(), "sandbox")
	if err != nil || len(items) != 2 {
		t.Fatalf("list=%#v err=%v", items, err)
	}
	if err := manager.Stop(context.Background(), "worker", true); err != nil || !control.immediate {
		t.Fatalf("kill err=%v immediate=%v", err, control.immediate)
	}
	listsBefore := control.lists
	if err := manager.StopInGroup(context.Background(), "group", "worker", false); err != nil || control.stoppedIn != "group" || control.lists != listsBefore {
		t.Fatalf("group-scoped stop err=%v group=%q lists=%d before=%d", err, control.stoppedIn, control.lists, listsBefore)
	}
	request.Permissions.Read = []string{"/host"}
	if _, err := manager.Start(context.Background(), "group", request); err == nil {
		t.Fatal("permission escalation accepted")
	}
	request.Permissions.Read = nil
	request.Metadata.Entrypoint = "file:///outside/main.ts"
	if _, err := manager.Start(context.Background(), "group", request); err == nil {
		t.Fatal("entrypoint escape accepted")
	}
}

func TestFilteredWorkerListResolvesOnlyTheExactRuntimeGroup(t *testing.T) {
	spec := model.SandboxSpec{SandboxID: "sandbox", RuntimeGroupID: "group", WorkloadType: model.WorkloadService}
	sandboxes := &fakeSandboxes{items: []manager.Inspection{{Spec: spec, Workers: []supervisor.WorkerStatus{{WorkerID: "worker"}}}}}
	control := &fakeControl{workers: map[string][]supervisor.WorkerStatus{"group": {{WorkerID: "worker"}}}}
	manager, err := New(sandboxes, control, 0, 64, "sqlite")
	if err != nil {
		t.Fatal(err)
	}
	items, err := manager.List(context.Background(), "group")
	if err != nil || len(items) != 1 || items[0].Worker.WorkerID != "worker" || control.lists != 0 {
		t.Fatalf("items=%#v lists=%d err=%v", items, control.lists, err)
	}
	if _, err := manager.List(context.Background(), "missing"); err == nil || (!errors.Is(err, context.Canceled) && !errors.Is(err, os.ErrNotExist)) {
		t.Fatalf("missing exact group error=%v", err)
	}
	if control.lists != 0 {
		t.Fatalf("missing exact group scanned supervisor Workers: %d", control.lists)
	}
}

func TestWorkerListingSkipsTerminalGroupsAndClassifiesExactUnavailability(t *testing.T) {
	ready := manager.Inspection{
		Spec:    model.SandboxSpec{SandboxID: "ready-sandbox", RuntimeGroupID: "ready-group", WorkloadType: model.WorkloadService},
		Status:  model.SandboxStatus{ObservedState: model.StateReady},
		Workers: []supervisor.WorkerStatus{{WorkerID: "ready-worker"}},
	}
	stopped := manager.Inspection{
		Spec:   model.SandboxSpec{SandboxID: "stopped-sandbox", RuntimeGroupID: "stopped-group", WorkloadType: model.WorkloadService},
		Status: model.SandboxStatus{ObservedState: model.StateStopped},
	}
	sandboxes := &fakeSandboxes{items: []manager.Inspection{stopped, ready}}
	control := &fakeControl{workers: map[string][]supervisor.WorkerStatus{
		"ready-group": {{WorkerID: "ready-worker"}},
	}}
	workerManager, err := New(sandboxes, control, 0, 64, "sqlite")
	if err != nil {
		t.Fatal(err)
	}
	listed, err := workerManager.List(context.Background(), "")
	if err != nil || len(listed) != 1 || listed[0].Worker.WorkerID != "ready-worker" || control.lists != 0 {
		t.Fatalf("listed=%#v supervisor lists=%d err=%v", listed, control.lists, err)
	}
	if _, err := workerManager.List(context.Background(), "stopped-group"); !errors.Is(err, ErrRuntimeUnavailable) {
		t.Fatalf("terminal exact-list error=%v", err)
	}
	if control.lists != 0 {
		t.Fatalf("terminal group contacted its supervisor: %d", control.lists)
	}
}

func TestWorkerStartEnforcesNodeMaximum(t *testing.T) {
	spec := model.SandboxSpec{SandboxID: "sandbox", RuntimeGroupID: "group", WorkloadType: model.WorkloadJob, DependencyMode: model.DependencyCachedOnly, Permissions: model.Permissions{ReadPaths: []string{"/programs"}}}
	sandboxes := &fakeSandboxes{items: []manager.Inspection{{Spec: spec, Workers: []supervisor.WorkerStatus{{WorkerID: "existing"}}}}}
	control := &fakeControl{workers: map[string][]supervisor.WorkerStatus{"group": {{WorkerID: "existing"}}}}
	manager, err := New(sandboxes, control, 1, 64, "sqlite")
	if err != nil {
		t.Fatal(err)
	}
	request := supervisor.StartWorkerRequest{Metadata: jobMetadata("next", "execution")}
	if _, err := manager.Start(context.Background(), "group", request); err == nil || !strings.Contains(err.Error(), "Worker capacity") {
		t.Fatalf("start error=%v", err)
	} else if !errors.Is(err, ErrNodeCapacity) {
		t.Fatalf("start error is not typed: %v", err)
	}
}

func TestWorkerStartEnforcesSandboxWorkerMaximum(t *testing.T) {
	request := supervisor.StartWorkerRequest{Metadata: jobMetadata("next", "execution")}
	spec := model.SandboxSpec{SandboxID: "sandbox", RuntimeGroupID: "group", WorkloadType: model.WorkloadJob, DependencyMode: model.DependencyCachedOnly, Permissions: model.Permissions{ReadPaths: []string{"/programs"}}}
	sandboxes := &fakeSandboxes{items: []manager.Inspection{{Spec: spec, Workers: []supervisor.WorkerStatus{{WorkerID: "first"}, {WorkerID: "second"}}}}}
	control := &fakeControl{workers: map[string][]supervisor.WorkerStatus{"group": {{WorkerID: "first"}, {WorkerID: "second"}}}}
	manager, err := New(sandboxes, control, 0, 2, "sqlite")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Start(context.Background(), "group", request); err == nil || !strings.Contains(err.Error(), "sandbox Worker capacity") {
		t.Fatalf("start error=%v", err)
	} else if !errors.Is(err, ErrSandboxCapacity) {
		t.Fatalf("start error is not typed: %v", err)
	}
}

func TestSuccessfulWorkerReservationPersistsUntilANewerSnapshotObservesIt(t *testing.T) {
	spec := model.SandboxSpec{SandboxID: "sandbox", RuntimeGroupID: "group", WorkloadType: model.WorkloadJob, DependencyMode: model.DependencyCachedOnly, Permissions: model.Permissions{ReadPaths: []string{"/programs"}}}
	sandboxes := &fakeSandboxes{items: []manager.Inspection{{Spec: spec}}}
	control := &fakeControl{}
	workerManager, err := New(sandboxes, control, 1, 64, "sqlite")
	if err != nil {
		t.Fatal(err)
	}
	request := supervisor.StartWorkerRequest{Metadata: jobMetadata("first", "execution-first")}
	if _, err := workerManager.Start(context.Background(), spec.RuntimeGroupID, request); err != nil {
		t.Fatal(err)
	}
	workerManager.capacityMu.Lock()
	workerManager.provisional["first"] = provisionalWorker{runtimeGroupID: spec.RuntimeGroupID, startedAt: time.Now().Add(-time.Hour), worker: supervisor.WorkerStatus{WorkerID: "first"}}
	workerManager.capacityMu.Unlock()
	request.Metadata.WorkerID = "second"
	request.Metadata.ExecutionID = "execution-second"
	if _, err := workerManager.Start(context.Background(), spec.RuntimeGroupID, request); !errors.Is(err, ErrNodeCapacity) {
		t.Fatalf("aged but unobserved Worker reservation did not enforce the hard limit: %v", err)
	}
	sandboxes.items[0].Runtime.ObservedAt = time.Now().Add(time.Second)
	if _, err := workerManager.Start(context.Background(), spec.RuntimeGroupID, request); err != nil {
		t.Fatalf("newer absolute snapshot did not clear the absent provisional Worker: %v", err)
	}
}

func TestResourceObservationsDoNotRejectWorkerAdmission(t *testing.T) {
	spec := model.SandboxSpec{SandboxID: "sandbox", RuntimeGroupID: "group", WorkloadType: model.WorkloadJob, DependencyMode: model.DependencyCachedOnly, Permissions: model.Permissions{ReadPaths: []string{"/programs"}}}
	sandboxes := &fakeSandboxes{items: []manager.Inspection{{Spec: spec, Status: model.SandboxStatus{Metrics: model.ResourceMetrics{CPUUsageMicros: 1 << 60, MemoryCurrent: 1 << 60}}, Workers: []supervisor.WorkerStatus{{WorkerID: "existing"}}}}}
	control := &fakeControl{workers: map[string][]supervisor.WorkerStatus{"group": {{WorkerID: "existing"}}}}
	manager, err := New(sandboxes, control, 0, 2, "sqlite")
	if err != nil {
		t.Fatal(err)
	}
	request := supervisor.StartWorkerRequest{Metadata: jobMetadata("first", "execution")}
	if _, err := manager.Start(context.Background(), "group", request); err != nil {
		t.Fatalf("resource observations rejected Worker admission: %v", err)
	}
}

func TestWorkerJobDelegationUsesTheExactWorker(t *testing.T) {
	spec := model.SandboxSpec{SandboxID: "sandbox", RuntimeGroupID: "group", WorkloadType: model.WorkloadJob, Permissions: model.Permissions{ReadPaths: []string{"/workspace/packages"}}}
	control := &fakeControl{workers: map[string][]supervisor.WorkerStatus{"group": {{WorkerID: "worker"}}}}
	manager, _ := New(&fakeSandboxes{items: []manager.Inspection{{Spec: spec, Workers: []supervisor.WorkerStatus{{WorkerID: "worker"}}}}}, control, 0, 64, "sqlite")
	if output, err := manager.RunJob(context.Background(), "worker", nil, nil, []string{"/workspace/packages/example/table.ts"}); err != nil || output.Result != "job" {
		t.Fatalf("job=%#v err=%v", output, err)
	}
	if _, err := manager.RunJob(context.Background(), "worker", nil, nil, []string{"/private/table.ts"}); err == nil {
		t.Fatal("out-of-envelope type-check module accepted")
	}
}

type fakeNodeRouter struct {
	local     string
	forwarded []nodes.WorkerInvocationRequest
	result    nodes.WorkerInvocationResult
}

func (f *fakeNodeRouter) LocalNodeID() string { return f.local }
func (f *fakeNodeRouter) InvokeWorker(_ context.Context, input nodes.WorkerInvocationRequest) nodes.WorkerInvocationResult {
	f.forwarded = append(f.forwarded, input)
	return f.result
}

func TestWorkerInvocationTargetsOneExactLocalWorker(t *testing.T) {
	spec := model.SandboxSpec{SandboxID: "sandbox-a", RuntimeGroupID: "group-a", WorkloadType: model.WorkloadService}
	control := &fakeControl{}
	manager, _ := New(&fakeSandboxes{items: []manager.Inspection{{Spec: spec}}}, control, 0, 64, "sqlite")
	router := &fakeNodeRouter{local: "node-a"}
	manager.SetNodeRouter(router)
	input := nodes.WorkerInvocationRequest{NodeID: "node-a", SandboxID: "sandbox-a", WorkerID: "worker-a", Function: "example.inspect", Input: map[string]any{"id": float64(7)}, User: execution.SystemUser()}
	result := manager.InvokeWorker(context.Background(), input)
	if !result.OK || result.Output != "controlled" || control.invokedIn.SandboxID != "sandbox-a" || control.invokedID != "worker-a" || control.function != "example.inspect" {
		t.Fatalf("result=%#v control=%#v", result, control)
	}
	if value, ok := control.input.(map[string]any)["id"]; !ok || value != float64(7) {
		t.Fatalf("opaque input=%#v", control.input)
	}
	if len(router.forwarded) != 0 {
		t.Fatalf("local invocation was forwarded: %#v", router.forwarded)
	}

	control.invoke = supervisor.WorkerInvocationResult{Error: &supervisor.WorkerInvocationError{Code: "function_not_found", Message: "not registered"}}
	result = manager.InvokeWorker(context.Background(), input)
	if result.Error == nil || result.Error.Code != "function_not_found" {
		t.Fatalf("structured function error=%#v", result)
	}
	control.invoke = supervisor.WorkerInvocationResult{}
	control.invokeErr = context.DeadlineExceeded
	result = manager.InvokeWorker(context.Background(), input)
	if result.Error == nil || result.Error.Code != "timeout" {
		t.Fatalf("timeout result=%#v", result)
	}
}

func TestWorkerInvocationRejectsMismatchedExactTarget(t *testing.T) {
	spec := model.SandboxSpec{SandboxID: "sandbox-a", RuntimeGroupID: "group-a", WorkloadType: model.WorkloadService}
	control := &fakeControl{}
	manager, _ := New(&fakeSandboxes{items: []manager.Inspection{{Spec: spec}}}, control, 0, 64, "sqlite")
	manager.SetNodeRouter(&fakeNodeRouter{local: "node-a"})
	base := nodes.WorkerInvocationRequest{NodeID: "node-a", SandboxID: "sandbox-a", WorkerID: "worker-a", Function: "example.inspect", Input: nil, User: execution.SystemUser()}

	nodeMismatch := base
	nodeMismatch.NodeID = "node-b"
	if result := manager.InvokeLocalWorker(context.Background(), nodeMismatch); result.Error == nil || result.Error.Code != "target_mismatch" {
		t.Fatalf("node mismatch=%#v", result)
	}
	sandboxMismatch := base
	sandboxMismatch.SandboxID = "group-a"
	if result := manager.InvokeLocalWorker(context.Background(), sandboxMismatch); result.Error == nil || result.Error.Code != "target_mismatch" {
		t.Fatalf("sandbox mismatch=%#v", result)
	}
	missing := base
	missing.SandboxID = "sandbox-missing"
	if result := manager.InvokeLocalWorker(context.Background(), missing); result.Error == nil || result.Error.Code != "target_not_found" {
		t.Fatalf("missing sandbox=%#v", result)
	}
	invalid := base
	invalid.WorkerID = ""
	if result := manager.InvokeLocalWorker(context.Background(), invalid); result.Error == nil || result.Error.Code != "invalid_request" {
		t.Fatalf("invalid target=%#v", result)
	}
	if control.invokedID != "" {
		t.Fatalf("mismatched target reached supervisor: %#v", control)
	}
}

func TestWorkerInvocationForwardsOnlyToNamedRemoteNode(t *testing.T) {
	control := &fakeControl{}
	manager, _ := New(&fakeSandboxes{}, control, 0, 64, "sqlite")
	router := &fakeNodeRouter{local: "node-a", result: nodes.WorkerInvocationResult{OK: true, Output: "remote"}}
	manager.SetNodeRouter(router)
	input := nodes.WorkerInvocationRequest{NodeID: "node-b", SandboxID: "sandbox-b", WorkerID: "worker-b", Function: "example.inspect", Input: map[string]any{"value": "opaque"}, User: execution.SystemUser()}
	result := manager.InvokeWorker(context.Background(), input)
	if !result.OK || result.Output != "remote" || len(router.forwarded) != 1 || router.forwarded[0].WorkerID != "worker-b" || control.invokedID != "" {
		t.Fatalf("result=%#v forwarded=%#v control=%#v", result, router.forwarded, control)
	}
	input.User = execution.User{}
	result = manager.InvokeWorker(context.Background(), input)
	if result.Error == nil || result.Error.Code != "invalid_request" || len(router.forwarded) != 1 {
		t.Fatalf("missing execution user forwarded: %#v", result)
	}
}
