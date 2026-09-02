// Package sync implements database.table.sync.
package sync

import (
	"context"

	databasecommands "the8020/kernel/cbus/commands/database"
	"the8020/kernel/cbus/commands/internal/commandutil"
	"the8020/kernel/cbus/core"
	"the8020/kernel/services"
)

func New(serviceSet *services.Services) core.Handler {
	return func(ctx context.Context, request core.Request) (core.Result, error) {
		service, err := databasecommands.Service(serviceSet)
		if err != nil {
			return nil, err
		}
		result, err := service.SynchronizeDefinition(ctx, commandutil.String(request, "table_id"), commandutil.String(request, "source_package"))
		if err != nil {
			return nil, core.NewError(core.CodeDatabaseOperation, err.Error())
		}
		return core.Result{"table": result}, nil
	}
}
