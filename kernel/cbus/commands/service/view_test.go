package service

import (
	"context"
	"testing"
	"time"

	"the8020/kernel/sandbox/manager"
	"the8020/kernel/sandbox/model"
	"the8020/kernel/services"
	"the8020/kernel/webservices"
)

type observedResourceSandboxes struct {
	services.SandboxService
	inspection manager.Inspection
}

func (f observedResourceSandboxes) Inspect(context.Context, string) (manager.Inspection, error) {
	return f.inspection, nil
}

func TestObservedResourcesIncludeDrainingWorkersAndExcludeColocatedServices(t *testing.T) {
	now := time.Now().UTC()
	status := webservices.Status{Sandboxes: []webservices.ServiceSandboxStatus{{
		SandboxID: "sbx-0123456789", PoolID: "current", WorkerIDs: []string{"current", "draining"},
	}}}
	workers := []model.RuntimeWorkerStatus{
		{WorkerID: "current", WorkloadID: "current", Resources: &model.WorkerResources{HeapUsedBytes: 10, CPUUsageMicros: 20, SampledAt: now}},
		{WorkerID: "draining", WorkloadID: "old", Resources: &model.WorkerResources{HeapUsedBytes: 30, CPUUsageMicros: 40, SampledAt: now.Add(-time.Second)}},
		{WorkerID: "other", WorkloadID: "other", Resources: &model.WorkerResources{HeapUsedBytes: 1000, CPUUsageMicros: 1000, SampledAt: now}},
	}
	catalog := observedResourceSandboxes{inspection: manager.Inspection{Runtime: model.RuntimeSnapshot{Revision: 1, Workers: workers}}}
	result := Observed(context.Background(), status, catalog)
	if result.WorkerCount != 2 || result.Resources == nil || result.Resources.HeapUsedBytes != 40 || result.Resources.CPUUsageMicros != 60 || !result.Resources.SampledAt.Equal(now.Add(-time.Second)) {
		t.Fatalf("wrong service resources: %#v", result)
	}
	if status.Sandboxes[0].WorkerIDs[0] != "current" {
		t.Fatal("observation changed the routing record")
	}
	workers[1].Resources = nil
	if result := Observed(context.Background(), status, catalog); result.Resources != nil {
		t.Fatalf("incomplete measurements reported a partial total: %#v", result.Resources)
	}
}
