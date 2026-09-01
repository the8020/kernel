// Package remote implements package.repository.remote.
package remote

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
		repository, err := management.ConfigurePackageRemote(ctx,
			commandutil.String(request, "package_id"), commandutil.String(request, "name"), commandutil.String(request, "url"))
		return core.Result{"repository": repository}, commandutil.OperationError(err)
	}
}
