// Package set implements node.paths.set.
package set

import (
	"context"

	"the8020/kernel/cbus/commands/internal/commandutil"
	"the8020/kernel/cbus/core"
	"the8020/kernel/instance"
	"the8020/kernel/services"
)

func New(serviceSet *services.Services) core.Handler {
	return func(_ context.Context, request core.Request) (core.Result, error) {
		if serviceSet.Layout == nil {
			return nil, core.NewError(core.CodeRuntimeUnavailable, "node layout is unavailable")
		}
		layout, err := serviceSet.Layout.Set(instance.Layout{
			Packages: commandutil.String(request, "packages"),
			Config:   commandutil.String(request, "config"),
			Users:    commandutil.String(request, "users"),
			State:    commandutil.String(request, "state"),
		})
		if err != nil {
			return nil, commandutil.OperationError(err)
		}
		return core.Result{"layout": layout}, nil
	}
}
