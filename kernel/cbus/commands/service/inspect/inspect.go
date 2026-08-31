// Package inspect implements service.inspect.
package inspect

import (
	"context"
	"the8020/kernel/cbus/commands/internal/commandutil"
	"the8020/kernel/cbus/core"
	"the8020/kernel/services"
)

func New(serviceSet *services.Services) core.Handler {
	return func(_ context.Context, request core.Request) (core.Result, error) {
		runtimeServices, err := commandutil.Runtime(serviceSet)
		if err != nil {
			return nil, err
		}
		if runtimeServices.Services == nil {
			return nil, core.NewError(core.CodeRuntimeUnavailable, "service manager is unavailable")
		}
		item, err := runtimeServices.Services.Inspect(commandutil.String(request, "service_id"))
		if err != nil {
			return nil, commandutil.OperationError(err)
		}
		return core.Result{"service": item}, nil
	}
}
