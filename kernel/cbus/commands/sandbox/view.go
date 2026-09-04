// Package sandbox provides shared presentation helpers for sandbox commands.
package sandbox

import (
	"sort"
	"strings"

	"the8020/kernel/cbus/core"
	"the8020/kernel/sandbox/manager"
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

// Reason returns the shortest stable explanation of why a sandbox exists.
func Reason(inspection manager.Inspection) string {
	if inspection.Spec.Lifecycle.Warm {
		return "warm pool"
	}
	if inspection.Spec.WorkloadType == model.WorkloadService && len(inspection.Spec.ServiceIDs) > 0 {
		values := append([]string(nil), inspection.Spec.ServiceIDs...)
		sort.Strings(values)
		return "service:" + values[0]
	}
	owners := map[string]bool{}
	for _, worker := range inspection.Workers {
		if worker.OwnerID != "" {
			owners[worker.OwnerID] = true
		}
	}
	if len(owners) > 0 {
		values := make([]string, 0, len(owners))
		for owner := range owners {
			values = append(values, owner)
		}
		sort.Strings(values)
		return strings.Join(values, ", ")
	}
	if inspection.Spec.GroupKey != "" {
		return inspection.Spec.GroupKey
	}
	return string(inspection.Spec.WorkloadType)
}

// Detail decorates one cached or explicitly refreshed inspection with the
// lightweight relationships used by both sandbox detail operations.
func Detail(runtimeServices *services.RuntimeServices, item manager.Inspection) (core.Result, error) {
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
	associated := []serviceAssociation{}
	if runtimeServices.Services != nil {
		for _, serviceID := range item.Spec.ServiceIDs {
			status, err := runtimeServices.Services.Inspect(serviceID)
			if err != nil {
				continue
			}
			associated = append(associated, serviceAssociation{ServiceID: status.ServiceID, State: status.State, Enabled: status.Enabled, SandboxCount: status.SandboxCount, WorkerCount: status.WorkerCount})
		}
	}
	sort.Slice(associated, func(i, j int) bool { return associated[i].ServiceID < associated[j].ServiceID })
	return core.Result{"sandbox": item, "reason": Reason(item), "services": associated}, nil
}
