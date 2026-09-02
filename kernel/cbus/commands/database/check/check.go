// Package check implements database.check.
package check

import (
	"context"

	databasecommands "the8020/kernel/cbus/commands/database"
	"the8020/kernel/cbus/core"
	"the8020/kernel/services"
)

func New(serviceSet *services.Services) core.Handler {
	return func(ctx context.Context, _ core.Request) (core.Result, error) {
		service, err := databasecommands.Service(serviceSet)
		if err != nil {
			return nil, err
		}
		status, err := service.Check(ctx)
		if err != nil {
			failure := core.NewError(core.CodeDatabaseUnavailable, "system database connection failed: "+err.Error())
			failure.Details = map[string]any{"backend": status.Backend, "location": status.Location, "status": status.State}
			return nil, failure
		}
		return core.Result{"backend": status.Backend, "location": status.Location, "status": status.State}, nil
	}
}
