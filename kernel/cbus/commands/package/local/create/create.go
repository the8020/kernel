// Package create implements package.local.create.
package create

import (
	"context"

	"the8020/kernel/cbus/commands/internal/commandutil"
	packagecommands "the8020/kernel/cbus/commands/package"
	"the8020/kernel/cbus/core"
	"the8020/kernel/services"
)

func New(serviceSet *services.Services) core.Handler {
	return func(ctx context.Context, request core.Request) (core.Result, error) {
		management, err := packagecommands.Management(serviceSet)
		if err != nil {
			return nil, err
		}
		created, err := management.CreateLocalPackage(ctx, commandutil.String(request, "author"), commandutil.String(request, "repository"), commandutil.String(request, "description"))
		return core.Result{"package": created}, commandutil.OperationError(err)
	}
}
