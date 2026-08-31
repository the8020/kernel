// Package restart implements the thin system.restart command handler.
package restart

import (
	"context"

	"the8020/kernel/cbus/core"
	"the8020/kernel/services"
)

// New binds system.restart to the process lifecycle service.
func New(serviceSet *services.Services) core.Handler {
	return func(_ context.Context, _ core.Request) (core.Result, error) {
		return core.Result{"requested": serviceSet.Lifecycle.RequestRestart()}, nil
	}
}
