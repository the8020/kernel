package activationinspect

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
		inspector, ok := management.(interface {
			InspectActivation(context.Context, string) (packages.ActivationInspection, error)
		})
		if !ok {
			return nil, core.NewError(core.CodeRuntimeUnavailable, "activation inspection is unavailable")
		}
		result, err := inspector.InspectActivation(ctx, commandutil.String(request, "activation_id"))
		return core.Result{"activation": result}, commandutil.OperationError(err)
	}
}
