// Package remove implements auth.bootstrap_admin.remove.
package remove

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
		if err := service.RemoveUser(ctx, commandutil.String(request, "username")); err != nil {
			return nil, commandutil.Error(err)
		}
		return core.Result{"removed": true}, nil
	}
}
