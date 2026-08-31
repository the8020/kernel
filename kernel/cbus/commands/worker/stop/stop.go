// Package stop implements worker.stop.
package stop

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
		if runtimeServices.Workers == nil {
			return nil, core.NewError(core.CodeRuntimeUnavailable, "Worker manager is unavailable")
		}
		if err := runtimeServices.Workers.Stop(ctx, commandutil.String(request, "worker_id"), false); err != nil {
			return nil, commandutil.OperationError(err)
		}
		return core.Result{"stopped": true}, nil
	}
}
