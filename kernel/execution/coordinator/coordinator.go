// Package coordinator ensures one compatible runtime group for a workload request.
package coordinator

import (
	"context"
	"errors"
	"fmt"
	"time"

	"the8020/kernel/execution/groups"
	"the8020/kernel/sandbox/manager"
	"the8020/kernel/sandbox/model"
)

type SandboxManager interface {
	NewSandboxID() (string, error)
	ReleaseSandboxID(string)
	List() ([]manager.Inspection, error)
	Create(context.Context, model.SandboxSpec) (manager.Inspection, error)
	AddOwner(context.Context, string, string, ...string) (manager.Inspection, error)
	RemoveOwner(context.Context, string, string, string) (bool, error)
}

// Release removes one workload owner and destroys the sandbox when it became
// empty. Worker shutdown remains the caller's responsibility.
func (c *Coordinator) Release(ctx context.Context, runtimeGroupID, ownerID, replicaServiceID string) error {
	_, err := c.sandboxes.RemoveOwner(ctx, runtimeGroupID, ownerID, replicaServiceID)
	return err
}

type WarmPool interface {
	Assign(context.Context, string, string, string) (manager.Inspection, bool, error)
}

type Coordinator struct {
	sandboxes SandboxManager
	warm      WarmPool
}

type Request struct {
	WorkloadType     model.WorkloadType
	OwnerID          string
	ExecutionID      string
	Namespace        string
	ExplicitGroupKey string
	PlacementGroup   *string
	ReplicaServiceID string
	Strategy         model.GroupingStrategy
	Profile          model.RuntimeProfile
	ResourceLimits   model.ResourceLimits
	Lifecycle        model.LifecyclePolicy
}

func New(sandboxes SandboxManager, warm ...WarmPool) (*Coordinator, error) {
	if sandboxes == nil {
		return nil, errors.New("sandbox manager is required")
	}
	coordinator := &Coordinator{sandboxes: sandboxes}
	if len(warm) > 0 {
		coordinator.warm = warm[0]
	}
	return coordinator, nil
}

func (c *Coordinator) Ensure(ctx context.Context, request Request) (manager.Inspection, error) {
	if request.ReplicaServiceID != "" && request.PlacementGroup == nil {
		return manager.Inspection{}, errors.New("service replica placement requires a sandbox group")
	}
	items, err := c.sandboxes.List()
	if err != nil {
		return manager.Inspection{}, err
	}
	existing := make([]groups.Group, 0, len(items))
	byID := map[string]manager.Inspection{}
	for _, item := range items {
		healthy := item.Status.SupervisorHealthy && (item.Status.ObservedState == model.StateReady || item.Status.ObservedState == model.StateActive)
		group := groups.Group{RuntimeGroupID: item.Spec.RuntimeGroupID, WorkloadType: item.Spec.WorkloadType, GroupKey: item.Spec.GroupKey, ProfileHash: item.Spec.ProfileHash, Owners: append([]string(nil), item.Spec.OwnerIDs...), ServiceIDs: append([]string(nil), item.Spec.ServiceIDs...), State: item.Status.ObservedState, Healthy: healthy}
		existing = append(existing, group)
		byID[item.Spec.RuntimeGroupID] = item
	}
	selection, err := groups.Select(groups.Request{WorkloadType: request.WorkloadType, OwnerID: request.OwnerID, ExecutionID: request.ExecutionID, Namespace: request.Namespace, ExplicitGroupKey: request.ExplicitGroupKey, PlacementGroup: request.PlacementGroup, ReplicaServiceID: request.ReplicaServiceID, Strategy: request.Strategy, Profile: request.Profile}, existing)
	if err != nil {
		return manager.Inspection{}, err
	}
	if selection.Existing {
		return c.sandboxes.AddOwner(ctx, selection.RuntimeGroupID, request.OwnerID, request.ReplicaServiceID)
	}
	if err := request.ResourceLimits.Validate(); err != nil {
		return manager.Inspection{}, err
	}
	if c.warm != nil && request.ReplicaServiceID == "" {
		inspection, assigned, assignErr := c.warm.Assign(ctx, selection.ProfileHash, selection.GroupKey, request.OwnerID)
		if assignErr != nil {
			return manager.Inspection{}, fmt.Errorf("assign warm runtime group: %w", assignErr)
		}
		if assigned {
			return inspection, nil
		}
	}
	runtimeGroupID, err := model.NewRuntimeGroupID()
	if err != nil {
		return manager.Inspection{}, err
	}
	sandboxID, err := c.sandboxes.NewSandboxID()
	if err != nil {
		return manager.Inspection{}, err
	}
	defer c.sandboxes.ReleaseSandboxID(sandboxID)
	token, err := model.NewID("token")
	if err != nil {
		return manager.Inspection{}, err
	}
	profileHash, err := request.Profile.Hash()
	if err != nil {
		return manager.Inspection{}, err
	}
	if request.Lifecycle.StopGracePeriod <= 0 {
		request.Lifecycle.StopGracePeriod = 10 * time.Second
	}
	egressHosts := request.Profile.Permissions.EgressHosts()
	serviceIDs := []string(nil)
	placementGroup := ""
	if request.ReplicaServiceID != "" {
		serviceIDs = []string{request.ReplicaServiceID}
		placementGroup = *request.PlacementGroup
	}
	spec := model.SandboxSpec{SandboxID: sandboxID, RuntimeGroupID: runtimeGroupID, WorkloadType: request.WorkloadType, GroupKey: selection.GroupKey, PlacementGroup: placementGroup, OwnerIDs: []string{request.OwnerID}, ServiceIDs: serviceIDs, ImageDigest: request.Profile.ImageDigest, RuntimeProfile: request.Profile, ProfileHash: profileHash, ResourceLimits: request.ResourceLimits, Network: model.NetworkConfiguration{Mode: "netstack", NetworkName: "the8020", EgressEnabled: request.Profile.EgressAllowed && len(egressHosts) > 0, AllowedHosts: egressHosts}, InternalPorts: []int{8000, 9229}, Mounts: append([]model.Mount(nil), request.Profile.Mounts...), Permissions: request.Profile.Permissions, DependencyMode: request.Profile.DependencyMode, Lifecycle: request.Lifecycle, Labels: map[string]string{"the8020.owner": request.OwnerID, "the8020.owners": request.OwnerID, "the8020.group_key": selection.GroupKey, "the8020.placement_group": placementGroup, "the8020.created_at": time.Now().UTC().Format(time.RFC3339Nano)}, InternalToken: token}
	inspection, err := c.sandboxes.Create(ctx, spec)
	if err != nil {
		return manager.Inspection{}, fmt.Errorf("create runtime group: %w", err)
	}
	return inspection, nil
}
