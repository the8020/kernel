// Package doctor implements runtime.doctor.
package doctor

import (
	"context"

	"the8020/kernel/cbus/core"
	"the8020/kernel/services"
)

func New(serviceSet *services.Services) core.Handler {
	return func(ctx context.Context, _ core.Request) (core.Result, error) {
		runtimeServices := serviceSet.RuntimeSnapshot()
		if runtimeServices == nil || runtimeServices.Doctor == nil {
			return nil, core.NewError(core.CodeRuntimeUnavailable, "runtime diagnostics are unavailable")
		}
		result := core.Result{"full": runtimeServices.Doctor.Inspect(ctx), "selection": runtimeServices.Isolation}
		if runtimeServices.RootlessDoctor != nil {
			result["rootless"] = runtimeServices.RootlessDoctor.Inspect(ctx)
		}
		return result, nil
	}
}
