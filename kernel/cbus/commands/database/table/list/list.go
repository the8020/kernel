// Package list implements database.table.list.
package list

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
		tables, err := service.ListTables(ctx)
		if err != nil {
			return nil, core.NewError(core.CodeDatabaseOperation, err.Error())
		}
		return core.Result{"tables": tables}, nil
	}
}
