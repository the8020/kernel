// Package open implements debug.open.
package open

import (
	"context"
	"the8020/kernel/cbus/commands/internal/commandutil"
	"the8020/kernel/cbus/core"
	"the8020/kernel/services"
	"time"
)

func New(serviceSet *services.Services) core.Handler {
	return func(ctx context.Context, request core.Request) (core.Result, error) {
		runtimeServices, err := commandutil.Runtime(serviceSet)
		if err != nil {
			return nil, err
		}
		if runtimeServices.Debugging == nil || runtimeServices.Sandboxes == nil {
			return nil, core.NewError(core.CodeRuntimeUnavailable, "debug or sandbox manager is unavailable")
		}
		inspection, err := runtimeServices.Sandboxes.Inspect(ctx, commandutil.String(request, "sandbox_id"))
		if err != nil {
			return nil, commandutil.OperationError(err)
		}
		duration := time.Duration(commandutil.Int(request, "duration")) * time.Second
		lease, err := runtimeServices.Debugging.Open(ctx, inspection.Spec, duration)
		if err != nil {
			return nil, commandutil.OperationError(err)
		}
		return core.Result{"lease": lease}, nil
	}
}
