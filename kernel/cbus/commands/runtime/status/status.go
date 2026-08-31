// Package status implements runtime.status.
package status

import (
	"context"

	"the8020/kernel/cbus/core"
	"the8020/kernel/services"
)

func New(serviceSet *services.Services) core.Handler {
	return func(_ context.Context, _ core.Request) (core.Result, error) {
		runtimeServices := serviceSet.RuntimeSnapshot()
		if runtimeServices == nil {
			return core.Result{"ready": false, "failure": "runtime subsystem is unavailable"}, nil
		}
		result := core.Result{
			"ready": runtimeServices.Failure == "", "failure": runtimeServices.Failure,
			"configured_mode":  runtimeServices.Isolation.ConfiguredMode,
			"selected_mode":    runtimeServices.Isolation.SelectedMode,
			"selection_reason": runtimeServices.Isolation.SelectionReason,
			"full_ready":       runtimeServices.Isolation.FullReady,
			"rootless_ready":   runtimeServices.Isolation.RootlessReady,
			"capabilities":     runtimeServices.Isolation.Capabilities,
			"limitations":      runtimeServices.Isolation.Limitations,
		}
		if runtimeServices.Sandboxes != nil {
			sandboxes, err := runtimeServices.Sandboxes.List()
			if err != nil {
				return nil, err
			}
			workerCount := 0
			for _, sandbox := range sandboxes {
				workerCount += sandbox.Status.WorkerCount
			}
			result["sandbox_count"] = len(sandboxes)
			result["worker_count"] = workerCount
		}
		if runtimeServices.Ports != nil {
			result["port_count"] = len(runtimeServices.Ports.List())
		}
		if runtimeServices.Pool != nil {
			profiles := runtimeServices.Pool.Status()
			desired, ready, failed := 0, 0, 0
			for _, profile := range profiles {
				desired += profile.Desired
				ready += profile.Ready
				failed += profile.Failed
			}
			result["warm_pool_profile_count"] = len(profiles)
			result["warm_pool_desired_count"] = desired
			result["warm_pool_ready_count"] = ready
			result["warm_pool_failed_count"] = failed
		}
		return result, nil
	}
}
