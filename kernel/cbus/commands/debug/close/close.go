// Package close implements debug.close.
package close

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
		if runtimeServices.Debugging == nil {
			return nil, core.NewError(core.CodeRuntimeUnavailable, "debug manager is unavailable")
		}
		if err := runtimeServices.Debugging.Close(commandutil.String(request, "lease_id")); err != nil {
			return nil, commandutil.OperationError(err)
		}
		return core.Result{"closed": true}, nil
	}
}
