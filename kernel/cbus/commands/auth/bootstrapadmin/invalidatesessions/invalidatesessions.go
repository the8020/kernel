// Package invalidatesessions implements auth.bootstrap_admin.invalidate_sessions.
package invalidatesessions

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
		user, err := service.InvalidateUserSessions(ctx, commandutil.String(request, "username"))
		if err != nil {
			return nil, commandutil.Error(err)
		}
		return core.Result{"user": user}, nil
	}
}
