package jobs

import (
	"context"
	"testing"
	"time"

	"the8020/kernel/execution"
	"the8020/kernel/sandbox/model"
	"the8020/kernel/settings"
)

func TestPreparedDefaultsValidateDiscardAndCommit(t *testing.T) {
	manager, err := New(&fakeCoordinator{}, &fakeWorkers{}, testPolicy())
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	originalTimeout := manager.policy.ExecutionTimeout
	values := settings.Values{
		"job.default.maximum_parallel_workers": int64(2), "job.default.queued_execution_limit": int64(3),
		"job.default.execution_timeout": int64(42000), "job.default.idle_runtime_timeout": int64(1000),
		"job.default.worker_reuse": true,
	}
	for _, invalid := range []any{"bad", int64(0), int64(-1), int64(1<<63 - 1)} {
		values["job.default.execution_timeout"] = invalid
		if _, err := manager.Prepare(context.Background(), values); err == nil {
			t.Fatalf("invalid timeout accepted: %v", invalid)
		}
	}
	values["job.default.execution_timeout"] = int64(42000)
	change, err := manager.Prepare(context.Background(), values)
	if err != nil {
		t.Fatal(err)
	}
	change.Discard()
	if manager.policy.ExecutionTimeout != originalTimeout {
		t.Fatal("discard applied defaults")
	}
	change, err = manager.Prepare(context.Background(), values)
	if err != nil {
		t.Fatal(err)
	}
	change.Commit()
	record, err := manager.Run(context.Background(), "live", "file:///programs/job.ts", Options{User: execution.SystemUser()})
	if err != nil || record.State != "IDLE" || record.Parallelism != 2 || record.Timeout != 42*time.Second || manager.policy.QueuedExecutionLimit != 3 || manager.policy.IdleRuntimeTimeout != time.Second {
		t.Fatalf("live defaults: %#v, %v", record, err)
	}
}

func TestAllocationPolicyCapturesFutureJobSubmissions(t *testing.T) {
	original := testPolicy()
	original.Resources.PIDMaximum, original.Resources.TmpfsMaximum = 10, 100
	coordinator := &fakeCoordinator{}
	m, err := New(coordinator, &fakeWorkers{}, original)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	nextProfile := original.Profile
	nextProfile.ResourceClass = "new-resources"
	nextResources := original.Resources
	nextResources.PIDMaximum = 20
	coordinator.beforeEnsure = func() { m.SetAllocationPolicy(model.GroupingShared, nextProfile, nextResources, original.Lifecycle) }
	if _, err := m.Run(t.Context(), "before", "file:///programs/job.ts", Options{User: execution.SystemUser()}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Run(t.Context(), "after", "file:///programs/job.ts", Options{User: execution.SystemUser()}); err != nil {
		t.Fatal(err)
	}
	if coordinator.requests[0].Strategy != original.Strategy || coordinator.requests[0].ResourceLimits.PIDMaximum != 10 || coordinator.requests[1].Strategy != model.GroupingShared || coordinator.requests[1].ResourceLimits.PIDMaximum != 20 {
		t.Fatalf("allocation snapshots: %+v", coordinator.requests)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			m.SetAllocationPolicy(model.GroupingShared, nextProfile, nextResources, original.Lifecycle)
		}
	}()
	for i := 0; i < 100; i++ {
		if _, err := m.prepare("concurrent", "file:///programs/job.ts", Options{User: execution.SystemUser()}); err != nil {
			t.Fatal(err)
		}
	}
	<-done
}
