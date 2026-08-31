// Package resize implements pool.resize.
package resize

import (
	"context"
	"the8020/kernel/cbus/commands/internal/commandutil"
	"the8020/kernel/cbus/core"
	"the8020/kernel/services"
)

func New(serviceSet *services.Services) core.Handler {
	return func(_ context.Context, request core.Request) (core.Result, error) {
		runtimeServices, err := commandutil.Runtime(serviceSet)
		if err != nil {
			return nil, err
		}
		if runtimeServices.Pool == nil {
			return nil, core.NewError(core.CodeRuntimeUnavailable, "warm-pool manager is unavailable")
		}
		profile := commandutil.String(request, "profile")
		if err := runtimeServices.Pool.Resize(profile, commandutil.Int(request, "count")); err != nil {
			return nil, commandutil.OperationError(err)
		}
		for _, item := range runtimeServices.Pool.Status() {
			if item.ProfileHash == profile {
				return core.Result{"profile": item}, nil
			}
		}
		return nil, core.NewError(core.CodeRuntimeOperation, "warm-pool profile status is unavailable")
	}
}
