// Package definitions implements database.table.definitions.
package definitions

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
		definitions, err := service.ListDefinitions(ctx)
		if err != nil {
			return nil, core.NewError(core.CodeDatabaseOperation, err.Error())
		}
		return core.Result{"definitions": definitions}, nil
	}
}
