// Package inspect implements sandbox.inspect.
package inspect

import (
	"context"

	"the8020/kernel/cbus/commands/internal/commandutil"
	sandboxview "the8020/kernel/cbus/commands/sandbox"
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
		item, err := runtimeServices.Sandboxes.Inspect(ctx, commandutil.String(request, "sandbox_id"))
		if err != nil {
			return nil, commandutil.OperationError(err)
		}
		result, err := sandboxview.Detail(runtimeServices, item)
		if err != nil {
			return nil, commandutil.OperationError(err)
		}
		return result, nil
	}
}
