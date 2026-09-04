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
			"logging_enabled": serviceSet.Logging.Enabled(),
			"active_log_file": serviceSet.Logging.ActiveFile(),
			"build_id":        serviceSet.Instance.BuildID,
		}
		if network := serviceSet.PlatformSnapshot().Network; network != nil {
			result["main_port"] = network.Port()
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
		if serviceSet.Database != nil {
			database := serviceSet.Database.Status()
			result["database_backend"] = database.Backend
			result["database_location"] = database.Location
			result["database_status"] = database.State
			result["database_catalog_version"] = database.CatalogVersion
			result["database_initialized"] = database.Initialized
			result["database_pending_deployment"] = database.PendingDeployment
			result["database_pool_maximum_open_connections"] = database.MaximumOpenConnections
			result["database_pool_maximum_idle_connections"] = database.MaximumIdleConnections
			result["database_pool_open_connections"] = database.OpenConnections
			result["database_pool_in_use_connections"] = database.InUseConnections
			result["database_pool_idle_connections"] = database.IdleConnections
			result["database_pool_wait_count"] = database.WaitCount
			result["database_pool_wait_duration_milliseconds"] = database.WaitDurationMilliseconds
			if database.Error != "" {
				result["database_error"] = database.Error
			}
			if database.CatalogError != "" {
				result["database_catalog_error"] = database.CatalogError
			}
			if database.LastDeploymentAt != "" {
				result["database_last_deployment_at"] = database.LastDeploymentAt
			}
			if database.LastDeploymentError != "" {
				result["database_last_deployment_error"] = database.LastDeploymentError
			}
		}
		return result, nil
	}
}
