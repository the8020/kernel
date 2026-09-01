// Package checkout implements package.repository.checkout.
package checkout

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
		mutation, err := management.CheckoutPackageRepository(ctx,
			commandutil.String(request, "package_id"), commandutil.String(request, "branch"), commandutil.String(request, "commit"))
		if err != nil {
			return nil, commandutil.OperationError(err)
		}
		if err := packagecommands.RefreshRepositoryMutation(ctx, serviceSet, mutation); err != nil {
			return nil, err
		}
		return core.Result{"repository": mutation.Repository}, nil
	}
}
