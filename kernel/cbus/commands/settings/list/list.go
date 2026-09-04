// Package list implements the thin kernel.config.list command handler.
package list

import (
	"context"

	"the8020/kernel/cbus/core"
	"the8020/kernel/services"
	settingservice "the8020/kernel/settings"
)

type summary struct {
	Key         string `json:"key"`
	Description string `json:"description"`
}

// New binds kernel.config.list to the settings service.
func New(serviceSet *services.Services) core.Handler {
	return func(_ context.Context, request core.Request) (core.Result, error) {
		all := serviceSet.Settings.List()
		settings := make([]settingservice.Info, 0, len(all))
		for _, setting := range all {
			if setting.Storage == settingservice.StorageNode {
				settings = append(settings, setting)
			}
		}
		view, detailed := request.Arguments["view"]
		if detailed {
			if view != "detail" {
				return nil, core.NewError(core.CodeInvalidArguments, "view must be detail")
			}
			return core.Result{"settings": settings}, nil
		}
		summaries := make([]summary, len(settings))
		for index, setting := range settings {
			summaries[index] = summary{Key: setting.Key, Description: setting.Description}
		}
		return core.Result{"settings": summaries}, nil
	}
}
