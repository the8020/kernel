// Package inspect implements package.index.inspect.
package inspect

import (
	"context"

	"the8020/kernel/cbus/commands/internal/commandutil"
	packagecommands "the8020/kernel/cbus/commands/package"
	"the8020/kernel/cbus/core"
	"the8020/kernel/services"
)

func New(serviceSet *services.Services) core.Handler {
	return func(_ context.Context, request core.Request) (core.Result, error) {
		management, err := packagecommands.Management(serviceSet)
		if err != nil {
			return nil, err
		}
		entry, err := management.InspectPackageIndex(commandutil.String(request, "package_id"))
		return core.Result{"package": entry}, commandutil.OperationError(err)
	}
}
