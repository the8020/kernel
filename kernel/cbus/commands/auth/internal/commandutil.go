// Package internal adapts bootstrap-authentication dependencies and errors for
// thin administrative command handlers.
package internal

import (
	"errors"

	"the8020/kernel/auth"
	"the8020/kernel/cbus/core"
	"the8020/kernel/services"
)

func Service(serviceSet *services.Services) (services.AuthService, error) {
	if serviceSet == nil || serviceSet.Auth == nil {
		return nil, core.NewError(core.CodeRuntimeUnavailable, "bootstrap authentication is unavailable")
	}
	return serviceSet.Auth, nil
}

func String(request core.Request, name string) string {
	value, _ := request.Arguments[name].(string)
	return value
}

func Error(err error) error {
	switch {
	case errors.Is(err, auth.ErrInvalidUsername), errors.Is(err, auth.ErrInvalidPassword), errors.Is(err, auth.ErrInvalidSessionID):
		return core.NewError(core.CodeInvalidArguments, err.Error())
	case errors.Is(err, auth.ErrDuplicateUser):
		return core.NewError(core.CodeConflict, err.Error())
	case errors.Is(err, auth.ErrUserNotFound):
		return core.NewError(core.CodeNotFound, err.Error())
	default:
		return err
	}
}
