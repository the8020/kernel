// Package packagecommands contains shared, package-domain command behavior.
package packagecommands

import (
	"context"
	"errors"

	"the8020/kernel/cbus/core"
	workspacepackages "the8020/kernel/packages"
	"the8020/kernel/services"
)

func Management(serviceSet *services.Services) (services.PackageManagementService, error) {
	if serviceSet == nil || serviceSet.PackageManagement == nil {
		return nil, core.NewError(core.CodeRuntimeUnavailable, "package management is unavailable")
	}
	return serviceSet.PackageManagement, nil
}

// Synchronize updates package worktrees and then refreshes only services owned
// by changed packages. Offline deployment dispatch has no runtime service and
// therefore performs only the package transaction.
func Synchronize(ctx context.Context, serviceSet *services.Services, packageIDs []string) ([]workspacepackages.PackageSynchronization, error) {
	management, err := Management(serviceSet)
	if err != nil {
		return nil, err
	}
	results, err := management.SynchronizePackages(ctx, packageIDs)
	if err != nil {
		return nil, err
	}
	runtimeServices := serviceSet.RuntimeSnapshot()
	for index := range results {
		result := &results[index]
		if !result.Success || !result.Changed || runtimeServices == nil || runtimeServices.Failure != "" || runtimeServices.Services == nil {
			continue
		}
		current := make(map[string]bool, len(result.Services))
		for _, serviceID := range result.Services {
			current[serviceID] = true
		}
		var lifecycleErrors []error
		for _, serviceID := range result.PreviousServices {
			if current[serviceID] {
				continue
			}
			if retireErr := runtimeServices.Services.Retire(ctx, serviceID); retireErr != nil {
				lifecycleErrors = append(lifecycleErrors, retireErr)
				continue
			}
			result.RetiredServices = append(result.RetiredServices, serviceID)
		}
		for _, serviceID := range result.Services {
			if _, reloadErr := runtimeServices.Services.Reload(ctx, serviceID); reloadErr != nil {
				lifecycleErrors = append(lifecycleErrors, reloadErr)
				continue
			}
			result.RestartedServices = append(result.RestartedServices, serviceID)
		}
		if joined := errors.Join(lifecycleErrors...); joined != nil {
			result.Success = false
			result.Error = "package synchronized but service refresh failed: " + joined.Error()
		}
	}
	failed := false
	for _, result := range results {
		failed = failed || !result.Success
	}
	if failed {
		commandError := core.NewError(core.CodeRuntimeOperation, "one or more packages failed to synchronize")
		commandError.Details = map[string]any{"packages": results}
		return results, commandError
	}
	return results, nil
}
