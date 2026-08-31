// Package set implements package.index.set.
package set

import (
	"context"

	"the8020/kernel/cbus/commands/internal/commandutil"
	packagecommands "the8020/kernel/cbus/commands/package"
	"the8020/kernel/cbus/core"
	workspacepackages "the8020/kernel/packages"
	"the8020/kernel/services"
)

func New(serviceSet *services.Services) core.Handler {
	return func(ctx context.Context, request core.Request) (core.Result, error) {
		management, err := packagecommands.Management(serviceSet)
		if err != nil {
			return nil, err
		}
		entry, err := management.SetPackageIndex(ctx, workspacepackages.PackageIndex{
			Author: commandutil.String(request, "author"), Repository: commandutil.String(request, "repository"),
			Source: commandutil.String(request, "source"), Commit: commandutil.String(request, "commit"),
			Tag: commandutil.String(request, "tag"), Local: commandutil.Bool(request, "local"),
		})
		return core.Result{"package": entry}, commandutil.OperationError(err)
	}
}
