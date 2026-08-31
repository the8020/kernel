// Package revokeuser implements auth.session.revoke_user.
package revokeuser

import (
	"context"

	commandutil "the8020/kernel/cbus/commands/auth/internal"
	"the8020/kernel/cbus/core"
	"the8020/kernel/services"
)

func New(serviceSet *services.Services) core.Handler {
	return func(_ context.Context, request core.Request) (core.Result, error) {
		service, err := commandutil.Service(serviceSet)
		if err != nil {
			return nil, err
		}
		count, err := service.RevokeUserSessions(commandutil.String(request, "username"))
		if err != nil {
			return nil, commandutil.Error(err)
		}
		return core.Result{"revoked_count": count}, nil
	}
}
