// Package cancel implements job.cancel.
package cancel

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
		if runtimeServices.Jobs == nil {
			return nil, core.NewError(core.CodeRuntimeUnavailable, "job manager is unavailable")
		}
		if err := runtimeServices.Jobs.Cancel(ctx, commandutil.String(request, "execution_id")); err != nil {
			return nil, commandutil.OperationError(err)
		}
		return core.Result{"cancelled": true}, nil
	}
}
