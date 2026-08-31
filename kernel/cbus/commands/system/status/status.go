// Package status implements the thin system.status command handler.
package status

import (
	"context"
	"time"

	"the8020/kernel/cbus/core"
	"the8020/kernel/services"
)

// New binds system.status to the current typed services.
func New(serviceSet *services.Services) core.Handler {
	return func(_ context.Context, _ core.Request) (core.Result, error) {
		result := core.Result{
			"instance_uuid":   serviceSet.Instance.UUID,
			"pid":             serviceSet.Instance.PID,
			"instance_root":   serviceSet.Instance.Paths.Root,
			"uptime":          time.Since(serviceSet.Instance.StartedAt).Round(time.Millisecond).String(),
			"admin_socket":    serviceSet.Instance.Paths.Socket,
			"main_port":       serviceSet.Network.Port(),
			"logging_enabled": serviceSet.Logging.Enabled(),
			"active_log_file": serviceSet.Logging.ActiveFile(),
			"build_id":        serviceSet.Instance.BuildID,
		}
		shutdown := serviceSet.Lifecycle.Snapshot()
		result["shutdown_requested"] = shutdown.Requested
		result["restart_requested"] = shutdown.RestartRequested
		result["shutdown_percent"] = shutdown.Percent
		result["shutdown_completed_steps"] = shutdown.CompletedSteps
		result["shutdown_total_steps"] = shutdown.TotalSteps
		result["shutdown_step"] = shutdown.Step
		result["shutdown_message"] = shutdown.Message
		if runtimeServices := serviceSet.RuntimeSnapshot(); runtimeServices != nil {
			result["runtime_ready"] = runtimeServices.Failure == ""
			result["runtime_mode"] = runtimeServices.Isolation.SelectedMode
			result["runtime_failure"] = runtimeServices.Failure
		}
		return result, nil
	}
}
