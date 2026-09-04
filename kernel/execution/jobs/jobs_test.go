package jobs

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"the8020/kernel/execution"
	"the8020/kernel/execution/coordinator"
	"the8020/kernel/execution/supervisor"
	"the8020/kernel/execution/workers"
	"the8020/kernel/sandbox/manager"
	"the8020/kernel/sandbox/model"
)

type fakeCoordinator struct {
	mu       sync.Mutex
	requests []coordinator.Request
	releases []string
}

func (f *fakeCoordinator) Ensure(_ context.Context, request coordinator.Request) (manager.Inspection, error) {
	f.mu.Lock()
	f.requests = append(f.requests, request)
	f.mu.Unlock()
	return manager.Inspection{Spec: model.SandboxSpec{
		SandboxID: "sandbox", RuntimeGroupID: "group", WorkloadType: model.WorkloadJob,
		Permissions: model.Permissions{ReadPaths: []string{"/programs"}},
	}}, nil
}

func (f *fakeCoordinator) Release(_ context.Context, groupID, allocationID, serviceID string) error {
	f.mu.Lock()
	f.releases = append(f.releases, groupID+":"+allocationID+":"+serviceID)
	f.mu.Unlock()
	return nil
}

type fakeWorkers struct {
	mu            sync.Mutex
	starts        []supervisor.StartWorkerRequest
	stops         []string
	runs          int
	arguments     []any
	secretCopy    map[string]string
	secretRef     map[string]string
	failure       error
	result        *supervisor.JobResult
	gate          <-chan struct{}
	runStarted    chan struct{}
	startBlocking bool
	startFailure  error
}

func (f *fakeWorkers) Start(ctx context.Context, group string, request supervisor.StartWorkerRequest) (workers.Record, error) {
	if f.startBlocking {
		<-ctx.Done()
		return workers.Record{}, ctx.Err()
	}
	if f.startFailure != nil {
		return workers.Record{}, f.startFailure
	}
	f.mu.Lock()
	f.starts = append(f.starts, request)
	f.mu.Unlock()
	return workers.Record{RuntimeGroupID: group, Worker: supervisor.WorkerStatus{WorkerID: request.Metadata.WorkerID}}, nil
}

func (f *fakeWorkers) RunJob(ctx context.Context, _ string, arguments []any, secrets map[string]string, _ []string) (supervisor.JobResult, error) {
	f.mu.Lock()
	f.runs++
	f.arguments = append([]any(nil), arguments...)
	f.secretRef = secrets
	f.secretCopy = copySecrets(secrets)
	started := f.runStarted
	gate := f.gate
	f.mu.Unlock()
	if started != nil {
		select {
		case started <- struct{}{}:
		default:
		}
	}
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return supervisor.JobResult{}, ctx.Err()
		}
	}
	if f.failure != nil {
		return supervisor.JobResult{}, f.failure
	}
	if f.result != nil {
		return *f.result, nil
	}
	return supervisor.JobResult{
		Result:             arguments,
		Logs:               []supervisor.LogEvent{{Level: "info", Message: "job output"}},
		ModuleDependencies: map[string][]string{"entry": {"dependency"}},
	}, nil
}

func TestSecureValuesAreRedactedFromResultsLogsAndFailures(t *testing.T) {
	const password = "test-password-never-visible"
	workersFake := &fakeWorkers{result: &supervisor.JobResult{
		Result: map[string]any{"nested": []any{"prefix " + password}},
		Logs: []supervisor.LogEvent{{
			Level: "error", Message: "failed with " + password,
			Fields: map[string]any{"detail": password},
		}},
	}}
	manager, _ := New(&fakeCoordinator{}, workersFake, testPolicy())
	record, err := manager.Run(context.Background(), "secure", "file:///programs/secure.ts", Options{Secrets: map[string]string{"password": password}})
	if err != nil {
		t.Fatal(err)
	}
	if rendered := fmt.Sprintf("%#v", record); strings.Contains(rendered, password) || !strings.Contains(rendered, "[secure input]") {
		t.Fatalf("unredacted successful record: %s", rendered)
	}

	workersFake.result = nil
	workersFake.failure = &supervisor.ResponseError{
		StatusCode: 400, Status: "400 Bad Request", Code: "invalid_arguments",
		Message: "program rejected " + password,
		Details: map[string]any{"reason": password},
	}
	record, err = manager.Run(context.Background(), "secure", "file:///programs/secure.ts", Options{Secrets: map[string]string{"password": password}})
	if err == nil || strings.Contains(err.Error(), password) || strings.Contains(record.Failure, password) {
		t.Fatalf("unredacted failure: record=%#v error=%v", record, err)
	}
	var response *supervisor.ResponseError
	if !errors.As(err, &response) || response.Code != "invalid_arguments" || response.Details["reason"] != "[secure input]" {
		t.Fatalf("structured failure = %#v, %v", response, err)
	}
}

func TestSecretFreeFailurePreservesItsCause(t *testing.T) {
	workersFake := &fakeWorkers{failure: context.DeadlineExceeded}
	manager, err := New(&fakeCoordinator{}, workersFake, testPolicy())
	if err != nil {
		t.Fatal(err)
	}
	_, err = manager.Run(context.Background(), "deadline", "file:///programs/deadline.ts", Options{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("job failure lost its cause: %v", err)
	}
}

func (*fakeWorkers) List(context.Context, string) ([]workers.Record, error) { return nil, nil }

func (f *fakeWorkers) StopInGroup(_ context.Context, _ string, workerID string, _ bool) error {
	f.mu.Lock()
	f.stops = append(f.stops, workerID)
	f.mu.Unlock()
	return nil
}

func TestOneTimeJobReturnsOutputWithoutRetainingHistory(t *testing.T) {
	coordinatorFake := &fakeCoordinator{}
	workersFake := &fakeWorkers{}
	manager, err := New(coordinatorFake, workersFake, testPolicy())
	if err != nil {
		t.Fatal(err)
	}
	passwords := map[string]string{"password": "do-not-persist"}
	record, err := manager.Run(context.Background(), "job", "file:///programs/job.ts", Options{
		Arguments: []any{"Alice Smith", "--admin"}, Secrets: passwords,
	})
	if err != nil {
		t.Fatal(err)
	}
	if record.State != "SUCCEEDED" || len(record.Logs) != 1 || !reflect.DeepEqual(record.Result, []any{"Alice Smith", "--admin"}) {
		t.Fatalf("record = %#v", record)
	}
	items, err := manager.List()
	if err != nil || len(items) != 0 {
		t.Fatalf("live jobs = %#v, %v", items, err)
	}
	if _, err := manager.Inspect(record.ExecutionID); err == nil {
		t.Fatal("completed one-time job was retained")
	}
	workersFake.mu.Lock()
	defer workersFake.mu.Unlock()
	if !reflect.DeepEqual(workersFake.arguments, []any{"Alice Smith", "--admin"}) || workersFake.secretCopy["password"] != "do-not-persist" || len(workersFake.secretRef) != 0 {
		t.Fatalf("arguments=%#v copied secrets=%#v live secrets=%#v", workersFake.arguments, workersFake.secretCopy, workersFake.secretRef)
	}
	if passwords["password"] != "do-not-persist" {
		t.Fatal("caller-owned secret map was modified")
	}
	coordinatorFake.mu.Lock()
	defer coordinatorFake.mu.Unlock()
	if len(coordinatorFake.releases) != 1 || coordinatorFake.releases[0] != "group:"+record.WorkerID+":" {
		t.Fatalf("runtime releases = %#v", coordinatorFake.releases)
	}
}

func TestStructuredArgumentArrayIsPassedUnchanged(t *testing.T) {
	workersFake := &fakeWorkers{}
	manager, _ := New(&fakeCoordinator{}, workersFake, testPolicy())
	input := map[string]any{"table": "users"}
	if _, err := manager.Run(context.Background(), "hook", "file:///programs/hook.ts", Options{Arguments: []any{input}}); err != nil {
		t.Fatal(err)
	}
	workersFake.mu.Lock()
	defer workersFake.mu.Unlock()
	if len(workersFake.arguments) != 1 || !reflect.DeepEqual(workersFake.arguments[0], input) {
		t.Fatalf("arguments = %#v", workersFake.arguments)
	}
}

func TestCompatibleReuseRetainsOnlyIdleWorkerMetadata(t *testing.T) {
	workersFake := &fakeWorkers{}
	policy := testPolicy()
	policy.Reuse = true
	policy.IdleRuntimeTimeout = time.Hour
	manager, _ := New(&fakeCoordinator{}, workersFake, policy)
	defer manager.Close()
	first, err := manager.Run(context.Background(), "job", "file:///programs/job.ts", Options{Arguments: []any{"one"}})
	if err != nil || first.State != "IDLE" {
		t.Fatalf("first=%#v err=%v", first, err)
	}
	live, err := manager.Inspect(first.ExecutionID)
	if err != nil || live.Result != nil || live.Logs != nil || live.ModuleDependencies != nil {
		t.Fatalf("live idle record=%#v err=%v", live, err)
	}
	second, err := manager.Run(context.Background(), "job", "file:///programs/job.ts", Options{Arguments: []any{"two"}})
	if err != nil || second.WorkerID != first.WorkerID || second.ExecutionID == first.ExecutionID {
		t.Fatalf("first=%#v second=%#v err=%v", first, second, err)
	}
	if _, err := manager.Inspect(first.ExecutionID); err == nil {
		t.Fatal("superseded idle execution remained live")
	}
	workersFake.mu.Lock()
	defer workersFake.mu.Unlock()
	if len(workersFake.starts) != 1 || workersFake.runs != 2 {
		t.Fatalf("starts=%d runs=%d", len(workersFake.starts), workersFake.runs)
	}
}

func TestJobQueueIsBoundedAndCancellationDoesNotStartWorker(t *testing.T) {
	gate := make(chan struct{})
	started := make(chan struct{}, 1)
	workersFake := &fakeWorkers{gate: gate, runStarted: started}
	policy := testPolicy()
	policy.MaximumParallel = 1
	policy.QueuedExecutionLimit = 1
	manager, _ := New(&fakeCoordinator{}, workersFake, policy)
	defer manager.Close()
	first, err := manager.Run(context.Background(), "first", "file:///programs/job.ts", Options{Detached: true})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first job did not start")
	}
	queued, err := manager.Run(context.Background(), "queued", "file:///programs/job.ts", Options{Detached: true})
	if err != nil || queued.State != "QUEUED" {
		t.Fatalf("queued=%#v err=%v", queued, err)
	}
	if _, err := manager.Run(context.Background(), "overflow", "file:///programs/job.ts", Options{Detached: true}); err == nil {
		t.Fatal("queue limit was not enforced")
	}
	if err := manager.Cancel(context.Background(), queued.ExecutionID); err != nil {
		t.Fatal(err)
	}
	close(gate)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		items, _ := manager.List()
		if len(items) == 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	workersFake.mu.Lock()
	defer workersFake.mu.Unlock()
	if len(workersFake.starts) != 1 {
		t.Fatalf("starts = %d", len(workersFake.starts))
	}
	_ = first
}

func TestSynchronousChildDoesNotQueueBehindItsWaitingParent(t *testing.T) {
	coordinatorFake := &fakeCoordinator{}
	workersFake := &fakeWorkers{}
	policy := testPolicy()
	policy.MaximumParallel = 1
	manager, _ := New(coordinatorFake, workersFake, policy)
	manager.records["parent"] = Record{ExecutionID: "parent", JobID: "parent-job", State: "RUNNING", Parallelism: 1}
	ctx := execution.WithCaller(context.Background(), execution.Caller{ExecutionID: "parent", Workload: model.WorkloadJob})
	record, err := manager.Run(ctx, "child-job", "file:///programs/child.ts", Options{Parallelism: 1})
	if err != nil || record.State != "SUCCEEDED" {
		t.Fatalf("child=%#v err=%v", record, err)
	}
}

func TestParallelismAppliesPerLogicalJob(t *testing.T) {
	manager, _ := New(&fakeCoordinator{}, &fakeWorkers{}, testPolicy())
	manager.records["same"] = Record{ExecutionID: "same", JobID: "one", State: "RUNNING"}
	manager.records["other"] = Record{ExecutionID: "other", JobID: "other", State: "RUNNING"}
	if manager.canStartLocked(Record{JobID: "one", Parallelism: 1}) {
		t.Fatal("same logical job exceeded its parallelism")
	}
	if !manager.canStartLocked(Record{JobID: "different", Parallelism: 1}) {
		t.Fatal("unrelated logical jobs incorrectly consumed the per-job limit")
	}
}

func TestJobTimeoutIncludesWorkerStartup(t *testing.T) {
	workersFake := &fakeWorkers{startBlocking: true}
	manager, _ := New(&fakeCoordinator{}, workersFake, testPolicy())
	record, err := manager.Run(context.Background(), "slow", "file:///programs/slow.ts", Options{Timeout: 20 * time.Millisecond})
	if !errors.Is(err, context.DeadlineExceeded) || record.State != "FAILED" {
		t.Fatalf("record=%#v err=%v", record, err)
	}
	items, _ := manager.List()
	if len(items) != 0 {
		t.Fatalf("failed history retained: %#v", items)
	}
}

func TestWorkerStartFailureReleasesItsSandboxClaim(t *testing.T) {
	coordinatorFake := &fakeCoordinator{}
	workersFake := &fakeWorkers{startFailure: errors.New("start failed")}
	manager, _ := New(coordinatorFake, workersFake, testPolicy())
	record, err := manager.Run(context.Background(), "broken", "file:///programs/broken.ts", Options{})
	if err == nil || record.State != "FAILED" {
		t.Fatalf("record=%#v err=%v", record, err)
	}
	coordinatorFake.mu.Lock()
	defer coordinatorFake.mu.Unlock()
	if len(coordinatorFake.releases) != 1 || coordinatorFake.releases[0] != "group:"+record.WorkerID+":" {
		t.Fatalf("runtime releases = %#v", coordinatorFake.releases)
	}
}

func TestReusableWorkerReleasesItsClaimAfterIdleTimeout(t *testing.T) {
	coordinatorFake := &fakeCoordinator{}
	workersFake := &fakeWorkers{}
	policy := testPolicy()
	policy.Reuse = true
	policy.IdleRuntimeTimeout = time.Millisecond
	manager, _ := New(coordinatorFake, workersFake, policy)
	if _, err := manager.Run(context.Background(), "reusable", "file:///programs/reusable.ts", Options{}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		coordinatorFake.mu.Lock()
		released := len(coordinatorFake.releases) == 1
		coordinatorFake.mu.Unlock()
		if released {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("idle reusable Worker did not release its sandbox claim")
}

func TestJobUsesExplicitOwnerAndRelease(t *testing.T) {
	coordinatorFake, workersFake := &fakeCoordinator{}, &fakeWorkers{}
	manager, _ := New(coordinatorFake, workersFake, testPolicy())
	if _, err := manager.Run(context.Background(), "logical-job", "file:///programs/job.ts", Options{OwnerID: "the8020/users", Namespace: "the8020", ReleaseID: "commit"}); err != nil {
		t.Fatal(err)
	}
	coordinatorFake.mu.Lock()
	defer coordinatorFake.mu.Unlock()
	if coordinatorFake.requests[0].OwnerID != "the8020/users" || coordinatorFake.requests[0].Namespace != "the8020" {
		t.Fatalf("request = %#v", coordinatorFake.requests[0])
	}
	if coordinatorFake.requests[0].AllocationID == "" || coordinatorFake.requests[0].AllocationID != workersFake.starts[0].Metadata.WorkerID {
		t.Fatalf("allocation request = %#v", coordinatorFake.requests[0])
	}
	if len(coordinatorFake.releases) != 1 || coordinatorFake.releases[0] != "group:"+coordinatorFake.requests[0].AllocationID+":" {
		t.Fatalf("runtime releases = %#v", coordinatorFake.releases)
	}
	workersFake.mu.Lock()
	defer workersFake.mu.Unlock()
	metadata := workersFake.starts[0].Metadata
	if metadata.OwnerID != "the8020/users" || metadata.WorkloadID != "logical-job" || metadata.ReleaseID != "commit" {
		t.Fatalf("metadata = %#v", metadata)
	}
}

func testPolicy() Policy {
	return Policy{
		Strategy: model.GroupingOwner,
		Profile: model.RuntimeProfile{
			WorkloadType:   model.WorkloadJob,
			ImageDigest:    "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			DependencyMode: model.DependencyCachedOnly,
			Permissions:    model.Permissions{ReadPaths: []string{"/programs"}},
			NetworkMode:    "netstack", ResourceClass: "job",
		},
	}
}
