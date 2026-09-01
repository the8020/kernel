// Package inspect implements sandbox.inspect.
package inspect

import (
	"context"
	"sort"

	"the8020/kernel/cbus/commands/internal/commandutil"
	sandboxview "the8020/kernel/cbus/commands/sandbox"
	"the8020/kernel/cbus/core"
	"the8020/kernel/sandbox/model"
	"the8020/kernel/services"
	"the8020/kernel/webservices"
)

type serviceAssociation struct {
	ServiceID    string            `json:"service_id"`
	State        webservices.State `json:"state"`
	Enabled      bool              `json:"enabled"`
	SandboxCount int               `json:"sandbox_count"`
	WorkerCount  int               `json:"worker_count"`
}

func New(serviceSet *services.Services) core.Handler {
	return func(ctx context.Context, request core.Request) (core.Result, error) {
		runtimeServices, err := commandutil.Runtime(serviceSet)
		if err != nil {
			return nil, err
		}
		if runtimeServices.Sandboxes == nil {
			return nil, core.NewError(core.CodeRuntimeUnavailable, "sandbox manager is unavailable")
		}
		item, err := runtimeServices.Sandboxes.Inspect(ctx, commandutil.String(request, "sandbox_id"))
		if err != nil {
			return nil, commandutil.OperationError(err)
		}
		if metrics, metricsErr := runtimeServices.Sandboxes.Metrics(item.Spec.SandboxID); metricsErr == nil {
			item.Status.Metrics = metrics
		}
		if runtimeServices.Ports != nil {
			for _, lease := range runtimeServices.Ports.List() {
				if lease.SandboxID != item.Spec.SandboxID {
					continue
				}
				item.Status.ExposedPorts = append(item.Status.ExposedPorts, model.PortStatus{LeaseID: lease.LeaseID, OwnerID: lease.OwnerID, BindAddress: lease.BindAddress, HostPort: lease.HostPort, InternalPort: lease.InternalPort, Protocol: lease.Protocol, Purpose: lease.Purpose, State: lease.State, ExpiresAt: lease.ExpiresAt})
				if lease.Purpose == "debug" {
					item.Status.DebugLease = &model.DebugLeaseStatus{LeaseID: lease.LeaseID, Host: lease.BindAddress, Port: lease.HostPort, ExpiresAt: lease.ExpiresAt}
				}
			}
		}
		if item.Status.ExposedPorts == nil {
			item.Status.ExposedPorts = []model.PortStatus{}
		}
		associatedServices := []serviceAssociation{}
		if runtimeServices.Services != nil {
			statuses, listErr := runtimeServices.Services.List()
			if listErr != nil {
				return nil, commandutil.OperationError(listErr)
			}
			for _, status := range statuses {
				for _, sandbox := range status.Sandboxes {
					if sandbox.SandboxID == item.Spec.SandboxID {
						associatedServices = append(associatedServices, serviceAssociation{ServiceID: status.ServiceID, State: status.State, Enabled: status.Enabled, SandboxCount: status.SandboxCount, WorkerCount: status.WorkerCount})
						break
					}
				}
			}
		}
		sort.Slice(associatedServices, func(i, j int) bool { return associatedServices[i].ServiceID < associatedServices[j].ServiceID })
		return core.Result{"sandbox": item, "reason": sandboxview.Reason(item), "services": associatedServices}, nil
	}
}
