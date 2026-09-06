package placement

import (
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
	groups := []Candidate{
		{SandboxID: "wrong-type", WorkloadType: model.WorkloadJob, GroupKey: "service:owner:service-a", ProfileHash: hash, State: model.StateReady, Healthy: true},
		{SandboxID: "wrong-profile", WorkloadType: model.WorkloadService, GroupKey: "service:owner:service-a", ProfileHash: "sha256:different", State: model.StateReady, Healthy: true},
		{SandboxID: "unhealthy", WorkloadType: model.WorkloadService, GroupKey: "service:owner:service-a", ProfileHash: hash, State: model.StateReady, Healthy: false},
		{SandboxID: "compatible", WorkloadType: model.WorkloadService, GroupKey: "service:owner:service-a", ProfileHash: hash, State: model.StateActive, Healthy: true},
	}
	selection, err := Select(request, groups)
	if err != nil || !selection.Existing || selection.SandboxID != "compatible" {
		t.Fatalf("Select() = %#v, %v", selection, err)
	}
	request.Profile.DependencyMode = model.DependencyOnline
	selection, err = Select(request, groups)
	if err != nil || selection.Existing {
		t.Fatalf("incompatible online profile selected group: %#v, %v", selection, err)
	}
}

func TestJobPlacementGroupsMatchByValueAndStaySeparateFromServices(t *testing.T) {
	group := ""
	request := Request{WorkloadType: model.WorkloadJob, OwnerID: "program", Strategy: model.GroupingOwner, Profile: profile(model.WorkloadJob, model.DependencyCachedOnly), PlacementGroup: &group}
	selected, err := Select(request, nil)
	if err != nil || selected.GroupKey != "job:placement:" {
		t.Fatalf("empty placement=%#v %v", selected, err)
	}
	hash, _ := request.Profile.Hash()
	candidates := []Candidate{{SandboxID: "job-group", WorkloadType: model.WorkloadJob, GroupKey: selected.GroupKey, ProfileHash: hash, State: model.StateReady, Healthy: true}, {SandboxID: "service-group", WorkloadType: model.WorkloadService, GroupKey: selected.GroupKey, ProfileHash: hash, State: model.StateReady, Healthy: true}}
	selected, err = Select(request, candidates)
	if err != nil || selected.SandboxID != "job-group" {
		t.Fatalf("selected=%#v %v", selected, err)
	}
	group = "other"
	selected, err = Select(request, candidates)
	if err != nil || selected.Existing {
		t.Fatalf("cross-group selection=%#v %v", selected, err)
	}
}

func TestServicePlacementGroupSharesAcrossServicesButNotDuplicateAllocations(t *testing.T) {
	placement := ""
	request := Request{WorkloadType: model.WorkloadService, OwnerID: "allocation-a", PlacementGroup: &placement, LogicalServiceID: "example/orders/api", Strategy: model.GroupingOwner, Profile: profile(model.WorkloadService, model.DependencyCachedOnly)}
	hash, _ := request.Profile.Hash()
	selection, err := Select(request, []Candidate{
		{SandboxID: "same-service", WorkloadType: model.WorkloadService, GroupKey: "service:placement:", ProfileHash: hash, ServiceIDs: []string{"example/orders/api"}, State: model.StateReady, Healthy: true},
		{SandboxID: "compatible", WorkloadType: model.WorkloadService, GroupKey: "service:placement:", ProfileHash: hash, ServiceIDs: []string{"example/catalog/api"}, State: model.StateReady, Healthy: true},
	})
	if err != nil || !selection.Existing || selection.SandboxID != "compatible" {
		t.Fatalf("Select() = %#v, %v", selection, err)
	}
}

func TestSelectSkipsSandboxesAtWorkerCapacity(t *testing.T) {
	placement := "shared"
	request := Request{WorkloadType: model.WorkloadService, OwnerID: "allocation", PlacementGroup: &placement, LogicalServiceID: "example/orders/api", RequestedWorkers: 1, MaximumWorkers: 64, Strategy: model.GroupingOwner, Profile: profile(model.WorkloadService, model.DependencyCachedOnly)}
	hash, _ := request.Profile.Hash()
	base := Candidate{WorkloadType: model.WorkloadService, GroupKey: "service:placement:c2hhcmVk", ProfileHash: hash, State: model.StateReady, Healthy: true}
	workerFull, eligible := base, base
	workerFull.SandboxID, workerFull.WorkerCount = "a-worker-full", 64
	eligible.SandboxID, eligible.WorkerCount = "b-eligible", 63
	selection, err := Select(request, []Candidate{workerFull, eligible})
	if err != nil || !selection.Existing || selection.SandboxID != "b-eligible" {
		t.Fatalf("selection=%#v err=%v", selection, err)
	}
	request.RequestedWorkers = 2
	selection, err = Select(request, []Candidate{eligible})
	if err != nil || selection.Existing {
		t.Fatalf("multi-Worker allocation overfilled sandbox: selection=%#v err=%v", selection, err)
	}
}
