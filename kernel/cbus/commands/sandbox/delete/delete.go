// Package delete implements sandbox.delete.
package delete

import (
	"context"
	"the8020/kernel/cbus/commands/internal/commandutil"
	"the8020/kernel/cbus/core"
	"the8020/kernel/services"
)

func New(serviceSet *services.Services) core.Handler {
	return func(ctx context.Context, request core.Request) (core.Result, error) {
		runtimeServices, err := commandutil.Runtime(serviceSet)
		if err != nil {
			return nil, err
		}
		if runtimeServices.Sandboxes == nil {
			return nil, core.NewError(core.CodeRuntimeUnavailable, "sandbox manager is unavailable")
		}
		inspection, err := runtimeServices.Sandboxes.Inspect(ctx, commandutil.String(request, "sandbox_id"))
		if err != nil {
			return nil, commandutil.OperationError(err)
		}
		if runtimeServices.Ports != nil {
			for _, lease := range runtimeServices.Ports.List() {
				if lease.SandboxID == inspection.Spec.SandboxID {
					if err := runtimeServices.Ports.Close(lease.LeaseID); err != nil {
						return nil, commandutil.OperationError(err)
					}
				}
			}
		}
		if err := runtimeServices.Sandboxes.Delete(ctx, commandutil.String(request, "sandbox_id")); err != nil {
			return nil, commandutil.OperationError(err)
		}
		if runtimeServices.Pool != nil {
			if err := runtimeServices.Pool.Forget(inspection.Spec.RuntimeGroupID); err != nil {
				return nil, commandutil.OperationError(err)
			}
		}
		return core.Result{"deleted": true}, nil
	}
}
