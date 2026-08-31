// Package unset implements the thin settings.unset command handler.
package unset

import (
	"context"

	"the8020/kernel/cbus/commands/settings/set"
	"the8020/kernel/cbus/core"
	"the8020/kernel/services"
)

// New binds settings.unset to the settings transaction service.
func New(serviceSet *services.Services) core.Handler {
	return func(ctx context.Context, request core.Request) (core.Result, error) {
		info, err := serviceSet.Settings.Unset(ctx, request.Arguments["key"].(string))
		if err != nil {
			return nil, set.MapError(err)
		}
		return core.Result{"setting": info}, nil
	}
}
