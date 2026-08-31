// Package add implements auth.bootstrap_admin.add.
package add

import (
	"context"

	commandutil "the8020/kernel/cbus/commands/auth/internal"
	"the8020/kernel/cbus/core"
	"the8020/kernel/services"
)

func New(serviceSet *services.Services) core.Handler {
	return func(ctx context.Context, request core.Request) (core.Result, error) {
		service, err := commandutil.Service(serviceSet)
		if err != nil {
			return nil, err
		}
		user, err := service.AddUser(ctx, commandutil.String(request, "username"), commandutil.String(request, "password"))
		if err != nil {
			return nil, commandutil.Error(err)
		}
		return core.Result{"user": user}, nil
	}
}
