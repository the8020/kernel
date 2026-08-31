// Package remove implements node.remove.
package remove

import (
	"context"

	"the8020/kernel/cbus/commands/internal/commandutil"
	"the8020/kernel/cbus/core"
	"the8020/kernel/services"
)

func New(serviceSet *services.Services) core.Handler {
	return func(ctx context.Context, request core.Request) (core.Result, error) {
		if serviceSet.Nodes == nil {
			return nil, core.NewError(core.CodeRuntimeUnavailable, "node topology is unavailable")
		}
		id := commandutil.String(request, "node_id")
		if err := serviceSet.Nodes.Remove(ctx, id); err != nil {
			return nil, commandutil.OperationError(err)
		}
		return core.Result{"removed": true, "node_id": id}, nil
	}
}
