// Package sql implements database.sql.
package sql

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	databasecommands "the8020/kernel/cbus/commands/database"
	"the8020/kernel/cbus/commands/internal/commandutil"
	"the8020/kernel/cbus/core"
	"the8020/kernel/database"
	"the8020/kernel/services"
)

// NewHTTP adapts the same SQL handler for authenticated development ingress.
func NewHTTP(serviceSet *services.Services) http.Handler {
	handler := New(serviceSet)
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Cache-Control", "no-store")
		var input struct {
			Statement  string `json:"statement"`
			Parameters string `json:"parameters"`
			Execute    bool   `json:"execute"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 1<<20))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&input) != nil || decoder.Decode(&struct{}{}) != io.EOF {
			http.Error(writer, "invalid SQL request", http.StatusBadRequest)
			return
		}
		ctx, cancel := context.WithTimeout(request.Context(), 30*time.Second)
		defer cancel()
		result, err := handler(ctx, core.Request{Arguments: map[string]any{
			"statement": input.Statement, "parameters": input.Parameters, "execute": input.Execute,
		}})
		if err != nil {
			status := http.StatusInternalServerError
			var commandError *core.Error
			if errors.As(err, &commandError) && commandError.Code == core.CodeInvalidArguments {
				status = http.StatusBadRequest
			}
			http.Error(writer, err.Error(), status)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(result)
	})
}

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
