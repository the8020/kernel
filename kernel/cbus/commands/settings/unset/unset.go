// Package unset implements the thin kernel.config.unset command handler.
package unset

import (
	"context"
	"fmt"

	"the8020/kernel/cbus/commands/settings/set"
	"the8020/kernel/cbus/core"
	"the8020/kernel/services"
	settingservice "the8020/kernel/settings"
)

// New binds kernel.config.unset to the settings transaction service.
func New(serviceSet *services.Services) core.Handler {
	return func(ctx context.Context, request core.Request) (core.Result, error) {
		key := request.Arguments["key"].(string)
		current, err := serviceSet.Settings.Get(key)
		if err == nil && current.Storage != settingservice.StorageNode {
			return nil, core.NewError(core.CodeInvalidArguments, fmt.Sprintf("setting %s is global; use system.settings.unset", key))
		}
		if err != nil {
			return nil, set.MapError(err)
		}
		info, err := serviceSet.Settings.Unset(ctx, key)
		if err != nil {
			return nil, set.MapError(err)
		}
		return core.Result{"setting": info}, nil
	}
}
