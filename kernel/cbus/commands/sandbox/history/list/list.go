// Package list implements sandbox.history.list.
package list

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
		if runtimeServices.Sandboxes == nil {
			return nil, core.NewError(core.CodeRuntimeUnavailable, "sandbox manager is unavailable")
		}
		page, err := runtimeServices.Sandboxes.ListHistory(commandutil.Int(request, "limit"), commandutil.String(request, "before"))
		if err != nil {
			return nil, commandutil.OperationError(err)
		}
		return core.Result{"sandboxes": page.Sandboxes, "next_cursor": page.NextCursor}, nil
	}
}
