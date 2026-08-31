// Package list implements package.version.list.
package list

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
		versions, err := management.ListPackageVersions(ctx, commandutil.String(request, "package_id"), commandutil.Int(request, "limit"))
		return core.Result{"package": versions}, commandutil.OperationError(err)
	}
}
