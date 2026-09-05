// Package reindex implements kernel.reindex.
package reindex

import (
	"context"

	"the8020/kernel/cbus/commands/internal/commandutil"
	"the8020/kernel/cbus/core"
	"the8020/kernel/services"
)

// New refreshes native declarations and Deno-provided service fragments.
func New(serviceSet *services.Services) core.Handler {
	return func(ctx context.Context, request core.Request) (core.Result, error) {
		runtimeServices, err := commandutil.Runtime(serviceSet)
		if err != nil {
			return nil, err
		}
		if runtimeServices.Reindex == nil {
			return nil, core.NewError(core.CodeRuntimeUnavailable, "package indexing is unavailable")
		}
		result, err := runtimeServices.Reindex(ctx, commandutil.CSV(request, "packages"))
		if err != nil {
			return nil, &core.Error{Code: core.CodeRuntimeOperation, Message: err.Error(), Details: result}
		}
		return result, nil
	}
}
