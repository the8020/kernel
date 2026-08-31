// Package get implements the thin settings.get command handler.
package get

import (
	"context"
	"the8020/kernel/cbus/core"
	"the8020/kernel/services"
)

// New binds settings.get to the settings service.
func New(serviceSet *services.Services) core.Handler {
	return func(_ context.Context, request core.Request) (core.Result, error) {
		info, err := serviceSet.Settings.Get(request.Arguments["key"].(string))
		if err != nil {
			return nil, mapError(err)
		}
		return core.Result{"setting": info}, nil
	}
}

func mapError(err error) error { return core.NewError(core.CodeUnknownSetting, err.Error()) }
