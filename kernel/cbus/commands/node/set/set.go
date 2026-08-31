// Package set implements node.set.
package set

import (
	"context"

	"the8020/kernel/cbus/commands/internal/commandutil"
	"the8020/kernel/cbus/core"
	"the8020/kernel/nodes"
	"the8020/kernel/services"
)

func New(serviceSet *services.Services) core.Handler {
	return func(ctx context.Context, request core.Request) (core.Result, error) {
		if serviceSet.Nodes == nil {
			return nil, core.NewError(core.CodeRuntimeUnavailable, "node topology is unavailable")
		}
		node, err := serviceSet.Nodes.Set(ctx, nodes.Node{
			ID: commandutil.String(request, "node_id"), URL: commandutil.String(request, "url"),
			RecipientAddress: commandutil.String(request, "recipient_address"), RecipientPort: commandutil.Int(request, "recipient_port"),
			Enabled: commandutil.Bool(request, "enabled"),
		})
		if err != nil {
			return nil, commandutil.OperationError(err)
		}
		return core.Result{"node": node}, nil
	}
}
