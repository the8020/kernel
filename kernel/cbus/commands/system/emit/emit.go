// Package emit implements kernel.events.emit.
package emit

import (
	"context"

	"the8020/kernel/cbus/commands/internal/commandutil"
	"the8020/kernel/cbus/core"
	"the8020/kernel/execution"
	"the8020/kernel/services"
)

func New(serviceSet *services.Services) core.Handler {
	return func(ctx context.Context, request core.Request) (core.Result, error) {
		runtime, err := commandutil.Runtime(serviceSet)
		if err != nil {
			return nil, err
		}
		if runtime.Events == nil {
			return nil, core.NewError(core.CodeRuntimeUnavailable, "event dispatcher is unavailable")
		}
		data, err := commandutil.JSON(request, "data")
		if err != nil {
			return nil, err
		}
		receipt, err := runtime.Events.Emit(commandutil.String(request, "event"), data, execution.DefaultUser(ctx))
		if err != nil {
			return nil, commandutil.OperationError(err)
		}
		return core.Result{"id": receipt.ID, "listeners": receipt.Listeners}, nil
	}
}
