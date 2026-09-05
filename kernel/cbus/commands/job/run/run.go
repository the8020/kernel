// Package run implements job.run.
package run

import (
	"context"
	"the8020/kernel/cbus/commands/internal/commandutil"
	"the8020/kernel/cbus/core"
	"the8020/kernel/execution"
	"the8020/kernel/execution/jobs"
	"the8020/kernel/services"
)

func New(serviceSet *services.Services) core.Handler {
	return func(ctx context.Context, request core.Request) (core.Result, error) {
		runtimeServices, err := commandutil.Runtime(serviceSet)
		if err != nil {
			return nil, err
		}
		if runtimeServices.Jobs == nil {
			return nil, core.NewError(core.CodeRuntimeUnavailable, "job manager is unavailable")
		}
		input, err := commandutil.JSON(request, "input")
		if err != nil {
			return nil, err
		}
		var reuse *bool
		var user execution.User
		if commandutil.Has(request, "user") {
			user, err = execution.UserForUsername(commandutil.String(request, "user"))
			if err != nil {
				return nil, core.NewError(core.CodeInvalidArguments, err.Error())
			}
		}
		if commandutil.Has(request, "reuse") {
			value := commandutil.Bool(request, "reuse")
			reuse = &value
		}
		arguments := []any(nil)
		if input != nil {
			arguments = []any{input}
		}
		record, err := runtimeServices.Jobs.Run(ctx, commandutil.String(request, "job_id"), commandutil.String(request, "entrypoint"), jobs.Options{
			User:      user,
			Arguments: arguments, Detached: commandutil.Bool(request, "detached"), GroupKey: commandutil.String(request, "group_key"),
			Namespace: commandutil.String(request, "namespace"), Timeout: commandutil.Duration(request, "timeout"),
			Parallelism: commandutil.Int(request, "parallelism"), Reuse: reuse, Permissions: commandutil.Permissions(request),
			Workspace: commandutil.String(request, "workspace"), WorkspaceWritable: commandutil.Bool(request, "workspace_write"),
		})
		if err != nil {
			return nil, commandutil.OperationError(err)
		}
		return core.Result{"execution": record}, nil
	}
}
