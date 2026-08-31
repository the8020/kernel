// Package setpassword implements auth.bootstrap_admin.set_password.
package setpassword

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
		user, err := service.SetPassword(ctx, commandutil.String(request, "username"), commandutil.String(request, "password"))
		if err != nil {
			return nil, commandutil.Error(err)
		}
		return core.Result{"user": user}, nil
	}
}
