// Package trim implements database.table.trim.
package trim

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
		tableID := commandutil.String(request, "table_id")
		columns := commandutil.CSV(request, "columns")
		dropTable := commandutil.Bool(request, "drop_table")
		if !commandutil.Bool(request, "confirm") {
			return nil, core.NewError(core.CodeInvalidArguments, "--confirm is required because trimming permanently deletes data")
		}
		if !dropTable && len(columns) == 0 {
			return nil, core.NewError(core.CodeInvalidArguments, "select retired columns or --drop-table")
		}
		if err := service.Trim(ctx, tableID, columns, dropTable); err != nil {
			return nil, core.NewError(core.CodeDatabaseOperation, err.Error())
		}
		return core.Result{"table_id": tableID, "dropped_table": dropTable, "dropped_columns": columns}, nil
	}
}
