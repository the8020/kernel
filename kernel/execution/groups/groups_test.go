package groups

import (
	"sync"
	"testing"

	"the8020/kernel/sandbox/model"
)

const digest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func profile(workload model.WorkloadType, dependency model.DependencyMode) model.RuntimeProfile {
	return model.RuntimeProfile{WorkloadType: workload, ImageDigest: digest, DependencyMode: dependency, NetworkMode: "netstack", ResourceClass: string(workload) + "-default"}
}

func TestSelectGroupingStrategiesAndOverrides(t *testing.T) {
	tests := []struct {
		name      string
		request   Request
		want      string
		wantError bool
	}{
		{"owner", Request{WorkloadType: model.WorkloadService, OwnerID: "s1", Strategy: model.GroupingOwner, Profile: profile(model.WorkloadService, model.DependencyCachedOnly)}, "service:owner:s1", false},
		{"isolated", Request{WorkloadType: model.WorkloadJob, OwnerID: "j1", ExecutionID: "e1", Strategy: model.GroupingIsolated, Profile: profile(model.WorkloadJob, model.DependencyCachedOnly)}, "job:isolated:e1", false},
		{"namespace", Request{WorkloadType: model.WorkloadService, OwnerID: "s1", Namespace: "package", Strategy: model.GroupingNamespace, Profile: profile(model.WorkloadService, model.DependencyCachedOnly)}, "service:namespace:package", false},
		{"shared", Request{WorkloadType: model.WorkloadService, OwnerID: "s1", Strategy: model.GroupingShared, Profile: profile(model.WorkloadService, model.DependencyCachedOnly)}, "service:shared", false},
		{"override", Request{WorkloadType: model.WorkloadJob, OwnerID: "j1", ExplicitGroupKey: "queue", Strategy: model.GroupingOwner, Profile: profile(model.WorkloadJob, model.DependencyCachedOnly)}, "job:explicit:queue", false},
		{"missing namespace", Request{WorkloadType: model.WorkloadService, OwnerID: "s1", Strategy: model.GroupingNamespace, Profile: profile(model.WorkloadService, model.DependencyCachedOnly)}, "", true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			selection, err := Select(test.request, nil)
			if test.wantError {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil || selection.GroupKey != test.want {
				t.Fatalf("Select() = %#v, %v; want key %q", selection, err, test.want)
			}
		})
	}
}

func TestSelectRequiresTypeKeyAndProfileCompatibility(t *testing.T) {
	request := Request{WorkloadType: model.WorkloadService, OwnerID: "service-a", Strategy: model.GroupingOwner, Profile: profile(model.WorkloadService, model.DependencyCachedOnly)}
	hash, _ := request.Profile.Hash()
	groups := []Group{
		{RuntimeGroupID: "wrong-type", WorkloadType: model.WorkloadJob, GroupKey: "service:owner:service-a", ProfileHash: hash, State: model.StateReady, Healthy: true},
		{RuntimeGroupID: "wrong-profile", WorkloadType: model.WorkloadService, GroupKey: "service:owner:service-a", ProfileHash: "sha256:different", State: model.StateReady, Healthy: true},
		{RuntimeGroupID: "unhealthy", WorkloadType: model.WorkloadService, GroupKey: "service:owner:service-a", ProfileHash: hash, State: model.StateReady, Healthy: false},
		{RuntimeGroupID: "compatible", WorkloadType: model.WorkloadService, GroupKey: "service:owner:service-a", ProfileHash: hash, State: model.StateActive, Healthy: true},
	}
	selection, err := Select(request, groups)
	if err != nil || !selection.Existing || selection.RuntimeGroupID != "compatible" {
		t.Fatalf("Select() = %#v, %v", selection, err)
	}
	request.Profile.DependencyMode = model.DependencyOnline
	selection, err = Select(request, groups)
	if err != nil || selection.Existing {
		t.Fatalf("incompatible online profile selected group: %#v, %v", selection, err)
	}
}

func TestServicePlacementGroupSharesAcrossServicesButNotDuplicateAllocations(t *testing.T) {
	placement := ""
	request := Request{WorkloadType: model.WorkloadService, OwnerID: "allocation-a", PlacementGroup: &placement, LogicalServiceID: "example/orders/api", Strategy: model.GroupingOwner, Profile: profile(model.WorkloadService, model.DependencyCachedOnly)}
	hash, _ := request.Profile.Hash()
	selection, err := Select(request, []Group{
		{RuntimeGroupID: "same-service", WorkloadType: model.WorkloadService, GroupKey: "service:placement:", ProfileHash: hash, ServiceIDs: []string{"example/orders/api"}, State: model.StateReady, Healthy: true},
		{RuntimeGroupID: "compatible", WorkloadType: model.WorkloadService, GroupKey: "service:placement:", ProfileHash: hash, ServiceIDs: []string{"example/catalog/api"}, State: model.StateReady, Healthy: true},
	})
	if err != nil || !selection.Existing || selection.RuntimeGroupID != "compatible" {
		t.Fatalf("Select() = %#v, %v", selection, err)
	}
}

func TestSelectSkipsSandboxesAtWorkerCPUOrRAMCapacity(t *testing.T) {
	placement := "shared"
	capacity := model.SandboxCapacityPolicy{MaximumWorkers: 64, TargetCPUUtilization: 0.8, TargetRAMUtilization: 0.8}
	request := Request{WorkloadType: model.WorkloadService, OwnerID: "allocation", PlacementGroup: &placement, LogicalServiceID: "example/orders/api", RequestedWorkers: 1, Strategy: model.GroupingOwner, Profile: profile(model.WorkloadService, model.DependencyCachedOnly), Capacity: capacity}
	hash, _ := request.Profile.Hash()
	base := Group{WorkloadType: model.WorkloadService, GroupKey: "service:placement:c2hhcmVk", ProfileHash: hash, State: model.StateReady, Healthy: true}
	workerFull, cpuFull, ramFull, eligible := base, base, base, base
	workerFull.RuntimeGroupID, workerFull.WorkerCount = "a-worker-full", 64
	cpuFull.RuntimeGroupID, cpuFull.CPUUtilization = "b-cpu-full", 0.8
	ramFull.RuntimeGroupID, ramFull.RAMUtilization = "c-ram-full", 0.9
	eligible.RuntimeGroupID, eligible.WorkerCount = "d-eligible", 63
	selection, err := Select(request, []Group{workerFull, cpuFull, ramFull, eligible})
	if err != nil || !selection.Existing || selection.RuntimeGroupID != "d-eligible" {
		t.Fatalf("selection=%#v err=%v", selection, err)
	}
	request.RequestedWorkers = 2
	selection, err = Select(request, []Group{eligible})
	if err != nil || selection.Existing {
		t.Fatalf("multi-Worker allocation overfilled sandbox: selection=%#v err=%v", selection, err)
	}
}

func TestWarmPoolAccountingAndNoReuseAfterAssignment(t *testing.T) {
	pool := NewWarmPool()
	profileHash, _ := profile(model.WorkloadJob, model.DependencyCachedOnly).Hash()
	if err := pool.Resize(profileHash, 2); err != nil {
		t.Fatal(err)
	}
	if err := pool.Add(WarmGroup{RuntimeGroupID: "warm-1", ProfileHash: profileHash, State: WarmCreating}); err != nil {
		t.Fatal(err)
	}
	if err := pool.SetState("warm-1", WarmReady); err != nil {
		t.Fatal(err)
	}
	status := pool.Status()[0]
	if status.Ready != 1 || status.Replenish != 1 {
		t.Fatalf("initial status = %#v", status)
	}
	reserved, ok := pool.Reserve(profileHash)
	if !ok || reserved.RuntimeGroupID != "warm-1" {
		t.Fatalf("Reserve() = %#v, %v", reserved, ok)
	}
	if err := pool.SetState("warm-1", WarmAssigned); err != nil {
		t.Fatal(err)
	}
	if _, ok := pool.Reserve(profileHash); ok {
		t.Fatal("assigned warm group was reused")
	}
	if err := pool.Destroy("warm-1"); err != nil {
		t.Fatal(err)
	}
	status = pool.Status()[0]
	if status.Replenish != 2 || status.Assigned != 0 {
		t.Fatalf("released status = %#v", status)
	}
}

func TestWarmPoolRestoresDurableAccounting(t *testing.T) {
	pool := NewWarmPool()
	hash := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	for _, group := range []WarmGroup{{RuntimeGroupID: "ready", ProfileHash: hash, State: WarmReady}, {RuntimeGroupID: "assigned", ProfileHash: hash, State: WarmAssigned}, {RuntimeGroupID: "failed", ProfileHash: hash, State: WarmFailed}} {
		if err := pool.Restore(group); err != nil {
			t.Fatal(err)
		}
	}
	status := pool.Status()[0]
	if status.Ready != 1 || status.Assigned != 1 || status.Failed != 1 {
		t.Fatalf("status=%#v", status)
	}
}

func TestWarmPoolReservationsAreAtomic(t *testing.T) {
	pool := NewWarmPool()
	profileHash, _ := profile(model.WorkloadJob, model.DependencyCachedOnly).Hash()
	_ = pool.Add(WarmGroup{RuntimeGroupID: "warm", ProfileHash: profileHash, State: WarmReady})
	var wait sync.WaitGroup
	winners := make(chan string, 2)
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if group, ok := pool.Reserve(profileHash); ok {
				winners <- group.RuntimeGroupID
			}
		}()
	}
	wait.Wait()
	close(winners)
	if len(winners) != 1 {
		t.Fatalf("reservation winners = %d, want 1", len(winners))
	}
}
