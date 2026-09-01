// Package list implements secret.list.
package list

import (
	"context"

	secretcommands "the8020/kernel/cbus/commands/secret"
	"the8020/kernel/cbus/core"
	"the8020/kernel/services"
)

func New(serviceSet *services.Services) core.Handler {
	return func(_ context.Context, _ core.Request) (core.Result, error) {
		service, err := secretcommands.Service(serviceSet)
		if err != nil {
			return nil, err
		}
		items, err := service.List()
		if err != nil {
			return nil, err
		}
		return core.Result{"secrets": items}, nil
	}
}
