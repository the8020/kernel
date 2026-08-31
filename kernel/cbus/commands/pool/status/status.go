// Package status implements pool.status.
package status

import (
	"context"
	"the8020/kernel/cbus/commands/internal/commandutil"
	"the8020/kernel/cbus/core"
	"the8020/kernel/services"
)

func New(serviceSet *services.Services) core.Handler {
	return func(_ context.Context, _ core.Request) (core.Result, error) {
		runtimeServices, err := commandutil.Runtime(serviceSet)
		if err != nil {
			return nil, err
		}
		if runtimeServices.Pool == nil {
			return nil, core.NewError(core.CodeRuntimeUnavailable, "warm-pool manager is unavailable")
		}
		return core.Result{"profiles": runtimeServices.Pool.Status()}, nil
	}
}
