// Package list implements auth.bootstrap_admin.list.
package list

import (
	"context"

	commandutil "the8020/kernel/cbus/commands/auth/internal"
	"the8020/kernel/cbus/core"
	"the8020/kernel/services"
)

func New(serviceSet *services.Services) core.Handler {
	return func(_ context.Context, _ core.Request) (core.Result, error) {
		service, err := commandutil.Service(serviceSet)
		if err != nil {
			return nil, err
		}
		users, err := service.ListUsers()
		if err != nil {
			return nil, commandutil.Error(err)
		}
		return core.Result{"users": users}, nil
	}
}
