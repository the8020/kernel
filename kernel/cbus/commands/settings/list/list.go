// Package list implements the thin settings.list command handler.
package list

import (
	"context"

	"the8020/kernel/cbus/core"
	"the8020/kernel/services"
)

type summary struct {
	Key         string `json:"key"`
	Description string `json:"description"`
}

// New binds settings.list to the settings service.
func New(serviceSet *services.Services) core.Handler {
	return func(_ context.Context, request core.Request) (core.Result, error) {
		settings := serviceSet.Settings.List()
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
