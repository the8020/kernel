// Package compare implements database.table.compare.
package compare

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
		detail, err := service.CompareTable(ctx, commandutil.String(request, "table_id"))
		if err != nil {
			return nil, commandutil.OperationError(err)
		}
		return core.Result{"table": detail}, nil
	}
}
