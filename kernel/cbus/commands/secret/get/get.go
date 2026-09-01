// Package get implements secret.get.
package get

import (
	"context"

	"the8020/kernel/cbus/commands/internal/commandutil"
	secretcommands "the8020/kernel/cbus/commands/secret"
	"the8020/kernel/cbus/core"
	"the8020/kernel/services"
)

func New(serviceSet *services.Services) core.Handler {
	return func(_ context.Context, request core.Request) (core.Result, error) {
		service, err := secretcommands.Service(serviceSet)
		if err != nil {
			return nil, err
		}
		item, err := service.Get(commandutil.String(request, "name"))
		if err != nil {
			return nil, commandutil.OperationError(err)
		}
		return core.Result{"secret": item}, nil
	}
}
