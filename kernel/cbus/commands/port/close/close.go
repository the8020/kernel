// Package close implements port.close.
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
		if runtimeServices.Ports == nil {
			return nil, core.NewError(core.CodeRuntimeUnavailable, "port manager is unavailable")
		}
		if err := runtimeServices.Ports.Close(commandutil.String(request, "lease_id")); err != nil {
			return nil, commandutil.OperationError(err)
		}
		return core.Result{"closed": true}, nil
	}
}
