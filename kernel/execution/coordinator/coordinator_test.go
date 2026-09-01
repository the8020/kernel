package coordinator

import (
	"context"
	"errors"
	"testing"

	"the8020/kernel/sandbox/manager"
	"the8020/kernel/sandbox/model"
)

type fakeSandboxes struct {
	items     []manager.Inspection
	creates   []model.SandboxSpec
	ownerAdds int
}

func (f *fakeSandboxes) NewSandboxID() (string, error) { return model.NewSandboxID() }
func (f *fakeSandboxes) ReleaseSandboxID(string)       {}

func (f *fakeSandboxes) AddOwner(_ context.Context, groupID, ownerID string, serviceID ...string) (manager.Inspection, error) {
	f.ownerAdds++
	for index := range f.items {
		if f.items[index].Spec.RuntimeGroupID != groupID {
			continue
		}
		for _, existing := range f.items[index].Spec.OwnerIDs {
			if existing == ownerID {
				return f.items[index], nil
			}
		}
		f.items[index].Spec.OwnerIDs = append(f.items[index].Spec.OwnerIDs, ownerID)
		if len(serviceID) > 0 && serviceID[0] != "" {
			f.items[index].Spec.ServiceIDs = append(f.items[index].Spec.ServiceIDs, serviceID[0])
		}
		f.items[index].Status.CurrentOwners = append(f.items[index].Status.CurrentOwners, ownerID)
		return f.items[index], nil
	}
	return manager.Inspection{}, errors.New("runtime group not found")
}

func (f *fakeSandboxes) RemoveOwner(_ context.Context, groupID, ownerID, serviceID string) (bool, error) {
	for index := range f.items {
		if f.items[index].Spec.RuntimeGroupID != groupID {
			continue
		}
		f.items[index].Spec.OwnerIDs = removeTestValue(f.items[index].Spec.OwnerIDs, ownerID)
		f.items[index].Spec.ServiceIDs = removeTestValue(f.items[index].Spec.ServiceIDs, serviceID)
		return len(f.items[index].Spec.OwnerIDs) == 0, nil
	}
	return true, nil
}

func removeTestValue(values []string, candidate string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value != candidate {
			result = append(result, value)
		}
	}
	return result
}

type fakeWarmPool struct {
	inspection manager.Inspection
	assigned   bool
	profile    string
	groupKey   string
	owner      string
}

func (f *fakeWarmPool) Assign(_ context.Context, profile, groupKey, owner string) (manager.Inspection, bool, error) {
	f.profile, f.groupKey, f.owner = profile, groupKey, owner
	return f.inspection, f.assigned, nil
}

func (f *fakeSandboxes) List() ([]manager.Inspection, error) {
	return append([]manager.Inspection(nil), f.items...), nil
}
func (f *fakeSandboxes) Create(_ context.Context, spec model.SandboxSpec) (manager.Inspection, error) {
	f.creates = append(f.creates, spec)
	item := manager.Inspection{Spec: spec, Status: model.SandboxStatus{ObservedState: model.StateReady, SupervisorHealthy: true}}
	f.items = append(f.items, item)
	return item, nil
}

func TestEnsureReusesOnlyCompatibleHealthyGroups(t *testing.T) {
	backend := &fakeSandboxes{}
	coordinator, _ := New(backend)
	request := testRequest(t, "owner-one", model.WorkloadJob)
	first, err := coordinator.Ensure(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := coordinator.Ensure(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if first.Spec.RuntimeGroupID != second.Spec.RuntimeGroupID || len(backend.creates) != 1 || backend.ownerAdds != 1 {
		t.Fatalf("first=%#v second=%#v creates=%d", first, second, len(backend.creates))
	}
	request.OwnerID = "owner-two"
	third, err := coordinator.Ensure(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if third.Spec.RuntimeGroupID == first.Spec.RuntimeGroupID || len(backend.creates) != 2 {
		t.Fatalf("third=%#v creates=%d", third, len(backend.creates))
	}
	request.OwnerID = "owner-one"
	request.Profile.ResourceClass = "different"
	fourth, err := coordinator.Ensure(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if fourth.Spec.RuntimeGroupID == first.Spec.RuntimeGroupID {
		t.Fatal("incompatible profile reused")
	}
}

func TestEnsureSharedGroupPersistsEveryOwner(t *testing.T) {
	backend := &fakeSandboxes{}
	coordinator, _ := New(backend)
	firstRequest := testRequest(t, "owner-one", model.WorkloadJob)
	firstRequest.ExplicitGroupKey = "shared"
	first, err := coordinator.Ensure(context.Background(), firstRequest)
	if err != nil {
		t.Fatal(err)
	}
	secondRequest := testRequest(t, "owner-two", model.WorkloadJob)
	secondRequest.ExplicitGroupKey = "shared"
	second, err := coordinator.Ensure(context.Background(), secondRequest)
	if err != nil {
		t.Fatal(err)
	}
	if first.Spec.RuntimeGroupID != second.Spec.RuntimeGroupID || len(second.Spec.OwnerIDs) != 2 || second.Spec.OwnerIDs[1] != "owner-two" {
		t.Fatalf("first=%#v second=%#v", first, second)
	}
}

func TestEnsureSharedExplicitKeyStillSeparatesWorkloadTypes(t *testing.T) {
	backend := &fakeSandboxes{}
	coordinator, _ := New(backend)
	job := testRequest(t, "job", model.WorkloadJob)
	job.Strategy = model.GroupingShared
	job.ExplicitGroupKey = "common"
	first, err := coordinator.Ensure(context.Background(), job)
	if err != nil {
		t.Fatal(err)
	}
	service := testRequest(t, "service", model.WorkloadService)
	service.Strategy = model.GroupingShared
	service.ExplicitGroupKey = "common"
	second, err := coordinator.Ensure(context.Background(), service)
	if err != nil {
		t.Fatal(err)
	}
	if first.Spec.RuntimeGroupID == second.Spec.RuntimeGroupID || first.Spec.WorkloadType == second.Spec.WorkloadType {
		t.Fatalf("groups=%#v %#v", first, second)
	}
}

func TestEnsureServiceSandboxesUsePlacementGroupWithoutDuplicateAllocation(t *testing.T) {
	backend := &fakeSandboxes{}
	coordinator, _ := New(backend)
	placement := "interactive"
	firstRequest := testRequest(t, "pool-one", model.WorkloadService)
	firstRequest.PlacementGroup = &placement
	firstRequest.LogicalServiceID = "example/chat/session"
	first, err := coordinator.Ensure(context.Background(), firstRequest)
	if err != nil {
		t.Fatal(err)
	}
	otherRequest := testRequest(t, "pool-other", model.WorkloadService)
	otherRequest.PlacementGroup = &placement
	otherRequest.LogicalServiceID = "example/chat/shell"
	other, err := coordinator.Ensure(context.Background(), otherRequest)
	if err != nil {
		t.Fatal(err)
	}
	secondRequest := testRequest(t, "pool-two", model.WorkloadService)
	secondRequest.PlacementGroup = &placement
	secondRequest.LogicalServiceID = "example/chat/session"
	second, err := coordinator.Ensure(context.Background(), secondRequest)
	if err != nil {
		t.Fatal(err)
	}
	if first.Spec.RuntimeGroupID != other.Spec.RuntimeGroupID {
		t.Fatal("different services in the same placement group did not share")
	}
	if second.Spec.RuntimeGroupID == first.Spec.RuntimeGroupID {
		t.Fatal("two allocations of one service were placed in one sandbox")
	}
}

func TestEnsureAssignsCompatibleWarmGroupBeforeColdCreate(t *testing.T) {
	backend := &fakeSandboxes{}
	request := testRequest(t, "owner-one", model.WorkloadJob)
	warmInspection := manager.Inspection{Spec: model.SandboxSpec{RuntimeGroupID: "group-warm", SandboxID: "sandbox-warm", WorkloadType: model.WorkloadJob}}
	warm := &fakeWarmPool{inspection: warmInspection, assigned: true}
	coordinator, err := New(backend, warm)
	if err != nil {
		t.Fatal(err)
	}
	result, err := coordinator.Ensure(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	wantHash, _ := request.Profile.Hash()
	if result.Spec.RuntimeGroupID != "group-warm" || warm.profile != wantHash || warm.groupKey != "job:owner:owner-one" || warm.owner != "owner-one" || len(backend.creates) != 0 {
		t.Fatalf("result=%#v warm=%#v creates=%#v", result, warm, backend.creates)
	}
}

func testRequest(t *testing.T, owner string, workload model.WorkloadType) Request {
	t.Helper()
	digest := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	profile := model.RuntimeProfile{WorkloadType: workload, ImageDigest: digest, DependencyMode: model.DependencyCachedOnly, NetworkMode: "netstack", ResourceClass: "default"}
	if _, err := profile.Hash(); err != nil {
		t.Fatal(err)
	}
	return Request{WorkloadType: workload, OwnerID: owner, ExecutionID: "execution", Strategy: model.GroupingOwner, Profile: profile, ResourceLimits: model.ResourceLimits{MemoryHigh: 128, MemoryMaximum: 256, CPUQuotaMicros: 50, CPUPeriodMicros: 100, CPUWeight: 100, PIDMaximum: 32, TmpfsMaximum: 64}}
}
