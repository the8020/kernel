// Package packagecommands contains shared, package-domain command behavior.
package packagecommands

import (
	"context"
	"errors"

	"the8020/kernel/cbus/core"
	workspacepackages "the8020/kernel/packages"
	"the8020/kernel/services"
)

// RefreshRepositoryMutation reloads only services affected by a changed
// checkout. Offline package administration performs only the Git transaction.
func RefreshRepositoryMutation(ctx context.Context, serviceSet *services.Services, mutation workspacepackages.RepositoryMutation) error {
	if !mutation.Changed {
		return nil
	}
	runtimeServices := serviceSet.RuntimeSnapshot()
	if runtimeServices == nil || runtimeServices.Failure != "" || runtimeServices.Services == nil {
		return nil
	}
	current := make(map[string]bool, len(mutation.Services))
	for _, serviceID := range mutation.Services {
		current[serviceID] = true
	}
	var lifecycleErrors []error
	for _, serviceID := range mutation.PreviousServices {
		if current[serviceID] {
			continue
		}
		if err := runtimeServices.Services.Retire(ctx, serviceID); err != nil {
			lifecycleErrors = append(lifecycleErrors, err)
		}
	}
	for _, serviceID := range mutation.Services {
		if _, err := runtimeServices.Services.Reload(ctx, serviceID); err != nil {
			lifecycleErrors = append(lifecycleErrors, err)
		}
	}
	if err := errors.Join(lifecycleErrors...); err != nil {
		return core.NewError(core.CodeRuntimeOperation, "repository changed but service refresh failed: "+err.Error())
	}
	return nil
}

// SynchronizationResult is the complete public result for one synchronized
// package. Detailed Git and service-refresh state remains internal.
type SynchronizationResult struct {
	PackageID string `json:"package_id"`
	Commit    string `json:"commit"`
	Success   bool   `json:"success"`
}

func Management(serviceSet *services.Services) (services.PackageManagementService, error) {
	if serviceSet == nil || serviceSet.PackageManagement == nil {
		return nil, core.NewError(core.CodeRuntimeUnavailable, "package management is unavailable")
	}
	return serviceSet.PackageManagement, nil
}

// Synchronize updates package worktrees and then refreshes only services owned
// by changed packages. Offline deployment dispatch has no runtime service and
// therefore performs only the package transaction.
func Synchronize(ctx context.Context, serviceSet *services.Services, packageIDs []string) ([]SynchronizationResult, error) {
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
	summaries := make([]SynchronizationResult, 0, len(results))
	firstFailure := -1
	for index, result := range results {
		summaries = append(summaries, SynchronizationResult{
			PackageID: result.PackageID,
			Commit:    result.Commit,
			Success:   result.Success,
		})
		if !result.Success && firstFailure < 0 {
			firstFailure = index
		}
	}
	if firstFailure >= 0 {
		failed := results[firstFailure]
		message := "one or more packages failed to synchronize"
		if failed.Error != "" {
			message += ": " + failed.PackageID + ": " + failed.Error
		}
		commandError := core.NewError(core.CodeRuntimeOperation, message)
		commandError.Details = map[string]any{"packages": summaries}
		return summaries, commandError
	}
	return summaries, nil
}
