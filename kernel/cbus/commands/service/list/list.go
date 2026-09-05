// Package list implements service.list.
package list

import (
	"context"
	"the8020/kernel/cbus/commands/internal/commandutil"
	serviceview "the8020/kernel/cbus/commands/service"
	"the8020/kernel/cbus/core"
	"the8020/kernel/services"
	"the8020/kernel/webservices"
)

type summary struct {
	ServiceID         string            `json:"service_id"`
	PackageID         string            `json:"package_id"`
	Entrypoint        string            `json:"source_entrypoint"`
	Description       string            `json:"description,omitempty"`
	CanonicalBasePath string            `json:"canonical_base_path"`
	State             webservices.State `json:"state"`
	Enabled           bool              `json:"enabled"`
	VersionCount      int               `json:"version_count"`
	SandboxCount      int               `json:"sandbox_count"`
	WorkerCount       int               `json:"worker_count"`
	ServiceType       string            `json:"service_type"`
	AccessMode        string            `json:"access_mode"`
	ValidationError   string            `json:"validation_error,omitempty"`
}

func New(serviceSet *services.Services) core.Handler {
	return func(ctx context.Context, _ core.Request) (core.Result, error) {
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
			status = serviceview.Observed(ctx, status, runtimeServices.Sandboxes)
			items = append(items, summary{
				ServiceID: status.ServiceID, CanonicalBasePath: status.CanonicalBasePath, Description: status.Description,
				PackageID: status.PackageID, Entrypoint: status.Entrypoint,
				Enabled: status.Enabled, State: status.State, VersionCount: status.VersionCount, SandboxCount: status.SandboxCount,
				WorkerCount: status.WorkerCount, ValidationError: status.ValidationError,
				ServiceType: status.ServiceType, AccessMode: status.AccessMode,
			})
		}
		return core.Result{"services": items}, nil
	}
}
