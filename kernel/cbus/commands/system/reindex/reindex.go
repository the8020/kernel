// Package reindex implements kernel.reindex.
package reindex

import (
	"context"

	"the8020/kernel/cbus/commands/internal/commandutil"
	"the8020/kernel/cbus/core"
	"the8020/kernel/services"
)

// New refreshes package-owned commands from the active package set.
func New(serviceSet *services.Services) core.Handler {
	return func(ctx context.Context, _ core.Request) (core.Result, error) {
		runtimeServices, err := commandutil.Runtime(serviceSet)
		if err != nil {
			return nil, err
		}
		if runtimeServices.ReindexCommands == nil {
			return nil, core.NewError(core.CodeRuntimeUnavailable, "package command indexing is unavailable")
		}
		return runtimeServices.ReindexCommands(ctx)
	}
}
