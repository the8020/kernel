// Package run implements runtime.run.
package run

import (
	"context"

	"the8020/kernel/cbus/commands/internal/commandutil"
	"the8020/kernel/cbus/core"
	"the8020/kernel/execution/adminrun"
	"the8020/kernel/sandbox/model"
	"the8020/kernel/services"
)

func New(serviceSet *services.Services) core.Handler {
	return func(ctx context.Context, request core.Request) (core.Result, error) {
		runtimeServices, err := commandutil.Runtime(serviceSet)
		if err != nil {
			return nil, err
		}
		if runtimeServices.AdminRun == nil {
			return nil, core.NewError(core.CodeRuntimeUnavailable, "administrative runtime execution is unavailable")
		}
		input, err := commandutil.JSON(request, "input")
		if err != nil {
			return nil, err
		}
		result, err := runtimeServices.AdminRun.Run(ctx, commandutil.String(request, "path"), adminrun.Options{
			WorkloadType: model.WorkloadType(commandutil.String(request, "workload_type")),
			OwnerID:      commandutil.String(request, "owner_id"), GroupKey: commandutil.String(request, "group_key"),
			Namespace: commandutil.String(request, "namespace"), Timeout: commandutil.Duration(request, "timeout"),
			Detached: commandutil.Bool(request, "detached"), Input: input, Permissions: commandutil.Permissions(request),
			Workspace: commandutil.String(request, "workspace"), WorkspaceWritable: commandutil.Bool(request, "workspace_write"),
		})
		if err != nil {
			return nil, commandutil.OperationError(err)
		}
		return commandutil.AdministrativeExecution(result, commandutil.Bool(request, "detail")), nil
	}
}
