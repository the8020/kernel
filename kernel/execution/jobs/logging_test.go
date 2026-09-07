package jobs

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"the8020/kernel/execution"
	"the8020/kernel/sandbox/model"
)

type eventCapture struct {
	mu      sync.Mutex
	entries []slog.Record
	inspect func()
}

func (*eventCapture) Enabled(context.Context, slog.Level) bool { return true }
func (c *eventCapture) WithAttrs([]slog.Attr) slog.Handler     { return c }
func (c *eventCapture) WithGroup(string) slog.Handler          { return c }
func (c *eventCapture) Handle(_ context.Context, record slog.Record) error {
	// Re-entering the manager proves that publication released its state lock.
	if c.inspect != nil {
		c.inspect()
	}
	c.mu.Lock()
	c.entries = append(c.entries, record.Clone())
	c.mu.Unlock()
	return nil
}
func (c *eventCapture) snapshot() []slog.Record {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]slog.Record(nil), c.entries...)
}
func eventFields(record slog.Record) map[string]string {
	fields := map[string]string{}
	record.Attrs(func(attr slog.Attr) bool { fields[attr.Key] = attr.Value.String(); return true })
	return fields
}

func TestJobLifecycleLogsFollowEachReusedInvocation(t *testing.T) {
	capture := &eventCapture{}
	policy := testPolicy()
	policy.Logger, policy.Reuse = slog.New(capture), true
	m, err := New(&fakeCoordinator{}, &fakeWorkers{}, policy)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	capture.inspect = func() { _, _ = m.List() }
	ctx := execution.WithCaller(context.Background(), execution.Caller{ContextID: "ctx-abcdefghij", User: execution.SystemUser(), Workload: model.WorkloadService})
	first, err := m.Run(ctx, "example/job", "file:///programs/job.ts", Options{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := m.Run(ctx, "example/job", "file:///programs/job.ts", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if first.WorkerID != second.WorkerID || first.ContextID == second.ContextID || first.ExecutionID == second.ExecutionID {
		t.Fatal("reused invocation identity is wrong")
	}
	entries := capture.snapshot()
	if len(entries) != 6 {
		t.Fatalf("events: %d", len(entries))
	}
	for index, run := range []Record{first, second} {
		for offset, event := range []string{"job_admitted", "job_started", "job_completed"} {
			entry := entries[index*3+offset]
			fields := eventFields(entry)
			if fields["event"] != event || fields["execution_id"] != run.ExecutionID || fields["context_id"] != run.ContextID || fields["parent_context_id"] != "ctx-abcdefghij" || fields["username"] != "system" || fields["object"] != "module:example/job" {
				t.Fatalf("event attribution: %#v", fields)
			}
			if offset > 0 && (fields["sandbox_id"] != run.SandboxID || fields["worker_id"] != run.WorkerID) {
				t.Fatal("allocated runtime identity missing", fields)
			}
			if offset == 2 && !entry.Time.Equal(run.FinishedAt) {
				t.Fatal("completion observation time changed")
			}
		}
	}
}

func TestJobFailuresLogAllocatedReferencesAndScrubBeforePublication(t *testing.T) {
	for _, phase := range []string{"sandbox", "worker", "invocation"} {
		t.Run(phase, func(t *testing.T) {
			const secret = "private-input-do-not-log"
			cause := errors.New("failure before " + secret + " after")
			coordinator, workers := &fakeCoordinator{}, &fakeWorkers{}
			switch phase {
			case "sandbox":
				coordinator.failure = cause
			case "worker":
				workers.startFailure = cause
			case "invocation":
				workers.failure = cause
			}
			capture := &eventCapture{}
			policy := testPolicy()
			policy.Logger = slog.New(capture)
			m, err := New(coordinator, workers, policy)
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			capture.inspect = func() { _, _ = m.List() }
			run, err := m.Run(context.Background(), "example/job", "file:///programs/job.ts", Options{User: execution.SystemUser(), Secrets: map[string]string{"input": secret}})
			if err == nil || strings.Contains(err.Error(), secret) {
				t.Fatalf("returned failure: %v", err)
			}
			failures := 0
			for _, entry := range capture.snapshot() {
				if strings.Contains(entry.Message, secret) {
					t.Fatal("secret persisted")
				}
				fields := eventFields(entry)
				if fields["event"] != "job_failed" {
					continue
				}
				failures++
				if entry.Level != slog.LevelError || entry.Message != run.Failure || !strings.Contains(entry.Message, "[secure input]") || fields["context_id"] != run.ContextID || fields["execution_id"] != run.ExecutionID || fields["sandbox_id"] != run.SandboxID || !entry.Time.Equal(run.FinishedAt) {
					t.Fatalf("failure observation lost identity or diagnostics: %#v", fields)
				}
			}
			if failures != 1 {
				t.Fatalf("failure events: %d", failures)
			}
		})
	}
}

func TestQueuedCancellationAndSandboxFailurePublishOutsideStateLock(t *testing.T) {
	gate := make(chan struct{})
	workers := &fakeWorkers{gate: gate, runStarted: make(chan struct{}, 1)}
	capture := &eventCapture{}
	policy := testPolicy()
	policy.MaximumParallel, policy.QueuedExecutionLimit, policy.Logger = 1, 2, slog.New(capture)
	m, err := New(&fakeCoordinator{}, workers, policy)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	capture.inspect = func() { _, _ = m.List() }
	options := Options{User: execution.SystemUser(), Detached: true}
	first, err := m.Run(context.Background(), "example/job", "file:///programs/job.ts", options)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-workers.runStarted:
	case <-time.After(time.Second):
		t.Fatal("job did not start")
	}
	queued, err := m.Run(context.Background(), "example/job", "file:///programs/job.ts", options)
	if err != nil || queued.State != "QUEUED" {
		t.Fatalf("queued: %#v %v", queued, err)
	}
	if err := m.Cancel(context.Background(), queued.ExecutionID); err != nil {
		t.Fatal(err)
	}
	live, err := m.Inspect(first.ExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.FailSandbox(live.SandboxID, "sandbox exited with code 137"); err != nil {
		t.Fatal(err)
	}
	close(gate)
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, entry := range capture.snapshot() {
		fields := eventFields(entry)
		event := fields["event"]
		seen[event]++
		if event == "job_cancelled" && fields["context_id"] != queued.ContextID {
			t.Fatal("wrong cancelled context")
		}
		if event == "job_failed" && (fields["context_id"] != first.ContextID || entry.Message != "sandbox exited with code 137") {
			t.Fatal("wrong sandbox failure")
		}
	}
	if seen["job_cancelled"] != 1 || seen["job_failed"] != 1 || seen["job_started"] != 1 || seen["job_completed"] != 0 {
		t.Fatal("terminal events", seen)
	}
}
