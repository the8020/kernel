package synchronizeset

import (
	"context"

	"the8020/kernel/cbus/commands/internal/commandutil"
	packagecommands "the8020/kernel/cbus/commands/package"
	"the8020/kernel/cbus/core"
	"the8020/kernel/deployment"
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
			SynchronizationSetAvailable(context.Context) error
			SynchronizeSet(context.Context, string, string, string) ([]packages.PackageSynchronization, error)
		})
		if !ok {
			return nil, core.NewError(core.CodeRuntimeUnavailable, "staged-set synchronization is unavailable")
		}
		if err := owner.SynchronizationSetAvailable(ctx); err != nil {
			return nil, commandutil.OperationError(err)
		}
		input := commandutil.String(request, "input")
		if input == "" {
			return core.Result{"protocol": 1, "maximum": 256}, nil
		}
		ctx = deployment.WithSchemaOptions(ctx, deployment.SchemaOptions{ForceColumns: commandutil.CSV(request, "force_columns"), ApprovedPlan: commandutil.String(request, "approved_plan")})
		result, err := owner.SynchronizeSet(ctx, input, commandutil.String(request, "git_token"), commandutil.String(request, "git_username"))
		return core.Result{"packages": result}, commandutil.OperationError(err)
	}
}
