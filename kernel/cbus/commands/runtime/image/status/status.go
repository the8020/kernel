// Package status implements runtime.image.status.
package status

import (
	"context"

	"the8020/kernel/cbus/core"
	"the8020/kernel/services"
)

func New(serviceSet *services.Services) core.Handler {
	return func(ctx context.Context, _ core.Request) (core.Result, error) {
		runtimeServices := serviceSet.RuntimeSnapshot()
		if runtimeServices == nil || runtimeServices.Doctor == nil {
			return nil, core.NewError(core.CodeRuntimeUnavailable, "runtime image diagnostics are unavailable")
		}
		report := runtimeServices.Doctor.Inspect(ctx)
		return core.Result{"image": map[string]any{
			"name": report.RuntimeImageName, "digest": report.RuntimeImageDigest,
			"recorded": report.RuntimeImageRecorded, "available": report.RuntimeImageAvailable,
			"deno_version": runtimeServices.Versions.Deno.Version,
			"smoke_status": report.GVisorSmokeStatus, "smoke_passed_at": report.GVisorSmokePassedAt,
		}}, nil
	}
}
