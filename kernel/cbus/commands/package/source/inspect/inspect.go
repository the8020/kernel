// Package inspect implements package.source.inspect.
package inspect

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
		source, err := management.InspectPackageSource(ctx, commandutil.String(request, "source"))
		return core.Result{"source": source}, commandutil.OperationError(err)
	}
}
