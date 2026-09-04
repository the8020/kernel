// Package synchronize implements package.synchronize.
package synchronize

import (
	"context"

	"the8020/kernel/cbus/commands/internal/commandutil"
	packagecommands "the8020/kernel/cbus/commands/package"
	"the8020/kernel/cbus/core"
	"the8020/kernel/services"
)

func New(serviceSet *services.Services) core.Handler {
	return func(ctx context.Context, request core.Request) (core.Result, error) {
		packages, err := packagecommands.SynchronizeWithCredential(ctx, serviceSet, commandutil.CSV(request, "packages"), commandutil.String(request, "git_token"))
		return core.Result{"packages": packages}, err
	}
}
