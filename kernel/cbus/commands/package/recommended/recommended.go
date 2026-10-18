package recommended

import (
	"context"
	"errors"

	"the8020/kernel/cbus/commands/internal/commandutil"
	packagecommands "the8020/kernel/cbus/commands/package"
	"the8020/kernel/cbus/core"
	"the8020/kernel/services"
)

func New(serviceSet *services.Services) core.Handler {
	return func(ctx context.Context, _ core.Request) (core.Result, error) {
		management, err := packagecommands.Management(serviceSet)
		if err != nil {
			return nil, err
		}
		results, err := management.InstallRecommendedPackages(ctx)
		failure := commandutil.OperationError(err)
		var commandError *core.Error
		if errors.As(failure, &commandError) {
			commandError.Details = map[string]any{"packages": results}
		}
		return core.Result{"packages": results}, failure
	}
}
