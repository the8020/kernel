// Package inspect implements package.inspect.
package inspect

import (
	"context"

	"the8020/kernel/cbus/commands/internal/commandutil"
	"the8020/kernel/cbus/core"
	"the8020/kernel/services"
)

func New(serviceSet *services.Services) core.Handler {
	return func(_ context.Context, request core.Request) (core.Result, error) {
		service := serviceSet.PlatformSnapshot().Packages
		if service == nil {
			return nil, core.NewError(core.CodeRuntimeUnavailable, "package store is unavailable")
		}
		item, err := service.InspectPackage(commandutil.String(request, "package_id"))
		if err != nil {
			return nil, commandutil.OperationError(err)
		}
		return core.Result{"package": item}, nil
	}
}
