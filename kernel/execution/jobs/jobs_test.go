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
	"the8020/kernel/identity"
	"the8020/kernel/sandbox/manager"
	"the8020/kernel/sandbox/model"
)

type fakeCoordinator struct {
	mu           sync.Mutex
	requests     []coordinator.Request
	failure      error
	beforeEnsure func()
	releases     []string
}

func (f *fakeCoordinator) Ensure(_ context.Context, request coordinator.Request) (manager.Inspection, error) {
	if f.beforeEnsure != nil {
		f.beforeEnsure()
	}
	f.mu.Lock()
	f.requests = append(f.requests, request)
	f.mu.Unlock()
	return manager.Inspection{Spec: model.SandboxSpec{
		SandboxID: "sbx-0123456789", WorkloadType: model.WorkloadJob,
		Permissions: model.Permissions{ReadPaths: []string{"/programs"}},
	}}, f.failure
}

func (f *fakeCoordinator) Release(_ context.Context, sandboxID, allocationID, serviceID string) error {
	f.mu.Lock()
	f.releases = append(f.releases, sandboxID+":"+allocationID+":"+serviceID)
	f.mu.Unlock()
	return nil
}

type fakeWorkers struct {
	mu            sync.Mutex
	starts        []supervisor.StartWorkerRequest
	stops         []string
	invocations   []execution.Invocation
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
	return workers.Record{SandboxID: group, Worker: supervisor.WorkerStatus{WorkerID: request.Metadata.WorkerID}}, nil
}

func (f *fakeWorkers) RunJob(ctx context.Context, _ string, arguments []any, secrets map[string]string, _ []string) (supervisor.JobResult, error) {
	f.mu.Lock()
	invocation, ok := execution.InvocationFromContext(ctx)
	if !ok {
		f.mu.Unlock()
		return supervisor.JobResult{}, errors.New("missing job invocation")
	}
	f.invocations = append(f.invocations, invocation)
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
		ModuleDependencies: map[string][]string{"entry": {"dependency"}},
	}, nil
}

func TestSecureValuesAreRedactedFromResultsAndFailures(t *testing.T) {
	const password = "test-password-never-visible"
	workersFake := &fakeWorkers{result: &supervisor.JobResult{
		Result: map[string]any{"nested": []any{"prefix " + password}},
	}}
	manager, _ := New(&fakeCoordinator{}, workersFake, testPolicy())
	record, err := manager.Run(context.Background(), "secure", "file:///programs/secure.ts", Options{User: execution.SystemUser(), Secrets: map[string]string{"password": password}})
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
	record, err = manager.Run(context.Background(), "secure", "file:///programs/secure.ts", Options{User: execution.SystemUser(), Secrets: map[string]string{"password": password}})
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
	_, err = manager.Run(context.Background(), "deadline", "file:///programs/deadline.ts", Options{User: execution.SystemUser()})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("job failure lost its cause: %v", err)
	}
}

func (*fakeWorkers) List(context.Context, string) ([]workers.Record, error) { return nil, nil }

func (f *fakeWorkers) StopInSandbox(_ context.Context, _ string, workerID string, _ bool) error {
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
	record, err := manager.Run(context.Background(), "job", "file:///programs/job.ts", Options{User: execution.SystemUser(),
		Arguments: []any{"Alice Smith", "--admin"}, Secrets: passwords,
	})
	if err != nil {
		t.Fatal(err)
	}
	if record.State != "SUCCEEDED" || !reflect.DeepEqual(record.Result, []any{"Alice Smith", "--admin"}) {
		t.Fatalf("record = %#v", record)
	}
	if record.User != execution.SystemUser() || record.Origin != (execution.Origin{Type: execution.OriginModule, ID: "job"}) {
		t.Fatalf("execution identity = user %#v origin %#v", record.User, record.Origin)
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
	if len(workersFake.starts) != 1 || workersFake.starts[0].Metadata.User != execution.SystemUser() || workersFake.starts[0].Metadata.Origin != record.Origin {
		t.Fatalf("Worker execution identity = %#v", workersFake.starts)
	}
	if !reflect.DeepEqual(workersFake.arguments, []any{"Alice Smith", "--admin"}) || workersFake.secretCopy["password"] != "do-not-persist" || len(workersFake.secretRef) != 0 {
		t.Fatalf("arguments=%#v copied secrets=%#v live secrets=%#v", workersFake.arguments, workersFake.secretCopy, workersFake.secretRef)
	}
	if passwords["password"] != "do-not-persist" {
		t.Fatal("caller-owned secret map was modified")
	}
	coordinatorFake.mu.Lock()
	defer coordinatorFake.mu.Unlock()
	if len(coordinatorFake.requests) != 1 || coordinatorFake.requests[0].RequestedWorkers != 1 {
		t.Fatalf("job Worker capacity request = %#v", coordinatorFake.requests)
	}
	if len(coordinatorFake.releases) != 1 || coordinatorFake.releases[0] != "sbx-0123456789:"+record.WorkerID+":" {
		t.Fatalf("runtime releases = %#v", coordinatorFake.releases)
	}
}

func TestStructuredArgumentArrayIsPassedUnchanged(t *testing.T) {
	workersFake := &fakeWorkers{}
	manager, _ := New(&fakeCoordinator{}, workersFake, testPolicy())
	input := map[string]any{"table": "users"}
	if _, err := manager.Run(context.Background(), "hook", "file:///programs/hook.ts", Options{User: execution.SystemUser(), Arguments: []any{input}}); err != nil {
		t.Fatal(err)
	}
	workersFake.mu.Lock()
	defer workersFake.mu.Unlock()
	if len(workersFake.arguments) != 1 || !reflect.DeepEqual(workersFake.arguments[0], input) {
		t.Fatalf("arguments = %#v", workersFake.arguments)
	}
}

func TestPreparedJobCopiesSandboxPlacement(t *testing.T) {
	m, _ := New(&fakeCoordinator{}, &fakeWorkers{}, testPolicy())
	defer m.Close()
	group := "batch"
	prepared, err := m.prepare("job", "file:///programs/job.ts", Options{User: execution.SystemUser(), PlacementGroup: &group})
	if err != nil {
		t.Fatal(err)
	}
	group = "changed"
	if prepared.placementGroup == nil || *prepared.placementGroup != "batch" {
		t.Fatal("prepared job retained caller-owned placement")
	}
}

func TestCompatibleReuseRetainsOnlyIdleWorkerMetadata(t *testing.T) {
	workersFake := &fakeWorkers{}
	policy := testPolicy()
	policy.Reuse = true
	policy.IdleRuntimeTimeout = time.Hour
	manager, _ := New(&fakeCoordinator{}, workersFake, policy)
	defer manager.Close()
	first, err := manager.Run(context.Background(), "job", "file:///programs/job.ts", Options{User: execution.SystemUser(), Arguments: []any{"one"}})
	if err != nil || first.State != "IDLE" {
		t.Fatalf("first=%#v err=%v", first, err)
	}
	live, err := manager.Inspect(first.ExecutionID)
	if err != nil || live.Result != nil || live.ModuleDependencies != nil {
		t.Fatalf("live idle record=%#v err=%v", live, err)
	}
	second, err := manager.Run(context.Background(), "job", "file:///programs/job.ts", Options{User: execution.SystemUser(), Arguments: []any{"two"}})
	if err != nil || second.WorkerID != first.WorkerID || second.ExecutionID == first.ExecutionID || second.ContextID == first.ContextID || !identity.Is(second.ContextID, "ctx") {
		t.Fatalf("first=%#v second=%#v err=%v", first, second, err)
	}
	if _, err := manager.Inspect(first.ExecutionID); err == nil {
		t.Fatal("superseded idle execution remained live")
	}
	workersFake.mu.Lock()
	defer workersFake.mu.Unlock()
	if len(workersFake.invocations) != 2 || workersFake.invocations[0].ContextID != first.ContextID || workersFake.invocations[1].ContextID != second.ContextID || workersFake.invocations[1].JobRunID != second.ExecutionID || workersFake.starts[0].Invocation == nil || workersFake.starts[0].Invocation.ContextID != first.ContextID {
		t.Fatalf("reused Worker lost invocation identity: %#v", workersFake.invocations)
	}
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
	first, err := manager.Run(context.Background(), "first", "file:///programs/job.ts", Options{User: execution.SystemUser(), Detached: true})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first job did not start")
	}
	queued, err := manager.Run(context.Background(), "queued", "file:///programs/job.ts", Options{User: execution.SystemUser(), Detached: true})
	if err != nil || queued.State != "QUEUED" {
		t.Fatalf("queued=%#v err=%v", queued, err)
	}
	if _, err := manager.Run(context.Background(), "overflow", "file:///programs/job.ts", Options{User: execution.SystemUser(), Detached: true}); err == nil {
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
	manager.records["job-pppppppppp"] = Record{ExecutionID: "job-pppppppppp", JobID: "parent-job", State: "RUNNING", Parallelism: 1}
	alice, err := execution.UserForUsername("alice")
	if err != nil {
		t.Fatal(err)
	}
	ctx := execution.WithCaller(context.Background(), execution.Caller{ContextID: "ctx-aaaaaaaaaa", JobRunID: "job-pppppppppp", Workload: model.WorkloadJob, User: alice})
	record, err := manager.Run(ctx, "child-job", "file:///programs/child.ts", Options{Parallelism: 1})
	if err != nil || record.State != "SUCCEEDED" || record.User != alice || record.ParentContextID != "ctx-aaaaaaaaaa" {
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
	record, err := manager.Run(context.Background(), "slow", "file:///programs/slow.ts", Options{User: execution.SystemUser(), Timeout: 20 * time.Millisecond})
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
	record, err := manager.Run(context.Background(), "broken", "file:///programs/broken.ts", Options{User: execution.SystemUser()})
	if err == nil || record.State != "FAILED" {
		t.Fatalf("record=%#v err=%v", record, err)
	}
	coordinatorFake.mu.Lock()
	defer coordinatorFake.mu.Unlock()
	if len(coordinatorFake.releases) != 1 || coordinatorFake.releases[0] != "sbx-0123456789:"+record.WorkerID+":" {
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
	if _, err := manager.Run(context.Background(), "reusable", "file:///programs/reusable.ts", Options{User: execution.SystemUser()}); err != nil {
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
	if _, err := manager.Run(context.Background(), "logical-job", "file:///programs/job.ts", Options{User: execution.SystemUser(), OwnerID: "the8020/users", Namespace: "the8020", ReleaseID: "commit"}); err != nil {
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
	if len(coordinatorFake.releases) != 1 || coordinatorFake.releases[0] != "sbx-0123456789:"+coordinatorFake.requests[0].AllocationID+":" {
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
		NodeID:   "nod-0123456789",
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

func TestLogReferencePrecedesStartupAndSurvivesFailure(t *testing.T) {
	for _, phase := range []string{"sandbox", "worker", "invocation"} {
		t.Run(phase, func(t *testing.T) {
			failure := errors.New("failed " + phase)
			captured := false
			coordinatorFake := &fakeCoordinator{beforeEnsure: func() {
				if !captured {
					t.Fatal("runtime started before capturing its log position")
				}
			}}
			workersFake := &fakeWorkers{}
			switch phase {
			case "sandbox":
				coordinatorFake.failure = failure
			case "worker":
				workersFake.startFailure = failure
			case "invocation":
				workersFake.failure = failure
			}
			policy := testPolicy()
			policy.LogPosition = func() string { captured = true; return "before-startup" }
			manager, err := New(coordinatorFake, workersFake, policy)
			if err != nil {
				t.Fatal(err)
			}
			defer manager.Close()
			record, err := manager.Run(context.Background(), "broken", "file:///programs/broken.ts", Options{User: execution.SystemUser()})
			if !errors.Is(err, failure) || record.State != "FAILED" || record.NodeID != policy.NodeID || record.LogPosition != "before-startup" || record.SandboxID != "sbx-0123456789" || !identity.Is(record.WorkerID, "wrk") || !identity.Is(record.ContextID, "ctx") || !identity.Is(record.ExecutionID, "job") || record.FinishedAt.Before(record.StartedAt) {
				t.Fatalf("failure lost its log reference: %#v, %v", record, err)
			}
		})
	}
}

func TestReusedWorkerGetsANewLogPosition(t *testing.T) {
	policy := testPolicy()
	policy.Reuse = true
	position := "before-first"
	policy.LogPosition = func() string { return position }
	manager, err := New(&fakeCoordinator{}, &fakeWorkers{}, policy)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	first, err := manager.Run(context.Background(), "reuse", "file:///programs/reuse.ts", Options{User: execution.SystemUser()})
	if err != nil {
		t.Fatal(err)
	}
	position = "before-second"
	second, err := manager.Run(context.Background(), "reuse", "file:///programs/reuse.ts", Options{User: execution.SystemUser()})
	if err != nil {
		t.Fatal(err)
	}
	if first.WorkerID != second.WorkerID || first.ContextID == second.ContextID || first.ExecutionID == second.ExecutionID || first.LogPosition != "before-first" || second.LogPosition != "before-second" {
		t.Fatalf("reused Worker reference: first=%#v, second=%#v", first, second)
	}
}
