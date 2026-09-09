package delete

import (
	"context"

	"the8020/kernel/cbus/commands/internal/commandutil"
	packagecommands "the8020/kernel/cbus/commands/package"
	"the8020/kernel/cbus/core"
	"the8020/kernel/services"
)

func New(serviceSet *services.Services) core.Handler {
	return func(ctx context.Context, request core.Request) (core.Result, error) {
		if confirmed, _ := request.Arguments["confirm"].(bool); !confirmed {
			return nil, core.NewError(core.CodeInvalidArguments, "package deletion requires --confirm")
		}
		management, err := packagecommands.Management(serviceSet)
		if err != nil {
			return nil, err
		}
		err = management.DeletePackage(ctx, commandutil.String(request, "package_id"))
		return core.Result{"deleted": err == nil}, commandutil.OperationError(err)
	}
}
