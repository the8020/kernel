// Package shutdown implements the thin system.shutdown command handler.
package shutdown

import (
	"context"
	"the8020/kernel/cbus/core"
	"the8020/kernel/services"
)

// New binds system.shutdown to the lifecycle service.
func New(serviceSet *services.Services) core.Handler {
	return func(_ context.Context, _ core.Request) (core.Result, error) {
		serviceSet.Lifecycle.Request()
		return core.Result{"requested": true}, nil
	}
}
