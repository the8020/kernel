package activationresume

import (
	"context"

	"the8020/kernel/cbus/commands/internal/commandutil"
	packagecommands "the8020/kernel/cbus/commands/package"
	"the8020/kernel/cbus/core"
	"the8020/kernel/packages"
	"the8020/kernel/services"
)

func New(serviceSet *services.Services) core.Handler {
	return func(ctx context.Context, request core.Request) (core.Result, error) {
		management, err := packagecommands.Management(serviceSet)
		if err != nil {
			return nil, err
		}
		owner, ok := management.(interface {
			ResumeSynchronization(context.Context, string) (packages.ActivationInspection, error)
		})
		if !ok {
			return nil, core.NewError(core.CodeRuntimeUnavailable, "staged-set recovery is unavailable")
		}
		result, err := owner.ResumeSynchronization(ctx, commandutil.String(request, "activation_id"))
		return core.Result{"activation": result}, commandutil.OperationError(err)
	}
}
