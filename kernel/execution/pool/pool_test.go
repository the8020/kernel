package pool

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"the8020/kernel/sandbox/manager"
	"the8020/kernel/sandbox/model"
)

type fakeSandboxes struct {
	mu       sync.Mutex
	items    []manager.Inspection
	created  []model.SandboxSpec
	assigned []string
	deleted  []string
}

func (f *fakeSandboxes) NewSandboxID() (string, error) { return model.NewSandboxID() }
func (f *fakeSandboxes) ReleaseSandboxID(string)       {}

func (f *fakeSandboxes) List() ([]manager.Inspection, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]manager.Inspection(nil), f.items...), nil
}

func (f *fakeSandboxes) Create(_ context.Context, spec model.SandboxSpec) (manager.Inspection, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created = append(f.created, spec)
	item := manager.Inspection{Spec: spec, Status: model.SandboxStatus{ObservedState: model.StateReady, SupervisorHealthy: true}}
	f.items = append(f.items, item)
	return item, nil
}

func TestControllerRestoresReadyAssignedAndFailedGroupsBeforeReplenishing(t *testing.T) {
	profile, resources := testTemplate(t)
	hash, _ := profile.Hash()
	ready := model.SandboxSpec{RuntimeGroupID: "group-ready", SandboxID: "sandbox-ready", ProfileHash: hash, Lifecycle: model.LifecyclePolicy{Warm: true}}
	assigned := model.SandboxSpec{RuntimeGroupID: "group-assigned", SandboxID: "sandbox-assigned", ProfileHash: hash, Labels: map[string]string{"the8020.assigned_at": "now"}}
	failed := model.SandboxSpec{RuntimeGroupID: "group-failed", SandboxID: "sandbox-failed", ProfileHash: hash, Lifecycle: model.LifecyclePolicy{Warm: true}}
	sandboxes := &fakeSandboxes{items: []manager.Inspection{
		{Spec: ready, Status: model.SandboxStatus{ObservedState: model.StateReady, SupervisorHealthy: true}},
		{Spec: assigned, Status: model.SandboxStatus{ObservedState: model.StateActive, SupervisorHealthy: true, WorkerCount: 1}},
		{Spec: failed, Status: model.SandboxStatus{ObservedState: model.StateFailed}},
	}}
	controller, err := New(sandboxes, []Template{{Profile: profile, Resources: resources}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer controller.Close()
	if err := controller.Start(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	status := statusFor(controller.Status(), hash)
	if status.Ready != 1 || status.Assigned != 1 || status.Failed != 1 || len(sandboxes.created) != 0 {
		t.Fatalf("status=%#v newly-created=%#v", status, sandboxes.created)
	}
}
func (f *fakeSandboxes) AssignWarm(_ context.Context, id, groupKey, owner string) (manager.Inspection, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, spec := range f.created {
		if spec.RuntimeGroupID == id {
			spec.GroupKey, spec.OwnerIDs, spec.Lifecycle.Warm = groupKey, []string{owner}, false
			f.assigned = append(f.assigned, id)
			return manager.Inspection{Spec: spec, Status: model.SandboxStatus{ObservedState: model.StateReady, SupervisorHealthy: true}}, nil
		}
	}
	return manager.Inspection{}, errors.New("missing warm group")
}
func (f *fakeSandboxes) Delete(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, id)
	return nil
}

func TestControllerProvisionsAssignsReplenishesAndTrims(t *testing.T) {
	profile, resources := testTemplate(t)
	sandboxes := &fakeSandboxes{}
	controller, err := New(sandboxes, []Template{{Profile: profile, Resources: resources}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer controller.Close()
	if err := controller.Start(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	hash, _ := profile.Hash()
	if status := statusFor(controller.Status(), hash); status.Ready != 1 || status.Desired != 1 {
		t.Fatalf("initial status=%#v", status)
	}
	assigned, ok, err := controller.Assign(context.Background(), hash, "job:owner:nightly", "nightly")
	if err != nil || !ok || assigned.Spec.OwnerIDs[0] != "nightly" {
		t.Fatalf("assigned=%#v ok=%v err=%v", assigned, ok, err)
	}
	deadline := time.Now().Add(time.Second)
	for statusFor(controller.Status(), hash).Ready != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	status := statusFor(controller.Status(), hash)
	if status.Ready != 1 || status.Assigned != 1 {
		t.Fatalf("replenished status=%#v", status)
	}
	if err := controller.Resize(hash, 0); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(time.Second)
	for statusFor(controller.Status(), hash).Ready != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if status := statusFor(controller.Status(), hash); status.Ready != 0 || len(sandboxes.deleted) != 1 {
		t.Fatalf("trimmed status=%#v deleted=%#v", status, sandboxes.deleted)
	}
	if err := controller.Resize("sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff", 1); err == nil {
		t.Fatal("unknown profile was accepted")
	}
}

func testTemplate(t *testing.T) (model.RuntimeProfile, model.ResourceLimits) {
	t.Helper()
	profile := model.RuntimeProfile{WorkloadType: model.WorkloadJob, ImageDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", DependencyMode: model.DependencyCachedOnly, NetworkMode: "netstack", ResourceClass: "job"}
	if _, err := profile.Hash(); err != nil {
		t.Fatal(err)
	}
	return profile, model.ResourceLimits{PIDMaximum: 32, TmpfsMaximum: 64}
}
