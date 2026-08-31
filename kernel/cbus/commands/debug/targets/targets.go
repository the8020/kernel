// Package targets implements debug.targets.
package targets

import (
	"context"

	"the8020/kernel/cbus/commands/internal/commandutil"
	"the8020/kernel/cbus/core"
	"the8020/kernel/services"
)

type summary struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	Title       string `json:"title"`
	ExecutionID string `json:"execution_id,omitempty"`
	Description string `json:"description,omitempty"`
}

func New(serviceSet *services.Services) core.Handler {
	return func(ctx context.Context, request core.Request) (core.Result, error) {
		runtimeServices, err := commandutil.Runtime(serviceSet)
		if err != nil {
			return nil, err
		}
		if runtimeServices.Debugging == nil || runtimeServices.Sandboxes == nil {
			return nil, core.NewError(core.CodeRuntimeUnavailable, "debug or sandbox manager is unavailable")
		}
		inspection, err := runtimeServices.Sandboxes.Inspect(ctx, commandutil.String(request, "sandbox_id"))
		if err != nil {
			return nil, commandutil.OperationError(err)
		}
		targets, err := runtimeServices.Debugging.Targets(ctx, inspection.Spec)
		if err != nil {
			return nil, commandutil.OperationError(err)
		}
		items := make([]summary, 0, len(targets))
		for _, target := range targets {
			items = append(items, summary{ID: target.ID, Type: target.Type, Title: target.Title, ExecutionID: target.ExecutionID, Description: target.Description})
		}
		return core.Result{"targets": items}, nil
	}
}
