// Package revoke implements auth.session.revoke.
package revoke

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
		if err := service.RevokeSession(commandutil.String(request, "session_id")); err != nil {
			return nil, commandutil.Error(err)
		}
		return core.Result{"revoked": true}, nil
	}
}
