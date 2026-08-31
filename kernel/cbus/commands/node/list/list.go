// Package list implements node.list.
package list

import (
	"context"

	"the8020/kernel/cbus/core"
	"the8020/kernel/services"
)

func New(serviceSet *services.Services) core.Handler {
	return func(ctx context.Context, _ core.Request) (core.Result, error) {
		if serviceSet.Nodes == nil {
			return nil, core.NewError(core.CodeRuntimeUnavailable, "node topology is unavailable")
		}
		return core.Result{"nodes": serviceSet.Nodes.Statuses(ctx), "local_node_id": serviceSet.Nodes.LocalNodeID()}, nil
	}
}
