// Package syncall implements database.table.sync_all.
package syncall

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
		results, err := service.SynchronizeDefinitions(ctx, nil, true)
		if err != nil {
			return core.Result{"tables": results}, core.NewError(core.CodeDatabaseOperation, err.Error())
		}
		return core.Result{"tables": results}, nil
	}
}
