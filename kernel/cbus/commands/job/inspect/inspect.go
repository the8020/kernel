// Package inspect implements job.inspect.
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
		if runtimeServices.Jobs == nil {
			return nil, core.NewError(core.CodeRuntimeUnavailable, "job manager is unavailable")
		}
		item, err := runtimeServices.Jobs.Inspect(commandutil.String(request, "execution_id"))
		if err != nil {
			return nil, commandutil.OperationError(err)
		}
		return core.Result{"execution": item}, nil
	}
}
