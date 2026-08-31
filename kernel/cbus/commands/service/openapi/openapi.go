// Package openapi implements service.openapi.
package openapi

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
		if runtimeServices.Services == nil {
			return nil, core.NewError(core.CodeRuntimeUnavailable, "service manager is unavailable")
		}
		document, err := runtimeServices.Services.OpenAPI(ctx, commandutil.String(request, "service_id"))
		if err != nil {
			return nil, commandutil.OperationError(err)
		}
		return core.Result{"openapi": document}, nil
	}
}
