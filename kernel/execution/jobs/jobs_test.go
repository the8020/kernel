package jobs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"the8020/kernel/execution/coordinator"
	"the8020/kernel/execution/records"
	"the8020/kernel/execution/supervisor"
	"the8020/kernel/execution/workers"
	"the8020/kernel/sandbox/manager"
	"the8020/kernel/sandbox/model"
)

type fakeCoordinator struct{ requests []coordinator.Request }

func (f *fakeCoordinator) Ensure(_ context.Context, request coordinator.Request) (manager.Inspection, error) {
	f.requests = append(f.requests, request)
	return manager.Inspection{Spec: model.SandboxSpec{SandboxID: "sandbox", RuntimeGroupID: "group", WorkloadType: model.WorkloadJob, Permissions: model.Permissions{ReadPaths: []string{"/programs"}}}}, nil
}

type fakeWorkers struct {
	starts  []supervisor.StartWorkerRequest
	runs    []string
	stops   []string
	failure error
}

type queueWorkers struct {
	mu     sync.Mutex
	starts []string
	stops  []string
}

type blockingWorkers struct{ fakeWorkers }

func (f *blockingWorkers) Start(ctx context.Context, _ string, _ supervisor.StartWorkerRequest) (workers.Record, error) {
	<-ctx.Done()
	return workers.Record{}, ctx.Err()
}

func (f *queueWorkers) Start(_ context.Context, group string, request supervisor.StartWorkerRequest) (workers.Record, error) {
	f.mu.Lock()
	f.starts = append(f.starts, request.Metadata.ExecutionID)
	f.mu.Unlock()
	return workers.Record{RuntimeGroupID: group, Worker: supervisor.WorkerStatus{WorkerID: request.Metadata.WorkerID}}, nil
}

func (f *queueWorkers) RunJob(_ context.Context, _ string, input any, _ []string) (supervisor.JobResult, error) {
	return supervisor.JobResult{Result: input}, nil
}

func (f *queueWorkers) Stop(_ context.Context, workerID string, _ bool) error {
	f.mu.Lock()
	f.stops = append(f.stops, workerID)
	f.mu.Unlock()
	return nil
}

func (f *queueWorkers) started() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.starts...)
}

func (f *fakeWorkers) Start(_ context.Context, group string, request supervisor.StartWorkerRequest) (workers.Record, error) {
	f.starts = append(f.starts, request)
	return workers.Record{RuntimeGroupID: group, Worker: supervisor.WorkerStatus{WorkerID: request.Metadata.WorkerID}}, nil
}
func (f *fakeWorkers) RunJob(_ context.Context, workerID string, input any, _ []string) (supervisor.JobResult, error) {
	f.runs = append(f.runs, workerID)
	if f.failure != nil {
		return supervisor.JobResult{}, f.failure
	}
	return supervisor.JobResult{Result: input, Logs: []supervisor.LogEvent{{Level: "info", Message: "job log"}}}, nil
}
func (f *fakeWorkers) Stop(_ context.Context, workerID string, _ bool) error {
	f.stops = append(f.stops, workerID)
	return nil
}

func TestJobListQuarantinesOnlyInvalidRecord(t *testing.T) {
	root := t.TempDir()
	store, err := records.New(root)
	if err != nil {
		t.Fatal(err)
	}
	valid := Record{ExecutionID: "execution-valid", State: "SUCCEEDED"}
	if err := store.Save(valid.ExecutionID, valid); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "execution-broken.json"), []byte(`{"unknown":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := New(&fakeCoordinator{}, &fakeWorkers{}, store, testPolicy())
	if err != nil {
		t.Fatal(err)
	}
	items, err := manager.List()
	if err != nil || len(items) != 1 || items[0].ExecutionID != valid.ExecutionID {
		t.Fatalf("items=%#v err=%v", items, err)
	}
	quarantined, err := filepath.Glob(filepath.Join(root, "quarantine", "execution-broken-*.json"))
	if err != nil || len(quarantined) != 1 {
		t.Fatalf("quarantined=%#v err=%v", quarantined, err)
	}
}

func TestJobSchedulingCompletionAndReusePreservesHistory(t *testing.T) {
	store, _ := records.New(t.TempDir())
	coordinatorFake, workersFake := &fakeCoordinator{}, &fakeWorkers{}
	policy := testPolicy()
	policy.MaximumParallel, policy.Reuse = 2, true
	manager, err := New(coordinatorFake, workersFake, store, policy)
	if err != nil {
		t.Fatal(err)
	}
	first, err := manager.Run(context.Background(), "job-1", "file:///programs/job.ts", Options{Input: "one"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.Run(context.Background(), "job-1", "file:///programs/job.ts", Options{Input: "two"})
	if err != nil {
		t.Fatal(err)
	}
	if first.State != "IDLE" || second.State != "IDLE" || first.ExecutionID == second.ExecutionID || first.WorkerID != second.WorkerID || len(first.Logs) != 1 || len(workersFake.starts) != 1 || len(workersFake.runs) != 2 {
		t.Fatalf("first=%#v second=%#v starts=%d runs=%d", first, second, len(workersFake.starts), len(workersFake.runs))
	}
	items, err := manager.List()
	if err != nil || len(items) != 2 {
		t.Fatalf("history=%#v err=%v", items, err)
	}
	storedFirst, err := manager.Inspect(first.ExecutionID)
	if err != nil || storedFirst.Result != "one" || storedFirst.State != "SUCCEEDED" {
		t.Fatalf("first history=%#v err=%v", storedFirst, err)
	}
}

func TestJobReuseRequiresCompatibleRelease(t *testing.T) {
	store, _ := records.New(t.TempDir())
	workersFake := &fakeWorkers{}
	policy := testPolicy()
	policy.Reuse = true
	manager, _ := New(&fakeCoordinator{}, workersFake, store, policy)
	if _, err := manager.Run(context.Background(), "job", "file:///programs/job.ts", Options{ReleaseID: "release-one"}); err != nil {
		t.Fatal(err)
	}
	second, err := manager.Run(context.Background(), "job", "file:///programs/job.ts", Options{ReleaseID: "release-two"})
	if err != nil {
		t.Fatal(err)
	}
	if len(workersFake.starts) != 2 || second.ReleaseID != "release-two" {
		t.Fatalf("starts=%d second=%#v", len(workersFake.starts), second)
	}
}

func TestJobNoReuseStopsWorkerFailureAndCancelPersist(t *testing.T) {
	store, _ := records.New(t.TempDir())
	workersFake := &fakeWorkers{}
	policy := testPolicy()
	policy.MaximumParallel = 1
	manager, _ := New(&fakeCoordinator{}, workersFake, store, policy)
	completed, err := manager.Run(context.Background(), "job", "file:///programs/job.ts", Options{Input: 1})
	if err != nil || completed.State != "SUCCEEDED" || len(workersFake.stops) != 1 {
		t.Fatalf("completed=%#v stops=%#v err=%v", completed, workersFake.stops, err)
	}
	workersFake.failure = errors.New("program crashed")
	failed, err := manager.Run(context.Background(), "bad", "file:///programs/bad.ts", Options{})
	if err == nil || failed.State != "FAILED" || failed.Failure != "program crashed" {
		t.Fatalf("failed=%#v err=%v", failed, err)
	}
	workersFake.failure = nil
	active := Record{ExecutionID: "execution-active", JobID: "job", WorkerID: "worker-active", State: "RUNNING"}
	if err := store.Save(active.ExecutionID, active); err != nil {
		t.Fatal(err)
	}
	if err := manager.Cancel(context.Background(), active.ExecutionID); err != nil {
		t.Fatal(err)
	}
	cancelled, _ := manager.Inspect(active.ExecutionID)
	if cancelled.State != "CANCELLED" {
		t.Fatalf("cancelled=%#v", cancelled)
	}
}

func TestJobTimeoutIncludesWorkerStartup(t *testing.T) {
	store, _ := records.New(t.TempDir())
	manager, err := New(&fakeCoordinator{}, &blockingWorkers{}, store, testPolicy())
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	record, err := manager.Run(context.Background(), "slow", "file:///programs/slow.ts", Options{Timeout: 20 * time.Millisecond})
	if !errors.Is(err, context.DeadlineExceeded) || record.State != "FAILED" || time.Since(started) > time.Second {
		t.Fatalf("startup timeout record=%#v elapsed=%s err=%v", record, time.Since(started), err)
	}
}

func TestJobQueueIsBoundedAndQueuedCancellationDoesNotStopAWorker(t *testing.T) {
	store, _ := records.New(t.TempDir())
	_ = store.Save("execution-running", Record{ExecutionID: "execution-running", State: "RUNNING"})
	policy := testPolicy()
	policy.MaximumParallel = 1
	policy.QueuedExecutionLimit = 1
	workersFake := &queueWorkers{}
	manager, _ := New(&fakeCoordinator{}, workersFake, store, policy)
	defer manager.Close()
	queued, err := manager.Run(context.Background(), "job-one", "file:///programs/job.ts", Options{Detached: true})
	if err != nil || queued.State != "QUEUED" {
		t.Fatalf("queued=%#v err=%v", queued, err)
	}
	if _, err := manager.Run(context.Background(), "job-two", "file:///programs/job.ts", Options{Detached: true}); err == nil || !strings.Contains(err.Error(), "queue limit 1 reached") {
		t.Fatalf("queue limit error = %v", err)
	}
	if err := manager.Cancel(context.Background(), queued.ExecutionID); err != nil {
		t.Fatal(err)
	}
	cancelled, err := manager.Inspect(queued.ExecutionID)
	if err != nil || cancelled.State != "CANCELLED" || len(workersFake.started()) != 0 {
		t.Fatalf("cancelled=%#v starts=%#v err=%v", cancelled, workersFake.started(), err)
	}
}

func TestJobQueueAdmitsDetachedExecutionsInSubmissionOrder(t *testing.T) {
	store, _ := records.New(t.TempDir())
	running := Record{ExecutionID: "execution-running", State: "RUNNING"}
	_ = store.Save(running.ExecutionID, running)
	policy := testPolicy()
	policy.MaximumParallel = 1
	policy.QueuedExecutionLimit = 2
	workersFake := &queueWorkers{}
	manager, _ := New(&fakeCoordinator{}, workersFake, store, policy)
	defer manager.Close()
	fixed := time.Date(2026, time.August, 20, 12, 0, 0, 0, time.UTC)
	manager.now = func() time.Time { return fixed }
	first, err := manager.Run(context.Background(), "job-one", "file:///programs/one.ts", Options{Detached: true, Input: "one"})
	if err != nil || first.State != "QUEUED" {
		t.Fatalf("first=%#v err=%v", first, err)
	}
	second, err := manager.Run(context.Background(), "job-two", "file:///programs/two.ts", Options{Detached: true, Input: "two"})
	if err != nil || second.State != "QUEUED" {
		t.Fatalf("second=%#v err=%v", second, err)
	}
	if !first.QueuedAt.Before(second.QueuedAt) {
		t.Fatalf("submission order was not made durable: first=%s second=%s", first.QueuedAt, second.QueuedAt)
	}
	running.State = "SUCCEEDED"
	if err := store.Save(running.ExecutionID, running); err != nil {
		t.Fatal(err)
	}
	waitForState(t, manager, first.ExecutionID, "SUCCEEDED")
	waitForState(t, manager, second.ExecutionID, "SUCCEEDED")
	started := workersFake.started()
	if len(started) != 2 || started[0] != first.ExecutionID || started[1] != second.ExecutionID {
		t.Fatalf("Worker start order = %#v, want [%s %s]", started, first.ExecutionID, second.ExecutionID)
	}
}

func TestSynchronousQueuedJobHonorsCallerCancellation(t *testing.T) {
	store, _ := records.New(t.TempDir())
	_ = store.Save("execution-running", Record{ExecutionID: "execution-running", State: "RUNNING"})
	policy := testPolicy()
	policy.MaximumParallel = 1
	workersFake := &queueWorkers{}
	manager, _ := New(&fakeCoordinator{}, workersFake, store, policy)
	defer manager.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	record, err := manager.Run(ctx, "job", "file:///programs/job.ts", Options{})
	if !errors.Is(err, context.Canceled) || record.State != "CANCELLED" || len(workersFake.started()) != 0 {
		t.Fatalf("record=%#v starts=%#v err=%v", record, workersFake.started(), err)
	}
	stored, inspectErr := manager.Inspect(record.ExecutionID)
	if inspectErr != nil || stored.State != "CANCELLED" {
		t.Fatalf("stored=%#v err=%v", stored, inspectErr)
	}
}

func TestJobUsesExplicitExecutionOwnerForGroupingAndWorkerIdentity(t *testing.T) {
	store, _ := records.New(t.TempDir())
	coordinatorFake, workersFake := &fakeCoordinator{}, &fakeWorkers{}
	manager, _ := New(coordinatorFake, workersFake, store, testPolicy())
	if _, err := manager.Run(context.Background(), "logical-job", "file:///programs/job.ts", Options{OwnerID: "admin-user"}); err != nil {
		t.Fatal(err)
	}
	if coordinatorFake.requests[0].OwnerID != "admin-user" || workersFake.starts[0].Metadata.OwnerID != "admin-user" || workersFake.starts[0].Metadata.WorkloadID != "logical-job" {
		t.Fatalf("request=%#v metadata=%#v", coordinatorFake.requests[0], workersFake.starts[0].Metadata)
	}
}

func TestRuntimeGroupFailureFailsActiveJobsAndRetiresIdleReuse(t *testing.T) {
	store, _ := records.New(t.TempDir())
	active := Record{ExecutionID: "active", RuntimeGroupID: "group", State: "RUNNING", StartedAt: time.Now().Add(-time.Second)}
	idle := Record{ExecutionID: "idle", RuntimeGroupID: "group", State: "IDLE", Reuse: true}
	_ = store.Save(active.ExecutionID, active)
	_ = store.Save(idle.ExecutionID, idle)
	manager, _ := New(&fakeCoordinator{}, &fakeWorkers{}, store, testPolicy())
	if err := manager.FailGroup("group", "supervisor timeout"); err != nil {
		t.Fatal(err)
	}
	failed, _ := manager.Inspect(active.ExecutionID)
	retired, _ := manager.Inspect(idle.ExecutionID)
	if failed.State != "FAILED" || failed.Failure != "supervisor timeout" || failed.FinishedAt.IsZero() || retired.State != "SUCCEEDED" || retired.Reuse {
		t.Fatalf("failed=%#v retired=%#v", failed, retired)
	}
}

func TestReusableWorkerRetiresAfterIdleTimeout(t *testing.T) {
	store, _ := records.New(t.TempDir())
	workersFake := &fakeWorkers{}
	policy := testPolicy()
	policy.Reuse = true
	policy.IdleRuntimeTimeout = 5 * time.Millisecond
	manager, _ := New(&fakeCoordinator{}, workersFake, store, policy)
	defer manager.Close()
	record, err := manager.Run(context.Background(), "job", "file:///programs/job.ts", Options{})
	if err != nil || record.State != "IDLE" {
		t.Fatalf("record=%#v err=%v", record, err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		current, inspectErr := manager.Inspect(record.ExecutionID)
		if inspectErr == nil && current.State == "SUCCEEDED" {
			break
		}
		time.Sleep(time.Millisecond)
	}
	retired, _ := manager.Inspect(record.ExecutionID)
	if retired.State != "SUCCEEDED" || retired.Reuse || len(workersFake.stops) != 1 {
		t.Fatalf("retired=%#v stops=%#v", retired, workersFake.stops)
	}
}

func TestRestoreFailsActiveWithoutReplayAndRestoresIdleRetirement(t *testing.T) {
	store, _ := records.New(t.TempDir())
	now := time.Now().UTC()
	active := Record{ExecutionID: "execution-active", WorkerID: "worker-active", State: "RUNNING", StartedAt: now.Add(-time.Second)}
	queued := Record{ExecutionID: "execution-queued", WorkerID: "worker-never-started", State: "QUEUED", QueuedAt: now.Add(-2 * time.Second)}
	idle := Record{ExecutionID: "execution-idle", WorkerID: "worker-idle", State: "IDLE", Reuse: true, FinishedAt: now}
	_ = store.Save(active.ExecutionID, active)
	_ = store.Save(queued.ExecutionID, queued)
	_ = store.Save(idle.ExecutionID, idle)
	workersFake := &fakeWorkers{}
	policy := testPolicy()
	policy.IdleRuntimeTimeout = time.Hour
	manager, _ := New(&fakeCoordinator{}, workersFake, store, policy)
	defer manager.Close()
	manager.now = func() time.Time { return now }
	if err := manager.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	failed, _ := manager.Inspect(active.ExecutionID)
	failedQueued, _ := manager.Inspect(queued.ExecutionID)
	if failed.State != "FAILED" || !strings.Contains(failed.Failure, "not replayed") || failedQueued.State != "FAILED" || !strings.Contains(failedQueued.Failure, "not replayed") || manager.timers[idle.ExecutionID] == nil || len(workersFake.stops) != 1 {
		t.Fatalf("active=%#v queued=%#v timers=%#v stops=%#v", failed, failedQueued, manager.timers, workersFake.stops)
	}
}

func waitForState(t *testing.T, manager *Manager, executionID, state string) Record {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		record, err := manager.Inspect(executionID)
		if err == nil && record.State == state {
			return record
		}
		time.Sleep(time.Millisecond)
	}
	record, err := manager.Inspect(executionID)
	t.Fatalf("execution %s did not enter %s: record=%#v err=%v", executionID, state, record, err)
	return Record{}
}

func testPolicy() Policy {
	return Policy{
		Strategy: model.GroupingOwner,
		Profile: model.RuntimeProfile{
			WorkloadType: model.WorkloadJob, ImageDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			DependencyMode: model.DependencyCachedOnly, Permissions: model.Permissions{ReadPaths: []string{"/programs"}}, NetworkMode: "netstack", ResourceClass: "job",
		},
	}
}
