// Package set implements the thin settings.set command handler.
package set

import (
	"context"
	"errors"

	"the8020/kernel/cbus/core"
	"the8020/kernel/logging"
	"the8020/kernel/network"
	"the8020/kernel/services"
	settingservice "the8020/kernel/settings"
)

// New binds settings.set to the settings transaction service.
func New(serviceSet *services.Services) core.Handler {
	return func(ctx context.Context, request core.Request) (core.Result, error) {
		info, err := serviceSet.Settings.Set(ctx, request.Arguments["key"].(string), request.Arguments["value"].(string))
		if err != nil {
			return nil, MapError(err)
		}
		return core.Result{"setting": info}, nil
	}
}

// MapError translates owned domain failures to stable command-bus codes.
func MapError(err error) error {
	var operation *settingservice.OperationError
	if errors.As(err, &operation) {
		switch operation.Kind {
		case settingservice.ErrorUnknown:
			return core.NewError(core.CodeUnknownSetting, err.Error())
		case settingservice.ErrorInvalid:
			return core.NewError(core.CodeInvalidSettingValue, err.Error())
		case settingservice.ErrorNotMutable:
			return core.NewError(core.CodeNotRuntimeMutable, err.Error())
		case settingservice.ErrorPersistence:
			return core.NewError(core.CodePersistence, err.Error())
		case settingservice.ErrorApplication:
			if errors.Is(err, network.ErrPortUnavailable) {
				return core.NewError(core.CodePortUnavailable, err.Error())
			}
			if errors.Is(err, logging.ErrInitialization) {
				return core.NewError(core.CodeLoggingInit, err.Error())
			}
		}
	}
	return err
}
