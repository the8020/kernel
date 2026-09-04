// Package refresh implements the private service.refresh runtime operation.
package refresh

import (
	"context"

	"the8020/kernel/cbus/commands/internal/commandutil"
	serviceview "the8020/kernel/cbus/commands/service"
	"the8020/kernel/cbus/core"
	"the8020/kernel/services"
)

func New(serviceSet *services.Services) core.Handler {
	return func(ctx context.Context, request core.Request) (core.Result, error) {
		runtimeServices, err := commandutil.Runtime(serviceSet)
		if err != nil {
			return nil, err
		}
		if runtimeServices.Services == nil || runtimeServices.Sandboxes == nil {
			return nil, core.NewError(core.CodeRuntimeUnavailable, "service runtime is unavailable")
		}
		item, err := runtimeServices.Services.Inspect(commandutil.String(request, "service_id"))
		if err != nil {
			return nil, commandutil.OperationError(err)
		}
		item, err = serviceview.Refresh(ctx, item, runtimeServices.Sandboxes)
		if err != nil {
			return nil, commandutil.OperationError(err)
		}
		return core.Result{"service": item}, nil
	}
}
