// Package get implements the thin kernel.config.get command handler.
package get

import (
	"context"
	"fmt"

	"the8020/kernel/cbus/core"
	"the8020/kernel/services"
	settingservice "the8020/kernel/settings"
)

// New binds kernel.config.get to the settings service.
func New(serviceSet *services.Services) core.Handler {
	return func(_ context.Context, request core.Request) (core.Result, error) {
		info, err := serviceSet.Settings.Get(request.Arguments["key"].(string))
		if err != nil {
			return nil, mapError(err)
		}
		if info.Storage != settingservice.StorageNode {
			return nil, core.NewError(core.CodeInvalidArguments, fmt.Sprintf("setting %s is global; use system.settings.get", info.Key))
		}
		return core.Result{"setting": info}, nil
	}
}

func mapError(err error) error { return core.NewError(core.CodeUnknownSetting, err.Error()) }
