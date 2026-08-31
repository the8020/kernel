// Package get implements node.paths.get.
package get

import (
	"context"

	"the8020/kernel/cbus/commands/internal/commandutil"
	"the8020/kernel/cbus/core"
	"the8020/kernel/services"
)

func New(serviceSet *services.Services) core.Handler {
	return func(context.Context, core.Request) (core.Result, error) {
		if serviceSet.Layout == nil {
			return nil, core.NewError(core.CodeRuntimeUnavailable, "node layout is unavailable")
		}
		layout, err := serviceSet.Layout.Current()
		if err != nil {
			return nil, commandutil.OperationError(err)
		}
		return core.Result{"layout": layout}, nil
	}
}
