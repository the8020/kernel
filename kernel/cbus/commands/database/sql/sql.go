// Package sql implements database.sql.
package sql

import (
	"context"

	databasecommands "the8020/kernel/cbus/commands/database"
	"the8020/kernel/cbus/commands/internal/commandutil"
	"the8020/kernel/cbus/core"
	"the8020/kernel/database"
	"the8020/kernel/services"
)

func New(serviceSet *services.Services) core.Handler {
	return func(ctx context.Context, request core.Request) (core.Result, error) {
		service, err := databasecommands.Service(serviceSet)
		if err != nil {
			return nil, err
		}
		parameters, err := database.DecodeParameters([]byte(commandutil.String(request, "parameters")))
		if err != nil {
			return nil, core.NewError(core.CodeInvalidArguments, err.Error())
		}
		statement := commandutil.String(request, "statement")
		if commandutil.Bool(request, "execute") {
			result, err := service.Execute(ctx, statement, parameters)
			if err != nil {
				return nil, core.NewError(core.CodeDatabaseOperation, err.Error())
			}
			return core.Result{"rows_affected": result.RowsAffected}, nil
		}
		result, err := service.Query(ctx, statement, parameters)
		if err != nil {
			return nil, core.NewError(core.CodeDatabaseOperation, err.Error())
		}
		return core.Result{"columns": result.Columns, "rows": result.Rows, "truncated": result.Truncated}, nil
	}
}
