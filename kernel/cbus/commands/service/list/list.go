// Package list implements service.list.
package list

import (
	"context"
	"the8020/kernel/cbus/commands/internal/commandutil"
	"the8020/kernel/cbus/core"
	"the8020/kernel/services"
	"the8020/kernel/webservices"
)

type summary struct {
	ServiceID         string            `json:"service_id"`
	Description       string            `json:"description,omitempty"`
	CanonicalBasePath string            `json:"canonical_base_path"`
	State             webservices.State `json:"state"`
	Enabled           bool              `json:"enabled"`
	InstanceCount     int               `json:"instance_count"`
	WorkerCount       int               `json:"worker_count"`
	ExecutionMode     string            `json:"execution_mode"`
	AccessMode        string            `json:"access_mode"`
	ValidationError   string            `json:"validation_error,omitempty"`
}

func New(serviceSet *services.Services) core.Handler {
	return func(_ context.Context, _ core.Request) (core.Result, error) {
		runtimeServices, err := commandutil.Runtime(serviceSet)
		if err != nil {
			return nil, err
		}
		if runtimeServices.Services == nil {
			return nil, core.NewError(core.CodeRuntimeUnavailable, "service manager is unavailable")
		}
		statuses, err := runtimeServices.Services.List()
		if err != nil {
			return nil, commandutil.OperationError(err)
		}
		items := make([]summary, 0, len(statuses))
		for _, status := range statuses {
			items = append(items, summary{
				ServiceID: status.ServiceID, CanonicalBasePath: status.CanonicalBasePath, Description: status.Description,
				Enabled: status.Enabled, State: status.State, InstanceCount: status.InstanceCount,
				WorkerCount: status.WorkerCount, ValidationError: status.ValidationError,
				ExecutionMode: status.ExecutionMode, AccessMode: status.AccessMode,
			})
		}
		return core.Result{"services": items}, nil
	}
}
